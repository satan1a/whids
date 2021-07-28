package hids

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xrawsec/golang-win32/win32"
	"github.com/0xrawsec/golang-win32/win32/kernel32"

	"github.com/0xrawsec/gene/engine"
	"github.com/0xrawsec/golang-evtx/evtx"
	"github.com/0xrawsec/golang-utils/crypto/data"
	"github.com/0xrawsec/golang-utils/datastructs"
	"github.com/0xrawsec/golang-utils/fsutil"
	"github.com/0xrawsec/golang-utils/fsutil/fswalker"
	"github.com/0xrawsec/golang-utils/log"
	"github.com/0xrawsec/golang-utils/sync/semaphore"
	"github.com/0xrawsec/golang-win32/win32/wevtapi"
	"github.com/0xrawsec/whids/api"
	"github.com/0xrawsec/whids/utils"
)

// XMLEventToGoEvtxMap converts an XMLEvent as returned by wevtapi to a GoEvtxMap
// object that Gene can use
// TODO: Improve for more perf
func XMLEventToGoEvtxMap(xe *wevtapi.XMLEvent) (*evtx.GoEvtxMap, error) {
	ge := make(evtx.GoEvtxMap)
	bytes, err := json.Marshal(xe.ToJSONEvent())
	if err != nil {
		return &ge, err
	}
	err = json.Unmarshal(bytes, &ge)
	if err != nil {
		return &ge, err
	}
	return &ge, nil
}

const (
	/** Private const **/

	// Container extension
	containerExt = ".cont.gz"
)

var (
	/** Public vars **/

	DumpOptions = []string{"registry", "memory", "file", "all"}

	ChannelAliases = map[string]string{
		"sysmon":   "Microsoft-Windows-Sysmon/Operational",
		"security": "Security",
		"ps":       "Microsoft-Windows-PowerShell/Operational",
		"defender": "Microsoft-Windows-Windows Defender/Operational",
		"all":      "All aliased channels",
	}

	ContainRuleName = "EDR containment"

	/** Private vars **/

	emptyForwarderConfig = api.ForwarderConfig{}

	// extensions of files to upload to manager
	uploadExts = datastructs.NewInitSyncedSet(".gz", ".sha256")

	archivedRe = regexp.MustCompile(`(CLIP-)??[0-9A-F]{32,}(\..*)?`)
)

func allChannels() []string {
	channels := make([]string, 0, len(ChannelAliases))
	for alias, channel := range ChannelAliases {
		if alias != "all" {
			channels = append(channels, channel)
		}
	}
	return channels
}

// HIDS structure
type HIDS struct {
	sync.RWMutex    // Mutex to lock the IDS when updating rules
	eventProvider   wevtapi.EventProvider
	preHooks        *HookManager
	postHooks       *HookManager
	forwarder       *api.Forwarder
	channels        *datastructs.SyncedSet // Windows log channels to listen to
	channelsSignals chan bool
	config          *Config
	eventScanned    uint64
	alertReported   uint64
	startTime       time.Time
	waitGroup       sync.WaitGroup

	flagProcTermEn bool
	bootCompleted  bool
	// Sysmon GUID of HIDS process
	guid           string
	processTracker *ActivityTracker
	memdumped      *datastructs.SyncedSet
	dumping        *datastructs.SyncedSet
	filedumped     *datastructs.SyncedSet
	hookSemaphore  semaphore.Semaphore

	// Compression management
	compressionIsRunning bool
	compressionChannel   chan string

	Engine   engine.Engine
	DryRun   bool
	PrintAll bool
}

func newActionnableEngine() (e engine.Engine) {
	e = engine.NewEngine(false)
	e.ShowActions = true
	return
}

