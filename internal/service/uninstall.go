//go:build linux

package service

import (
	"errors"
	"fmt"
	"os"
)

// Uninstaller removes a service installed by Installer. Its side effects are
// function fields for the same test reasons as Installer.
type Uninstaller struct {
	Spec ServiceSpec
	Init InitSystem
	Run  Runner

	removeFile func(path string) error
	removeAll  func(path string) error
	userExists func(name string) bool
}

// NewUninstaller builds an Uninstaller with the production init system, runner,
// and filesystem and user operations.
func NewUninstaller(spec ServiceSpec) *Uninstaller {
	return &Uninstaller{
		Spec:       spec,
		Init:       NewSystemd(),
		Run:        execRunner,
		removeFile: os.Remove,
		removeAll:  os.RemoveAll,
		userExists: userExists,
	}
}

// Uninstall stops and disables the unit, removes the unit file, and reloads
// systemd. With purge it also removes the binary, the config and state
// directories, and the service user.
//
// Stop and disable are best-effort: a unit that is already stopped or was never
// enabled is not an error, so a partial or repeated uninstall still converges.
// Config and state survive a plain uninstall (an operator's token and
// certificates are not thrown away on a reinstall); purge is the explicit
// opt-in to delete them.
func (un *Uninstaller) Uninstall(purge bool) error {
	s := un.Spec.withDefaults()
	if err := s.Validate(); err != nil {
		return err
	}

	_ = un.Init.Stop(s.UnitName)
	_ = un.Init.Disable(s.UnitName)

	if err := un.removeFile(s.UnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove unit %s: %w", s.UnitPath(), err)
	}
	if err := un.Init.DaemonReload(); err != nil {
		return fmt.Errorf("service: daemon-reload: %w", err)
	}

	if !purge {
		return nil
	}
	if err := un.removeFile(s.BinPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove binary %s: %w", s.BinPath, err)
	}
	for _, dir := range []string{s.ConfigDir(), s.StateDir} {
		if err := un.removeAll(dir); err != nil {
			return fmt.Errorf("service: remove %s: %w", dir, err)
		}
	}
	// Only delete the user if it exists, so a repeated purge does not fail on an
	// already-removed account. userdel also drops the matching primary group.
	if un.userExists(s.User) {
		if _, err := un.Run("userdel", s.User); err != nil {
			return fmt.Errorf("service: delete user %q: %w", s.User, err)
		}
	}
	return nil
}
