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

// Uninstall stops and disables the unit and the root updater's units, removes
// their unit files, and reloads systemd. With purge it also removes the
// binary, the config and state directories, and the service user.
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

	// The path unit goes first, so it cannot start the updater while the
	// appliance is being torn down.
	_ = un.Init.Stop(UpdatePathUnit)
	_ = un.Init.Disable(UpdatePathUnit)
	_ = un.Init.Stop(UpdateServiceUnit)
	_ = un.Init.Stop(DefaultUnitName)
	_ = un.Init.Disable(DefaultUnitName)

	for _, p := range []string{s.UpdatePathUnitPath(), s.UpdateServiceUnitPath(), s.UnitPath()} {
		if err := un.removeFile(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("service: remove unit %s: %w", p, err)
		}
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