// NewHIDS creates a new HIDS object from configuration
func NewHIDS(c *Config) (h *HIDS, err error) {
	h = &HIDS{
		// PushEventProvider seems not to retrieve all the events (observed this at boot)
		eventProvider:      wevtapi.NewPullEventProvider(),
		preHooks:           NewHookMan(),
		postHooks:          NewHookMan(),
		channels:           datastructs.NewSyncedSet(),
		channelsSignals:    make(chan bool),
		config:             c,
		waitGroup:          sync.WaitGroup{},
		processTracker:     NewActivityTracker(),
		memdumped:          datastructs.NewSyncedSet(),
		dumping:            datastructs.NewSyncedSet(),
		filedumped:         datastructs.NewSyncedSet(),
		hookSemaphore:      semaphore.New(4),
		compressionChannel: make(chan string),
	}

	// Creates missing directories
	c.Prepare()

	// Create logfile asap if needed
	if c.Logfile != "" {
		log.SetLogfile(c.Logfile, 0600)
	}

	// Verify configuration
	if err = c.Verify(); err != nil {
		return nil, err
	}

	// loading forwarder config
	if h.forwarder, err = api.NewForwarder(c.FwdConfig); err != nil {
		return nil, err
	}

	// cleaning up previous runs
	h.cleanup()

	// initialization
	h.initChannels(c.Channels)
	h.initHooks(c.EnableHooks)
	// initializing canaries
	h.config.CanariesConfig.Configure()
	// fixing local audit policies if necessary
	h.config.AuditConfig.Configure()

	// tries to update the engine
	if err := h.updateEngine(true); err != nil {
		return h, err
	}
	return h, nil
}

/** Private Methods **/

func (h *HIDS) initChannels(channels []string) {
	for _, c := range channels {
		if c == "all" {
			h.channels.Add(datastructs.ToInterfaceSlice(allChannels())...)
			continue
		}
		if rc, ok := ChannelAliases[c]; ok {
			h.channels.Add(rc)
		} else {
			h.channels.Add(c)
		}
	}
}

func (h *HIDS) initHooks(advanced bool) {
	// We enable those hooks anyway since it is needed to skip
	// events generated by WHIDS process. These ar very light hooks
	h.preHooks.Hook(hookSelfGUID, fltImageSize)
	h.preHooks.Hook(hookProcTerm, fltProcTermination)
	h.preHooks.Hook(hookStats, fltStats)
	h.preHooks.Hook(hookTrack, fltTrack)
	if advanced {
		// Process terminator hook, terminating blacklisted (by action) processes
		h.preHooks.Hook(hookTerminator, fltProcessCreate)
		h.preHooks.Hook(hookImageLoad, fltImageLoad)
		h.preHooks.Hook(hookSetImageSize, fltImageSize)
		h.preHooks.Hook(hookProcessIntegrityProcTamp, fltImageTampering)
		h.preHooks.Hook(hookEnrichServices, fltAnySysmon)
		h.preHooks.Hook(hookClipboardEvents, fltClipboard)
		h.preHooks.Hook(hookFileSystemAudit, fltFSObjectAccess)
		// Must be run the last as it depends on other filters
		h.preHooks.Hook(hookEnrichAnySysmon, fltAnySysmon)
		// Not sastifying results with Sysmon 11.11 we should try enabling this on newer versions
		//h.preHooks.Hook(HookDNS, fltDNS)
		//h.preHooks.Hook(hookEnrichDNSSysmon, fltNetworkConnect)
		// Experimental
		//h.preHooks.Hook(hookSetValueSize, fltRegSetValue)

		// Registering post detection hooks
		// if endpoint we enable dump features
		if h.config.Endpoint {
			if h.config.Dump.IsModeEnabled("registry") {
				h.postHooks.Hook(hookDumpRegistry, fltRegSetValue)
			}
			if h.config.Dump.IsModeEnabled("file") {
				h.postHooks.Hook(hookDumpFiles, fltAnySysmon)
			}
			if h.config.Dump.IsModeEnabled("memory") {
				h.postHooks.Hook(hookDumpProcess, fltAnySysmon)
			}
		}

		// This hook must run before action handling as we want
		// the gene score to be set before an eventual reporting
		h.postHooks.Hook(hookUpdateGeneScore, fltAnyEvent)
		// Handles actions defined in rules for any Sysmon event
		h.postHooks.Hook(hookHandleActions, fltAnyEvent)
	}
}

