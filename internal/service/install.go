//go:build linux

package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
)

// Installer performs a system-wide install of the appliance as a systemd
// service. Its side-effecting operations are function fields so the whole
// orchestration is unit-testable without touching the real system, real users,
// or real systemctl; NewInstaller wires the production implementations.
type Installer struct {
	Spec ServiceSpec
	Init InitSystem
	Run  Runner
	Plat Platform

	selfExe    func() (string, error)
	userExists func(name string) bool
	lookupUser func(name string) (uid, gid int, err error)
	ensureDir  func(path string, perm os.FileMode) error
	chownTree  func(root string, uid, gid int) error
	copyFile   func(src, dst string, perm os.FileMode) error
	writeFile  func(path string, data []byte, perm os.FileMode) error
}

// NewInstaller builds an Installer for spec with the production init system,
// command runner, detected platform, and real filesystem and user operations.
func NewInstaller(spec ServiceSpec) *Installer {
	return &Installer{
		Spec:       spec,
		Init:       NewSystemd(),
		Run:        execRunner,
		Plat:       Detect(),
		selfExe:    os.Executable,
		userExists: userExists,
		lookupUser: lookupUser,
		ensureDir:  ensureDir,
		chownTree:  chownTree,
		copyFile:   copyFile,
		writeFile:  atomicfile.Write,
	}
}

// Install creates the service user, installs the binary and unit, hands the
// config and state directories to the service user, then reloads systemd and
// enables the unit (starting it too when now is true).
//
// The order is deliberate: ownership is handed over BEFORE the unit starts, so
// the appliance can write config.yaml on first provision and take its run lock
// beside the config. Starting first would boot the service against a
// root-owned config directory, where the first-provision write fails and the
// run lock cannot be created (leaving the appliance running unlocked).
func (in *Installer) Install(now bool) error {
	s := in.Spec.withDefaults()
	if err := s.Validate(); err != nil {
		return err
	}
	if !in.Init.Present() {
		return errors.New("service: systemd is not the active init system (no /run/systemd/system); cannot install a service unit")
	}

	if err := in.ensureUser(s); err != nil {
		return err
	}
	uid, gid, err := in.lookupUser(s.User)
	if err != nil {
		return fmt.Errorf("service: resolve user %q after creation: %w", s.User, err)
	}

	self, err := in.selfExe()
	if err != nil {
		return fmt.Errorf("service: locate the running binary: %w", err)
	}
	if err := in.ensureDir(filepath.Dir(s.BinPath), 0o755); err != nil {
		return fmt.Errorf("service: create %s: %w", filepath.Dir(s.BinPath), err)
	}
	if err := in.copyFile(self, s.BinPath, 0o755); err != nil {
		return fmt.Errorf("service: install binary to %s: %w", s.BinPath, err)
	}

	unit, err := Render(s)
	if err != nil {
		return err
	}
	if err := in.writeFile(s.UnitPath(), unit, 0o644); err != nil {
		return fmt.Errorf("service: write unit %s: %w", s.UnitPath(), err)
	}

	// Create the config and state directories and hand them to the service user
	// recursively, so a pre-existing root-owned config.yaml or config.yaml.lock
	// (left by an earlier hand-run `sudo remote-mic serve`) is handed over too.
	dirs := []struct {
		path string
		perm os.FileMode
	}{
		{s.ConfigDir(), 0o750},
		{s.StateDir, 0o700},
	}
	for _, d := range dirs {
		if err := in.ensureDir(d.path, d.perm); err != nil {
			return fmt.Errorf("service: create %s: %w", d.path, err)
		}
		if err := in.chownTree(d.path, uid, gid); err != nil {
			return fmt.Errorf("service: chown %s to %s: %w", d.path, s.User, err)
		}
	}

	if err := in.Init.DaemonReload(); err != nil {
		return fmt.Errorf("service: daemon-reload: %w", err)
	}
	if err := in.Init.Enable(DefaultUnitName, now); err != nil {
		return fmt.Errorf("service: enable %s: %w", DefaultUnitName, err)
	}
	return nil
}

// ensureUser makes sure the service group and user exist. The group named after
// the user is ensured unconditionally (so the unit's Group= always resolves);
// the user and its audio-group membership are created only when the user does
// not already exist, leaving a pre-existing account's memberships and shell to
// the operator. groupadd --force is idempotent, so an existing group is not an
// error.
func (in *Installer) ensureUser(s ServiceSpec) error {
	// Always ensure a group named after the user exists (groupadd --force is
	// idempotent), even when the user already exists, so the unit's
	// Group=<user> always resolves. A pre-existing user whose primary group has
	// a different name would otherwise fail the unit at start with "Failed to
	// determine group credentials".
	if _, err := in.Run("groupadd", "--system", "--force", s.User); err != nil {
		return fmt.Errorf("service: create group %q: %w", s.User, err)
	}
	if in.userExists(s.User) {
		return nil
	}
	if _, err := in.Run("useradd", "--system", "--no-create-home",
		"--shell", in.Plat.NologinShell(), "--gid", s.User, s.User); err != nil {
		return fmt.Errorf("service: create user %q: %w", s.User, err)
	}
	if _, err := in.Run("usermod", "--append", "--groups", "audio", s.User); err != nil {
		return fmt.Errorf("service: add %q to the audio group: %w", s.User, err)
	}
	return nil
}

// userExists reports whether a system user is already defined.
func userExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}

// lookupUser resolves a user name to its numeric uid and gid.
func lookupUser(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err = strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	return uid, gid, nil
}

// ensureDir creates path (and parents) and enforces its mode, so an existing
// directory with looser permissions is tightened to perm.
func ensureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}

// chownTree chowns root and its immediate flat-file entries to uid/gid, without
// descending into subdirectories, using Lchown so a symlink entry is retargeted
// rather than followed. Staying shallow both matches the flat layout (config,
// lock, cert, key, pin) and closes the subdirectory-swap TOCTOU noted in the
// callback.
// lchown is a seam so chownTree's traversal (which paths it touches, and that it
// does not descend) is testable without a second uid.
var lchown = os.Lchown

func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Do not descend into subdirectories: the config and state dirs hold only
		// flat files (config, run lock, certificate, key, pin marker), and refusing
		// to recurse closes a TOCTOU where an unprivileged user swaps a
		// subdirectory for a symlink between the walk's stat and its read, which
		// would otherwise let the chown escape to a linked-to tree.
		if p != root && d.IsDir() {
			return filepath.SkipDir
		}
		return lchown(p, uid, gid)
	})
}

// copyFile copies src to dst atomically with the given mode, reading the whole
// file into memory (the binary is small). It uses atomicfile so a concurrent
// reader never sees a partial binary, and copying the running binary onto its
// own destination is safe (the source is fully read before the rename).
func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src) //nolint:gosec // src is the running binary path from os.Executable
	if err != nil {
		return err
	}
	return atomicfile.Write(dst, data, perm)
}
