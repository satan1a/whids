package hids

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xrawsec/golang-utils/fsutil"
	"github.com/0xrawsec/golang-utils/log"
	"github.com/0xrawsec/whids/api"
	"github.com/0xrawsec/whids/utils"
	"github.com/pelletier/go-toml"
)

// DumpConfig structure definition
type DumpConfig struct {
	Mode          string `toml:"mode" comment:"Dump mode (choices: file, registry, memory)\n Modes can be combined together, separated by |"`
	Dir           string `toml:"dir" comment:"Directory used to store dumps"`
	Treshold      int    `toml:"treshold" comment:"Dumps only when event criticality is above this threshold"`
	MaxDumps      int    `toml:"max-dumps" comment:"Maximum number of dumps per process"` // maximum number of dump per GUID
	Compression   bool   `toml:"compression" comment:"Enable dumps compression"`
	DumpUntracked bool   `toml:"dump-untracked" comment:"Dumps untracked process. Untracked processes are missing\n enrichment information and may generate unwanted dumps"` // whether or not we should dump untracked processes, if true it would create many FPs
}

// IsModeEnabled checks if dump mode is enabled
func (d *DumpConfig) IsModeEnabled(mode string) bool {
	if strings.Contains(d.Mode, "all") {
		return true
	}
	return strings.Contains(d.Mode, mode)
}

// SysmonConfig holds Sysmon related configuration
type SysmonConfig struct {
	Bin              string `toml:"bin" comment:"Path to Sysmon binary"`
	ArchiveDirectory string `toml:"archive-directory" comment:"Path to Sysmon Archive directory"`
	CleanArchived    bool   `toml:"clean-archived" comment:"Delete files older than 5min archived by Sysmon"`
}

// RulesConfig holds rules configuration
type RulesConfig struct {
	RulesDB        string        `toml:"rules-db" comment:"Path to Gene rules database"`
	ContainersDB   string        `toml:"containers-db" comment:"Path to Gene rules containers\n (c.f. Gene documentation)"`
	UpdateInterval time.Duration `toml:"update-interval" comment:"Update interval at which rules should be pulled from manager\n NB: only applies if a manager server is configured"`
}

// AuditConfig holds Windows audit configuration
type AuditConfig struct {
	Enable        bool     `toml:"enable" comment:"Enable following Audit Policies or not"`
	AuditPolicies []string `toml:"audit-policies" comment:"Audit Policies to enable (c.f. auditpol /get /category:* /r)"`
	AuditDirs     []string `toml:"audit-dirs" comment:"Set Audit ACL to directories, sub-directories and files to generate File System audit events\n https://docs.microsoft.com/en-us/windows/security/threat-protection/auditing/audit-file-system)"`
}

// Configure configures the desired audit policies
func (c *AuditConfig) Configure() {

	if c.Enable {
		for _, ap := range c.AuditPolicies {
			if err := utils.EnableAuditPolicy(ap); err != nil {
				log.Errorf("Failed to enable audit policy %s: %s", ap, err)
			} else {
				log.Infof("Enabled Audit Policy: %s", ap)
			}
		}
	}

	// run this function async as it might take a little bit of time
	go func() {
		dirs := utils.StdDirs(utils.ExpandEnvs(c.AuditDirs...)...)
		if len(dirs) > 0 {
			log.Infof("Setting ACLs for directories: %s", strings.Join(dirs, ", "))
			if err := utils.SetEDRAuditACL(dirs...); err != nil {
				log.Errorf("Error while setting configured File System Audit ACLs: %s", err)
			}
			log.Infof("Finished setting up ACLs for directories: %s", strings.Join(dirs, ", "))
		}
	}()
}

// Restore the audit policies
func (c *AuditConfig) Restore() {
	for _, ap := range c.AuditPolicies {
		if err := utils.DisableAuditPolicy(ap); err != nil {
			log.Errorf("Failed to disable audit policy %s: %s", ap, err)
		}
	}

	dirs := utils.StdDirs(utils.ExpandEnvs(c.AuditDirs...)...)
	if err := utils.RemoveEDRAuditACL(dirs...); err != nil {
		log.Errorf("Error while restoring File System Audit ACLs: %s", err)
	}
}