func (h *HIDS) updateEngine(force bool) error {
	h.Lock()
	defer h.Unlock()

	reloadRules := h.needsRulesUpdate()
	reloadContainers := h.needsContainersUpdate()

	// check if we need rule update
	if reloadRules {
		log.Info("Updating WHIDS rules")
		if err := h.fetchRulesFromManager(); err != nil {
			log.Errorf("Failed to fetch rules from manager: %s", err)
			reloadRules = false
		}
	}

	if reloadContainers {
		log.Info("Updating WHIDS containers")
		if err := h.fetchContainersFromManager(); err != nil {
			log.Errorf("Failed to fetch containers from manager: %s", err)
			reloadContainers = false
		}
	}

	log.Debugf("reloading rules:%t containers:%t forced:%t", reloadRules, reloadContainers, force)
	if reloadRules || reloadContainers || force {
		// We need to create a new engine if we received a rule/containers update
		h.Engine = newActionnableEngine()

		// containers must be loaded before the rules anyway
		log.Infof("Loading HIDS containers (used in rules) from: %s", h.config.RulesConfig.ContainersDB)
		if err := h.loadContainers(); err != nil {
			return fmt.Errorf("error loading containers: %s", err)
		}

		if reloadRules || force {
			// Loading canary rules
			if h.config.CanariesConfig.Enable {
				log.Infof("Loading canary rules")
				// Sysmon rule
				sr := h.config.CanariesConfig.GenRuleSysmon()
				if scr, err := sr.Compile(nil); err != nil {
					log.Errorf("Failed to compile canary rule: %s", err)
				} else {
					h.Engine.AddRule(scr)
				}

				// File System Audit Rule
				fsr := h.config.CanariesConfig.GenRuleFSAudit()
				if fscr, err := fsr.Compile(nil); err != nil {
					log.Errorf("Failed to compile canary rule: %s", err)
				} else {
					h.Engine.AddRule(fscr)
				}
			}

			log.Infof("Loading HIDS rules from: %s", h.config.RulesConfig.RulesDB)
			if err := h.Engine.LoadDirectory(h.config.RulesConfig.RulesDB); err != nil {
				return fmt.Errorf("failed to load rules: %s", err)
			}
			log.Infof("Number of rules loaded in engine: %d", h.Engine.Count())
		}
	} else {
		log.Debug("Neither rules nor containers need to be updated")
	}

	return nil
}

// rules needs to be updated with the new ones available in manager
func (h *HIDS) needsRulesUpdate() bool {
	var err error
	var oldSha256, sha256 string
	_, rulesSha256Path := h.RulesPaths()

	if h.forwarder.Local {
		return false
	}

	if sha256, err = h.forwarder.Client.GetRulesSha256(); err != nil {
		return false
	}
	oldSha256, _ = utils.ReadFileString(rulesSha256Path)

	log.Debugf("Rules: remote=%s local=%s", sha256, oldSha256)
	return oldSha256 != sha256
}

// at least one container needs to be updated
func (h *HIDS) needsContainersUpdate() bool {
	var containers []string
	var err error

	cl := h.forwarder.Client

	if h.forwarder.Local {
		return false
	}

	if containers, err = cl.GetContainersList(); err != nil {
		return false
	}

	for _, cont := range containers {
		if h.needsContainerUpdate(cont) {
			return true
		}
	}
	return false
}

// returns true if a container needs to be updated
func (h *HIDS) needsContainerUpdate(remoteCont string) bool {
	var localSha256, remoteSha256 string
	_, locContSha256Path := h.containerPaths(remoteCont)
	// means that remoteCont is also a local container
	remoteSha256, _ = h.forwarder.Client.GetContainerSha256(remoteCont)
	localSha256, _ = utils.ReadFileString(locContSha256Path)
	log.Infof("container %s: remote=%s local=%s", remoteCont, remoteSha256, localSha256)
	return localSha256 != remoteSha256
}

