//go:build linux

package service

import "strings"

// InitSystem abstracts the boot service manager so the installer stays
// independent of systemd specifics. Only systemd is implemented today; an
// rpm-family or other target that needed a different manager would add its own
// implementation without touching the install orchestration.
type InitSystem interface {
	// Present reports whether this manager is the host's active init system.
	Present() bool
	// DaemonReload makes the manager re-read unit files after one is written.
	DaemonReload() error
	// Enable marks a unit to start at boot; when now is true it also starts it.
	Enable(unit string, now bool) error
	// Disable removes a unit's boot enablement.
	Disable(unit string) error
	// Stop stops a running unit.
	Stop(unit string) error
	// IsEnabled reports whether a unit is enabled at boot.
	IsEnabled(unit string) (bool, error)
	// IsActive reports whether a unit is currently running.
	IsActive(unit string) (bool, error)
}

// Systemd drives systemctl through the Runner seam.
type Systemd struct{ Run Runner }

// NewSystemd builds a Systemd backed by the real command runner.
func NewSystemd() *Systemd { return &Systemd{Run: execRunner} }

// Present reports whether the host is running systemd, via the standard
// /run/systemd/system marker (the sd_booted check). Install refuses on a host
// where this is false rather than writing a unit no manager will read.
func (s *Systemd) Present() bool { return fileExists("/run/systemd/system") }

// DaemonReload runs systemctl daemon-reload.
func (s *Systemd) DaemonReload() error {
	_, err := s.Run("systemctl", "daemon-reload")
	return err
}

// Enable runs systemctl enable, adding --now to also start the unit.
func (s *Systemd) Enable(unit string, now bool) error {
	args := []string{"enable"}
	if now {
		args = append(args, "--now")
	}
	args = append(args, unit)
	_, err := s.Run("systemctl", args...)
	return err
}

// Disable runs systemctl disable.
func (s *Systemd) Disable(unit string) error {
	_, err := s.Run("systemctl", "disable", unit)
	return err
}

// Stop runs systemctl stop.
func (s *Systemd) Stop(unit string) error {
	_, err := s.Run("systemctl", "stop", unit)
	return err
}

// IsEnabled reports whether unit is enabled at boot.
func (s *Systemd) IsEnabled(unit string) (bool, error) {
	return s.query(true, "is-enabled", unit)
}

// IsActive reports whether unit is currently running.
func (s *Systemd) IsActive(unit string) (bool, error) {
	return s.query(false, "is-active", unit)
}

// enabledStates and activeStates are the systemctl state words that count as a
// yes for is-enabled and is-active. systemctl exits nonzero for a "no" state
// (disabled, inactive) while still printing the word, so the boolean comes from
// the word, not the exit code.
var (
	enabledStates = map[string]bool{"enabled": true, "enabled-runtime": true, "static": true, "alias": true, "indirect": true, "generated": true}
	activeStates  = map[string]bool{"active": true}
)

// query runs a systemctl state check and maps its printed word to a boolean. A
// recognized word (whether a yes or a no) returns a nil error, since a "no" is a
// valid answer even though systemctl exits nonzero for it. Only an
// unrecognized word (an empty result on a not-found unit, or a genuine failure)
// returns the underlying error, so a real fault is never read as a plain "no".
func (s *Systemd) query(enabled bool, verb, unit string) (bool, error) {
	out, err := s.Run("systemctl", verb, unit)
	word := strings.TrimSpace(string(out))
	yes := activeStates
	no := map[string]bool{"inactive": true, "failed": true, "activating": true, "deactivating": true, "reloading": true, "maintenance": true}
	if enabled {
		yes = enabledStates
		no = map[string]bool{"disabled": true, "masked": true, "masked-runtime": true, "linked": true, "linked-runtime": true, "transient": true, "bad": true}
	}
	switch {
	case yes[word]:
		return true, nil
	case no[word]:
		return false, nil
	default:
		return false, err
	}
}
