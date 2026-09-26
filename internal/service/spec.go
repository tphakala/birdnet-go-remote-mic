//go:build linux

// Package service installs, removes, and reports a systemd unit that runs the
// remote-mic appliance at boot as a dedicated least-privilege system user. It
// targets deb-family Linux (Ubuntu, Debian, Raspberry Pi OS) today; the
// InitSystem and Platform seams keep rpm-family support a small later addition.
package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// userNameRe is the standard shadow-utils NAME_REGEX for a system account. It
// also blocks a name that starts with '-' (which useradd/usermod would reparse
// as an option) and a '%' (a systemd unit specifier), closing an option- and
// specifier-injection shape even though install already requires root.
var userNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// sharedSystemDirs are directories the installer must never own recursively or
// remove. The installer chowns the config and state directories (recursively at
// their root) and --purge deletes them, so pointing --config or --state-dir at
// one of these would re-home or delete system-wide files. The service's config
// and state directories must be dedicated subdirectories, not these.
var sharedSystemDirs = map[string]bool{
	"/": true, "/etc": true, "/etc/systemd": true, "/etc/default": true,
	"/etc/cron.d": true, "/var": true, "/var/lib": true, "/var/log": true,
	"/var/run": true, "/var/cache": true, "/var/tmp": true, "/var/spool": true,
	"/var/mail": true, "/var/lock": true, "/run": true, "/run/lock": true,
	"/usr": true, "/usr/local": true, "/usr/local/bin": true, "/usr/local/sbin": true,
	"/usr/local/etc": true, "/usr/local/lib": true, "/usr/local/share": true,
	"/usr/bin": true, "/usr/sbin": true, "/usr/lib": true, "/usr/share": true,
	"/bin": true, "/sbin": true, "/lib": true, "/lib64": true, "/boot": true,
	"/boot/efi": true, "/opt": true, "/home": true, "/root": true, "/tmp": true,
	"/dev": true, "/proc": true, "/sys": true, "/mnt": true, "/media": true,
	"/srv": true,
}

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
	// UpdatePathUnit watches the staging directory for an update request and
	// starts UpdateServiceUnit, the root updater that installs it.
	UpdatePathUnit    = "remote-mic-update.path"
	UpdateServiceUnit = "remote-mic-update.service"
	// UpdateDirName is the staging directory inside the state directory,
	// where the appliance and the updater meet.
	UpdateDirName = update.DirName
)

// unitDir is where the generated unit is written. Kept unexported: an operator
// never relocates it, and hardcoding it keeps uninstall's remove target exact.
const unitDir = "/etc/systemd/system"

// ServiceSpec is the resolved description of an install: who the service runs
// as and where its binary, config, and state live. Zero fields are filled from
// the Default* constants by withDefaults, so a caller can pass only the fields
// it overrides. The unit file name is fixed (DefaultUnitName); an operator does
// not run two differently named copies of this appliance on one host.
type ServiceSpec struct {
	User       string // system user to create and run as; its primary group takes the same name
	BinPath    string // absolute path the binary is copied to and ExecStart runs
	ConfigPath string // absolute config path baked into the unit via REMOTEMIC_CONFIG
	StateDir   string // absolute cert-store directory, owned by the service user
}

// withDefaults returns a copy of s with empty fields filled from the Default*
// constants.
func (s ServiceSpec) withDefaults() ServiceSpec {
	if s.User == "" {
		s.User = DefaultUser
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
	return s
}

// ConfigDir is the directory holding the config file; the installer creates and
// chowns it to the service user so the appliance and its run lock can write it.
func (s ServiceSpec) ConfigDir() string { return filepath.Dir(s.ConfigPath) }

// UnitPath is the absolute path the unit file is written to.
func (s ServiceSpec) UnitPath() string { return filepath.Join(unitDir, DefaultUnitName) }

// UpdateDir is the update staging directory, owned by the service user.
func (s ServiceSpec) UpdateDir() string { return filepath.Join(s.StateDir, UpdateDirName) }

// UpdatePathUnitPath is where the root updater's path unit is written.
func (s ServiceSpec) UpdatePathUnitPath() string { return filepath.Join(unitDir, UpdatePathUnit) }

// UpdateServiceUnitPath is where the root updater's service unit is written.
func (s ServiceSpec) UpdateServiceUnitPath() string {
	return filepath.Join(unitDir, UpdateServiceUnit)
}

// Validate rejects a spec that would render an unusable unit or damage the
// host: an invalid system user name, a non-absolute path (systemd requires
// absolute ExecStart and directory paths), a config or state directory that
// is a shared system location the installer would chown recursively and --purge
// would delete, or a bin path inside the config or state directory, which the
// service user owns. It assumes defaults have already been applied.
func (s ServiceSpec) Validate() error {
	if !userNameRe.MatchString(s.User) {
		return fmt.Errorf("service: invalid user name %q (want %s)", s.User, userNameRe)
	}
	if s.User == "root" {
		return errors.New("service: refusing to run as root; use a dedicated unprivileged user")
	}
	for label, p := range map[string]string{
		"bin path":    s.BinPath,
		"config path": s.ConfigPath,
		"state dir":   s.StateDir,
	} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("service: %s must be absolute, got %q", label, p)
		}
		// systemd splits ExecStart and ReadWritePaths on spaces and reads '%' as
		// a specifier, so a path containing either would silently misparse.
		if strings.ContainsAny(p, " \t\n%") {
			return fmt.Errorf("service: %s %q must not contain spaces, tabs, newlines, or %%", label, p)
		}
	}
	for label, dir := range map[string]string{
		"config directory": s.ConfigDir(),
		"state directory":  s.StateDir,
	} {
		if isSharedSystemDir(dir) {
			return fmt.Errorf("service: %s %q is a shared system directory; use a dedicated subdirectory such as %s or %s",
				label, filepath.Clean(dir), filepath.Dir(DefaultConfigPath), DefaultStateDir)
		}
		// The root updater runs the installed binary, so the service user,
		// which owns these directories, must not be able to replace it.
		if within(filepath.Dir(s.BinPath), dir) {
			return fmt.Errorf("service: bin path %q is inside the %s %q, which the service user can write; the root updater runs that binary",
				s.BinPath, label, filepath.Clean(dir))
		}
	}
	return nil
}

// within reports whether path is dir or lies under it, lexically.
func within(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// isSharedSystemDir reports whether dir is the filesystem root or a well-known
// shared system directory that must never be chowned or removed. It checks the
// lexically cleaned path and, best-effort, the symlink-resolved path, so a link
// such as /var/lock -> /run/lock or an operator pointing through a symlink into
// a shared tree is caught. A path that does not exist yet (a fresh install)
// resolves to itself and is judged on its literal form. The list cannot be
// exhaustive, so it is a guardrail against the common footguns, not a full
// sandbox.
func isSharedSystemDir(dir string) bool {
	clean := filepath.Clean(dir)
	if sharedSystemDirs[clean] {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return sharedSystemDirs[filepath.Clean(resolved)]
	}
	return false
}