func (h *HIDS) fetchRulesFromManager() (err error) {
	var rules, sha256 string

	rulePath, sha256Path := h.RulesPaths()

	// if we are not connected to a manager we return
	if h.config.FwdConfig.Local {
		return
	}

	log.Infof("Fetching new rules available in manager")
	if sha256, err = h.forwarder.Client.GetRulesSha256(); err != nil {
		return err
	}

	if rules, err = h.forwarder.Client.GetRules(); err != nil {
		return err
	}

	if sha256 != data.Sha256([]byte(rules)) {
		return fmt.Errorf("failed to verify rules integrity")
	}

	ioutil.WriteFile(sha256Path, []byte(sha256), 0600)
	return ioutil.WriteFile(rulePath, []byte(rules), 0600)
}

// containerPaths returns the path to the container and the path to its sha256 file
func (h *HIDS) containerPaths(container string) (path, sha256Path string) {
	path = filepath.Join(h.config.RulesConfig.ContainersDB, fmt.Sprintf("%s%s", container, containerExt))
	sha256Path = fmt.Sprintf("%s.sha256", path)
	return
}

func (h *HIDS) fetchContainersFromManager() (err error) {
	var containers []string
	cl := h.forwarder.Client

	// if we are not connected to a manager we return
	if h.config.FwdConfig.Local {
		return
	}

	if containers, err = cl.GetContainersList(); err != nil {
		return
	}

	for _, contName := range containers {
		// if container needs to be updated
		if h.needsContainerUpdate(contName) {
			cont, err := cl.GetContainer(contName)
			if err != nil {
				return err
			}

			// we compare the integrity of the container received
			compSha256 := utils.Sha256StringArray(cont)
			sha256, _ := cl.GetContainerSha256(contName)
			if compSha256 != sha256 {
				return fmt.Errorf("failed to verify container \"%s\" integrity", contName)
			}

			// we dump the container
			contPath, contSha256Path := h.containerPaths(contName)
			fd, err := os.Create(contPath)
			if err != nil {
				return err
			}
			w := gzip.NewWriter(fd)
			for _, e := range cont {
				w.Write([]byte(fmt.Sprintln(e)))
			}
			w.Flush()
			w.Close()
			fd.Close()
			// Dump current container sha256 to a file
			ioutil.WriteFile(contSha256Path, []byte(compSha256), 0600)
		}
	}
	return nil
}

// loads containers found in container database directory
func (h *HIDS) loadContainers() (lastErr error) {
	for wi := range fswalker.Walk(h.config.RulesConfig.ContainersDB) {
		for _, fi := range wi.Files {
			path := filepath.Join(wi.Dirpath, fi.Name())
			// we take only files with good extension
			if strings.HasSuffix(fi.Name(), containerExt) {
				cont := strings.SplitN(fi.Name(), ".", 2)[0]
				fd, err := os.Open(path)
				if err != nil {
					lastErr = err
					continue
				}
				r, err := gzip.NewReader(fd)
				if err != nil {
					lastErr = err
					// we close file descriptor
					fd.Close()
					continue
				}
				log.Infof("Loading container: %s", cont)
				h.Engine.LoadContainer(cont, r)
				r.Close()
				fd.Close()
			}
		}
	}
	return
}

func (h *HIDS) cleanup() {
	// Cleaning up empty dump directories if needed
	fis, _ := ioutil.ReadDir(h.config.Dump.Dir)
	for _, fi := range fis {
		if fi.IsDir() {
			fp := filepath.Join(h.config.Dump.Dir, fi.Name())
			if utils.CountFiles(fp) == 0 {
				os.RemoveAll(fp)
			}
		}
	}
}

////////////////// Routines

