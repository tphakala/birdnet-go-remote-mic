//go:build linux

// Package service installs, removes, and reports a systemd unit that runs the
// remote-mic appliance at boot as a dedicated least-privilege system user. It
// targets deb-family Linux (Ubuntu, Debian, Raspberry Pi OS) today; the
// InitSystem and Platform seams keep rpm-family support a small later addition.
package service

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Defaults for a system-wide install. The binary is copied to a stable path so
// the unit's ExecStart never points at a download location that may move; the
// config dir is owned by the service user so the run lock it writes beside the
// config is its own (a root-owned lock makes the appliance run unlocked).
const (
	DefaultUser       = "remote-mic"
	DefaultBinPath    = "/usr/local/bin/remote-mic"
	DefaultConfigPath = "/etc/remote-mic/config.yaml"
	DefaultStateDir   = "/var/lib/remote-mic"
	DefaultUnitName   = "remote-mic.service"
)

// unitDir is where the generated unit is written. Kept unexported: an operator
// never relocates it, and hardcoding it keeps uninstall's remove target exact.
const unitDir = "/etc/systemd/system"

// ServiceSpec is the resolved description of an install: who the service runs
// as and where its binary, config, and state live. Zero fields are filled from
// the Default* constants by withDefaults, so a caller can pass only the fields
// it overrides.
type ServiceSpec struct {
	User       string // system user to create and run as
	Group      string // system group; defaults to User
	BinPath    string // absolute path the binary is copied to and ExecStart runs
	ConfigPath string // absolute config path baked into the unit via REMOTEMIC_CONFIG
	StateDir   string // absolute cert-store directory, owned by the service user
	UnitName   string // unit file name, e.g. remote-mic.service
}

// withDefaults returns a copy of s with empty fields filled from the Default*
// constants. Group mirrors User when unset, matching the user-plus-group a
// system account gets.
func (s ServiceSpec) withDefaults() ServiceSpec {
	if s.User == "" {
		s.User = DefaultUser
	}
	if s.Group == "" {
		s.Group = s.User
	}
	if s.BinPath == "" {
		s.BinPath = DefaultBinPath
	}
	if s.ConfigPath == "" {
		s.ConfigPath = DefaultConfigPath
	}
	if s.StateDir == "" {
		s.StateDir = DefaultStateDir
	}
	if s.UnitName == "" {
		s.UnitName = DefaultUnitName
	}
	return s
}

// ConfigDir is the directory holding the config file; the installer creates and
// chowns it to the service user so the appliance and its run lock can write it.
func (s ServiceSpec) ConfigDir() string { return filepath.Dir(s.ConfigPath) }

// UnitPath is the absolute path the unit file is written to.
func (s ServiceSpec) UnitPath() string { return filepath.Join(unitDir, s.UnitName) }

// Validate rejects a spec that would render an unusable unit: a non-absolute
// path (systemd requires absolute ExecStart and directory paths), an empty or
// whitespace-bearing user name, or a unit name that is not a bare
// "<name>.service" file. It assumes defaults have already been applied.
func (s ServiceSpec) Validate() error {
	if strings.TrimSpace(s.User) == "" || strings.ContainsAny(s.User, " \t\n") {
		return fmt.Errorf("service: invalid user name %q", s.User)
	}
	if strings.TrimSpace(s.Group) == "" || strings.ContainsAny(s.Group, " \t\n") {
		return fmt.Errorf("service: invalid group name %q", s.Group)
	}
	for label, p := range map[string]string{
		"bin path":    s.BinPath,
		"config path": s.ConfigPath,
		"state dir":   s.StateDir,
	} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("service: %s must be absolute, got %q", label, p)
		}
	}
	if s.UnitName == "" || strings.ContainsAny(s.UnitName, "/ \t\n") || !strings.HasSuffix(s.UnitName, ".service") {
		return fmt.Errorf("service: invalid unit name %q (want <name>.service)", s.UnitName)
	}
	return nil
}