// Config structure
type Config struct {
	Channels        []string             `toml:"channels" comment:"Windows log channels to listen to. Either channel names\n can be used (i.e. Microsoft-Windows-Sysmon/Operational) or aliases"`
	CritTresh       int                  `toml:"criticality-treshold" comment:"Dumps/forward only events above criticality threshold\n or filtered events (i.e. Gene filtering rules)"`
	EnableHooks     bool                 `toml:"en-hooks" comment:"Enable enrichment hooks and dump hooks"`
	EnableFiltering bool                 `toml:"en-filters" comment:"Enable event filtering (log filtered events, not only alerts)\n See documentation: https://github.com/0xrawsec/gene"`
	Logfile         string               `toml:"logfile" comment:"Logfile used to log messages generated by the engine"` // for WHIDS log messages (not alerts)
	LogAll          bool                 `toml:"log-all" comment:"Log any incoming event passing through the engine"`    // log all events to logfile (used for debugging)
	Endpoint        bool                 `toml:"endpoint" comment:"True if current host is the endpoint on which logs are generated\n Example: turn this off if running on a WEC"`
	FwdConfig       *api.ForwarderConfig `toml:"forwarder" comment:"Forwarder configuration"`
	Sysmon          *SysmonConfig        `toml:"sysmon" comment:"Sysmon related settings"`
	Dump            *DumpConfig          `toml:"dump" comment:"Dump related settings"`
	Report          *ReportConfig        `toml:"reporting" comment:"Reporting related settings"`
	RulesConfig     *RulesConfig         `toml:"rules" comment:"Gene rules related settings\n Gene repo: https://github.com/0xrawsec/gene\n Gene rules repo: https://github.com/0xrawsec/gene-rules"`
	AuditConfig     *AuditConfig         `toml:"audit" comment:"Windows auditing configuration"`
	CanariesConfig  *CanariesConfig      `toml:"canaries" comment:"Canary files configuration"`
}

// LoadsHIDSConfig loads a HIDS configuration from a file
func LoadsHIDSConfig(path string) (c Config, err error) {
	fd, err := os.Open(path)
	if err != nil {
		return
	}
	defer fd.Close()
	dec := toml.NewDecoder(fd)
	err = dec.Decode(&c)
	return
}

// IsDumpEnabled returns true if any kind of dump is enabled
func (c *Config) IsDumpEnabled() bool {
	// Dump can be enabled only in endpoint mode
	return c.Endpoint && (c.Dump.IsModeEnabled("file") || c.Dump.IsModeEnabled("registry") || c.Dump.IsModeEnabled("memory"))
}

// IsForwardingEnabled returns true if a forwarder is actually configured to forward logs
func (c *Config) IsForwardingEnabled() bool {
	return *c.FwdConfig != emptyForwarderConfig && !c.FwdConfig.Local
}

// Prepare creates directory used in the config if not existing
func (c *Config) Prepare() {
	if !fsutil.Exists(c.RulesConfig.RulesDB) {
		os.MkdirAll(c.RulesConfig.RulesDB, 0600)
	}
	if !fsutil.Exists(c.RulesConfig.ContainersDB) {
		os.MkdirAll(c.RulesConfig.ContainersDB, 0600)
	}
	if !fsutil.Exists(c.Dump.Dir) {
		os.MkdirAll(c.Dump.Dir, 0600)
	}
	if !fsutil.Exists(filepath.Dir(c.FwdConfig.Logging.Dir)) {
		os.MkdirAll(filepath.Dir(c.FwdConfig.Logging.Dir), 0600)
	}
	if !fsutil.Exists(filepath.Dir(c.Logfile)) {
		os.MkdirAll(filepath.Dir(c.Logfile), 0600)
	}
}

// Verify validate HIDS configuration object
func (c *Config) Verify() error {
	if !fsutil.IsDir(c.RulesConfig.RulesDB) {
		return fmt.Errorf("rules database must be a directory")
	}
	if !fsutil.IsDir(c.RulesConfig.ContainersDB) {
		return fmt.Errorf("containers database must be a directory")
	}
	return nil
}