// schedules the different routines to be ran
func (h *HIDS) cronRoutine() {
	now := time.Now()
	// timestamps
	lastUpdateTs := now
	lastUploadTs := now
	lastArchDelTs := now
	lastCmdRunTs := now

	go func() {
		for {
			now = time.Now()
			switch {
			// handle updates
			case now.Sub(lastUpdateTs) >= h.config.RulesConfig.UpdateInterval:
				// put here function to update
				lastUpdateTs = now
			// handle uploads
			case now.Sub(lastUploadTs) >= time.Minute:
				lastUploadTs = now
			// handle sysmon archive cleaning
			case now.Sub(lastArchDelTs) >= time.Minute:
				// put here code to delete archived files
				lastArchDelTs = now
			// handle command to run
			case now.Sub(lastCmdRunTs) >= 5*time.Second:
				// put here code to run commands
				lastCmdRunTs = now
			}

			time.Sleep(1 * time.Second)
		}
	}()
}

func (h *HIDS) cleanArchivedRoutine() bool {
	if h.config.Sysmon.CleanArchived {
		go func() {
			log.Info("Starting routine to cleanup Sysmon archived files")
			archivePath := h.config.Sysmon.ArchiveDirectory

			if archivePath == "" {
				log.Error("Sysmon archive directory not found")
				return
			}

			if fsutil.IsDir(archivePath) {
				// used to mark files for which we already reported errors
				reported := datastructs.NewSyncedSet()
				log.Infof("Starting archive cleanup loop for directory: %s", archivePath)
				for {
					// expiration fixed to five minutes
					expired := time.Now().Add(time.Minute * -5)
					for wi := range fswalker.Walk(archivePath) {
						for _, fi := range wi.Files {
							if archivedRe.MatchString(fi.Name()) {
								path := filepath.Join(wi.Dirpath, fi.Name())
								if fi.ModTime().Before(expired) {
									// we print out error only once
									if err := os.Remove(path); err != nil && !reported.Contains(path) {
										log.Errorf("Failed to remove archived file: %s", err)
										reported.Add(path)
									}
								}
							}
						}
					}
					time.Sleep(time.Minute * 1)
				}
			} else {
				log.Errorf(fmt.Sprintf("No such Sysmon archive directory: %s", archivePath))
			}
		}()
		return true
	}
	return false
}

// returns true if the update routine is started
func (h *HIDS) updateRoutine() bool {
	d := h.config.RulesConfig.UpdateInterval
	if h.config.IsForwardingEnabled() {
		if d > 0 {
			go func() {
				t := time.NewTimer(d)
				for range t.C {
					if err := h.updateEngine(false); err != nil {
						log.Error(err)
					}
					t.Reset(d)
				}
			}()
			return true
		}
	}
	return false
}

func (h *HIDS) uploadRoutine() bool {
	if h.config.IsDumpEnabled() && h.config.IsForwardingEnabled() {
		// force compression in this case
		h.config.Dump.Compression = true
		go func() {
			for {
				// Sending dump files over to the manager
				for wi := range fswalker.Walk(h.config.Dump.Dir) {
					for _, fi := range wi.Files {
						sp := strings.Split(wi.Dirpath, string(os.PathSeparator))
						// upload only file with some extensions
						if uploadExts.Contains(filepath.Ext(fi.Name())) {
							if len(sp) >= 2 {
								fullpath := filepath.Join(wi.Dirpath, fi.Name())
								fu, err := h.forwarder.Client.PrepareFileUpload(fullpath, sp[len(sp)-2], sp[len(sp)-1], fi.Name())
								if err != nil {
									log.Errorf("Failed to prepare dump file to upload: %s", err)
									continue
								}
								if err := h.forwarder.Client.PostDump(fu); err != nil {
									log.Errorf("%s", err)
									continue
								}
								log.Infof("Dump file successfully sent to manager, deleting: %s", fullpath)
								os.Remove(fullpath)
							} else {
								log.Errorf("Unexpected directory layout, cannot send dump to manager")
							}
						}
					}

				}
				time.Sleep(60 * time.Second)
			}
		}()
		return true
	}
	return false
}

func (h *HIDS) containCmd() *exec.Cmd {
	ip := h.forwarder.Client.ManagerIP
	// only allow connection to the manager configured
	return exec.Command("netsh.exe",
		"advfirewall",
		"firewall",
		"add",
		"rule",
		fmt.Sprintf("name=%s", ContainRuleName),
		"dir=out",
		fmt.Sprintf("remoteip=0.0.0.0-%s,%s-255.255.255.255", utils.PrevIP(ip), utils.NextIP(ip)),
		"action=block")
}

func (h *HIDS) uncontainCmd() *exec.Cmd {
	return exec.Command("netsh.exe", "advfirewall",
		"firewall",
		"delete",
		"rule",
		fmt.Sprintf("name=%s", ContainRuleName),
	)
}

func (h *HIDS) handleManagerCommand(cmd *api.Command) {

	// Switch processing the commands
	switch cmd.Name {
	// Aliases
	case "contain":
		cmd.FromExecCmd(h.containCmd())
	case "uncontain":
		cmd.FromExecCmd(h.uncontainCmd())
	case "osquery":
		if fsutil.IsFile(h.config.Report.OSQuery.Bin) {
			cmd.Name = h.config.Report.OSQuery.Bin
			cmd.Args = append([]string{"--json", "-A"}, cmd.Args...)
			cmd.ExpectJSON = true
		} else {
			cmd.Unrunnable()
			cmd.Error = fmt.Sprintf("OSQuery binary file configured does not exist: %s", h.config.Report.OSQuery.Bin)
		}
	// HIDs internal commands
	case "terminate":
		cmd.Unrunnable()
		if len(cmd.Args) > 0 {
			spid := cmd.Args[0]
			if pid, err := strconv.Atoi(spid); err != nil {
				cmd.Error = fmt.Sprintf("failed to parse pid: %s", err)
			} else if err := terminate(pid); err != nil {
				cmd.Error = err.Error()
			}
		}
	case "hash":
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		if len(cmd.Args) > 0 {
			if out, err := cmdHash(cmd.Args[0]); err != nil {
				cmd.Error = err.Error()
			} else {
				cmd.Stdout = out
			}
		}
	case "stat":
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		if len(cmd.Args) > 0 {
			if out, err := cmdStat(cmd.Args[0]); err != nil {
				cmd.Error = err.Error()
			} else {
				cmd.Stdout = out
			}
		}
	case "dir":
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		if len(cmd.Args) > 0 {
			if out, err := cmdDir(cmd.Args[0]); err != nil {
				cmd.Error = err.Error()
			} else {
				cmd.Stdout = out
			}
		}
	case "walk":
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		if len(cmd.Args) > 0 {
			cmd.Stdout = cmdWalk(cmd.Args[0])
		}
	case "find":
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		if len(cmd.Args) == 2 {
			if out, err := cmdFind(cmd.Args[0], cmd.Args[1]); err != nil {
				cmd.Error = err.Error()
			} else {
				cmd.Stdout = out
			}
		}
	case "report":
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		cmd.Stdout = h.Report()
	case "processes":
		h.processTracker.RLock()
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		cmd.Stdout = h.processTracker.PS()
		h.processTracker.RUnlock()
	case "drivers":
		h.processTracker.RLock()
		cmd.Unrunnable()
		cmd.ExpectJSON = true
		cmd.Stdout = h.processTracker.Drivers
		h.processTracker.RUnlock()
	}

	// we finally run the command
	if err := cmd.Run(); err != nil {
		log.Errorf("failed to run command sent by manager \"%s\": %s", cmd.String(), err)
	}
}

// routine which manages command to be executed on the endpoint
// it is made in such a way that we can send burst of commands
func (h *HIDS) commandRunnerRoutine() bool {
	if h.config.IsForwardingEnabled() {
		go func() {

			defaultSleep := time.Second * 5
			sleep := defaultSleep

			burstDur := time.Duration(0)
			tgtBurstDur := time.Second * 30
			burstSleep := time.Millisecond * 500

			for {
				if cmd, err := h.forwarder.Client.FetchCommand(); err != nil && err != api.ErrNothingToDo {
					log.Error(err)
				} else if err == nil {
					// reduce sleeping time if a command was received
					sleep = burstSleep
					burstDur = 0
					log.Infof("Handling command: %s", cmd.String())
					h.handleManagerCommand(cmd)
					if err := h.forwarder.Client.PostCommand(cmd); err != nil {
						log.Error(err)
					}
				}

				// if we reached the targetted burst duration
				if burstDur >= tgtBurstDur {
					sleep = defaultSleep
				}

				if sleep == burstSleep {
					burstDur += sleep
				}

				time.Sleep(sleep)
			}
		}()
		return true
	}
	return false
}

func (h *HIDS) compress(path string) {
	if h.config.Dump.Compression {
		if !h.compressionIsRunning {
			// start compression routine
			go func() {
				h.compressionIsRunning = true
				for path := range compressionChannel {
					log.Infof("Compressing %s", path)
					if err := utils.GzipFileBestSpeed(path); err != nil {
						log.Errorf("Cannot compress %s: %s", path, err)
					}
				}
				h.compressionIsRunning = false
			}()
		}
		compressionChannel <- path
	}
}

/** Public Methods **/

// IsHIDSEvent returns true if the event is generated by IDS activity
func (h *HIDS) IsHIDSEvent(e *evtx.GoEvtxMap) bool {
	if pguid, err := e.GetString(&pathSysmonParentProcessGUID); err == nil {
		if pguid == h.guid {
			return true
		}
	}

	if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
		if guid == h.guid {
			return true
		}
		// search for parent in processTracker
		if pt := h.processTracker.GetByGuid(guid); pt != nil {
			if pt.ParentProcessGUID == h.guid {
				return true
			}
		}
	}
	if sguid, err := e.GetString(&pathSysmonSourceProcessGUID); err == nil {
		if sguid == h.guid {
			return true
		}
		// search for parent in processTracker
		if pt := h.processTracker.GetByGuid(sguid); pt != nil {
			if pt.ParentProcessGUID == h.guid {
				return true
			}
		}
	}
	return false
}

// Report generate a forensic ready report (meant to be dumped)
// this method is blocking as it runs commands and wait after those
func (h *HIDS) Report() (r Report) {
	r.StartTime = time.Now()

	// generate a report for running processes or those terminated still having one child or more
	// do this step first not to polute report with commands to run
	r.Processes = h.processTracker.PS()

	// Drivers loaded
	r.Drivers = h.processTracker.Drivers

	// run all the commands configured to inculde in the report
	r.Commands = h.config.Report.PrepareCommands()
	for i := range r.Commands {
		r.Commands[i].Run()
	}

	r.StopTime = time.Now()
	return
}

// RulesPaths returns the path used by WHIDS to save gene rules
func (h *HIDS) RulesPaths() (path, sha256Path string) {
	path = filepath.Join(h.config.RulesConfig.RulesDB, "database.gen")
	sha256Path = fmt.Sprintf("%s.sha256", path)
	return
}

// Run starts the WHIDS engine and waits channel listening is stopped
func (h *HIDS) Run() {
	// Running all the threads
	// Runs the forwarder
	h.forwarder.Run()

	// Start the update routine
	log.Infof("Update routine running: %t", h.updateRoutine())
	// starting dump forwarding routine
	log.Infof("Dump forwarding routine running: %t", h.uploadRoutine())
	// running the command runner routine
	log.Infof("Command runner routine running: %t", h.commandRunnerRoutine())
	// start the archive cleanup routine (might create a new thread)
	log.Infof("Sysmon archived files cleanup routine running: %t", h.cleanArchivedRoutine())

	channels := make([]string, 0)
	// We prepare the list of channels
	for it := range h.channels.Items() {
		channel := it.(string)
		channels = append(channels, channel)
	}

	// Dry run don't do anything
	if h.DryRun {
		for _, channel := range channels {
			log.Infof("Dry run: would listen on %s", channel)
		}
		return
	}

	h.startTime = time.Now()
	h.waitGroup.Add(1)
	go func() {
		defer h.waitGroup.Done()

		// Trying to raise thread priority
		if err := kernel32.SetCurrentThreadPriority(win32.THREAD_PRIORITY_ABOVE_NORMAL); err != nil {
			log.Errorf("Failed to raise IDS thread priority: %s", err)
		}

		xmlEvents := h.eventProvider.FetchEvents(channels, wevtapi.EvtSubscribeToFutureEvents)
		for xe := range xmlEvents {
			event, err := XMLEventToGoEvtxMap(xe)
			if err != nil {
				log.Errorf("Failed to convert event: %s", err)
				log.Debugf("Error data: %v", xe)
			}

			// Warning message in certain circumstances
			if h.config.EnableHooks && !h.flagProcTermEn && h.eventScanned > 0 && h.eventScanned%1000 == 0 {
				log.Warn("Sysmon process termination events seem to be missing. WHIDS won't work as expected.")
			}

			h.RLock()

			// Runs pre detection hooks
			// putting this before next condition makes the processTracker registering
			// HIDS events and allows detecting ProcessAccess events from HIDS childs
			h.preHooks.RunHooksOn(h, event)

			// We skip if it is one of IDS event
			// we keep process termination event because it is used to control if process termination is enabled
			if h.IsHIDSEvent(event) && !isSysmonProcessTerminate(event) {
				if h.PrintAll {
					fmt.Println(utils.JSON(event))
				}
				goto LoopTail
			}

			// if the event has matched at least one signature or is filtered
			if n, crit, filtered := h.Engine.MatchOrFilter(event); len(n) > 0 || filtered {
				switch {
				case crit >= h.config.CritTresh:
					if !h.PrintAll && !h.config.LogAll {
						h.forwarder.PipeEvent(event)
					}
					// Pipe the event to be sent to the forwarder
					// Run hooks post detection
					h.postHooks.RunHooksOn(h, event)
					h.alertReported++
				case filtered && h.config.EnableFiltering && !h.PrintAll && !h.config.LogAll:
					event.Del(&engine.GeneInfoPath)
					// we pipe filtered event
					h.forwarder.PipeEvent(event)
				}
			}

			// Print everything
			if h.PrintAll {
				fmt.Println(utils.JSON(event))
			}

			// We log all events
			if h.config.LogAll {
				h.forwarder.PipeEvent(event)
			}

			h.eventScanned++

		LoopTail:
			h.RUnlock()
		}
		log.Infof("HIDS main loop terminated")
	}()

	// Run bogus command so that at least one Process Terminate
	// is generated (used to check if process termination events are enabled)
	exec.Command(os.Args[0], "-h").Start()
}

// LogStats logs whids statistics
func (h *HIDS) LogStats() {
	stop := time.Now()
	log.Infof("Time Running: %s", stop.Sub(h.startTime))
	log.Infof("Count Event Scanned: %d", h.eventScanned)
	log.Infof("Average Event Rate: %.2f EPS", float64(h.eventScanned)/(stop.Sub(h.startTime).Seconds()))
	log.Infof("Alerts Reported: %d", h.alertReported)
	log.Infof("Count Rules Used (loaded + generated): %d", h.Engine.Count())
}

// Stop stops the IDS
func (h *HIDS) Stop() {
	log.Infof("Stopping HIDS")
	// gently close forwarder needs to be done before
	// stop listening othewise we corrupt local logfiles
	// because of race condition
	log.Infof("Closing forwarder")
	h.forwarder.Close()
	log.Infof("Closing event provider")
	h.eventProvider.Stop()
	if h.config.CanariesConfig.Enable {
		log.Infof("Cleaning canaries")
		h.config.CanariesConfig.Clean()
	}
	log.Infof("HIDS stopped")
}

// Wait waits the IDS to finish
func (h *HIDS) Wait() {
	h.waitGroup.Wait()
}

// WaitWithTimeout waits the IDS to finish
func (h *HIDS) WaitWithTimeout(timeout time.Duration) {
	t := time.NewTimer(timeout)
	go func() {
		h.waitGroup.Wait()
		t.Stop()
	}()
	<-t.C
}
