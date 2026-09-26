//go:build linux

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
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
	// binDirOK refuses a bin directory anyone but root could change (see
	// checkBinDir); it runs before install writes anything.
	binDirOK func(dir string) error
	// makeBinDir creates the bin directory when it is missing and leaves an
	// existing one as it is (see ensureBinDir).
	makeBinDir func(path string) error
	// isLink reports whether path is a symlink, for the warning that the
	// install replaces it.
	isLink    func(path string) bool
	chownTree func(root string, uid, gid int) error
	copyFile  func(src, dst string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	// stagingDir creates the update staging directory inside the state
	// directory and hands it to the service user.
	stagingDir func(stateDir string, uid, gid int) error
	// rootOnly refuses an installed binary that is not a regular file only
	// root can write, in directories only root can write
	// (update.CheckRootOnlyFile).
	rootOnly   func(path string) error
	removeFile func(path string) error
	// warn receives a warning that does not fail the install.
	warn io.Writer
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
		binDirOK:   checkBinDir,
		makeBinDir: ensureBinDir,
		isLink:     isSymlink,
		chownTree:  chownTree,
		copyFile:   copyFile,
		writeFile:  atomicfile.Write,
		stagingDir: ensureStagingDir,
		rootOnly:   update.CheckRootOnlyFile,
		removeFile: os.Remove,
		warn:       os.Stderr,
	}
}

// Install creates the service user, installs the binary, the unit and the root
// updater's path and service units, hands the config, state and update staging
// directories to the service user, then reloads systemd and enables the unit
// and the updater's path unit (starting both too when now is true). A bin
// directory (or a directory above it) that anyone but root can write is
// refused before anything is written. Since the root updater runs the
// installed binary, an installed binary that still fails the root-only check
// gets no updater: install warns, removes updater units an earlier install
// left, and installs the appliance alone. That check runs on the installed
// binary after the config and state directories are handed over, so neither
// can hand it over too.
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
	// Before anything is written: in a bin directory, or a directory above
	// it, that someone else can write, they could plant a link that sends
	// root's writes below somewhere of their choosing.
	if err := in.binDirOK(filepath.Dir(s.BinPath)); err != nil {
		return fmt.Errorf("service: refusing to install to %s: %w; install it somewhere only root can write (the default is %s)", s.BinPath, err, DefaultBinPath)
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
	if err := in.makeBinDir(filepath.Dir(s.BinPath)); err != nil {
		return fmt.Errorf("service: create %s: %w", filepath.Dir(s.BinPath), err)
	}
	replacesLink := in.isLink(s.BinPath)
	if err := in.copyFile(self, s.BinPath, 0o755); err != nil {
		return fmt.Errorf("service: install binary to %s: %w", s.BinPath, err)
	}
	if replacesLink {
		_, _ = fmt.Fprintf(in.warn, "warning: %s was a symlink; it was replaced by the binary itself, not written through, so the file it pointed to is unchanged\n", s.BinPath)
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

	// The root updater runs this binary, so nobody but root may be able to
	// replace it; otherwise the appliance is installed without the updater.
	// The installed file is checked, not just its directory, and only after
	// the ownership handover, which a config or state path aliased onto the
	// bin directory through a link would otherwise slip past. An
	// install that fails before here leaves an earlier install's updater
	// units in place; the updater makes this same check before it acts.
	updater := true
	if err := in.rootOnly(s.BinPath); err != nil {
		updater = false
		_, _ = fmt.Fprintf(in.warn, "warning: installing without automatic updates: the root updater would run %s, but %v; make the binary and its directories writable only by root (or install it somewhere only root can write) and re-run sudo remote-mic service install\n", s.BinPath, err)
	}
	if updater {
		if err := in.writeUpdaterUnits(s); err != nil {
			return err
		}
	} else if err := in.removeUpdaterUnits(s); err != nil {
		return err
	}

	// The staging directory sits in the state directory the service user
	// owns, so it is handled on its own (see ensureStagingDir).
	if updater {
		if err := in.stagingDir(s.StateDir, uid, gid); err != nil {
			return fmt.Errorf("service: update staging directory %s: %w", s.UpdateDir(), err)
		}
	}

	if err := in.Init.DaemonReload(); err != nil {
		return fmt.Errorf("service: daemon-reload: %w", err)
	}
	if err := in.Init.Enable(DefaultUnitName, now); err != nil {
		return fmt.Errorf("service: enable %s: %w", DefaultUnitName, err)
	}
	if !updater {
		return nil
	}
	// A path unit that hit its start limit stays failed until reset, so
	// re-running install is how an operator revives it. Nothing to reset is
	// not an error worth failing the install for.
	_ = in.Init.ResetFailed(UpdateServiceUnit)
	_ = in.Init.ResetFailed(UpdatePathUnit)
	// Only the path unit is enabled: it starts the updater service on demand.
	if err := in.Init.Enable(UpdatePathUnit, now); err != nil {
		return fmt.Errorf("service: enable %s: %w", UpdatePathUnit, err)
	}
	return nil
}

// writeUpdaterUnits writes the root updater's service and path units.
func (in *Installer) writeUpdaterUnits(s ServiceSpec) error {
	pathUnit, updaterUnit, err := RenderUpdater(s)
	if err != nil {
		return err
	}
	if err := in.writeFile(s.UpdateServiceUnitPath(), updaterUnit, 0o644); err != nil {
		return fmt.Errorf("service: write unit %s: %w", s.UpdateServiceUnitPath(), err)
	}
	if err := in.writeFile(s.UpdatePathUnitPath(), pathUnit, 0o644); err != nil {
		return fmt.Errorf("service: write unit %s: %w", s.UpdatePathUnitPath(), err)
	}
	return nil
}

// removeUpdaterUnits stops and removes updater units an earlier install left,
// which would otherwise keep running a binary in a directory that is no
// longer root-only. Stopping or disabling a unit that is not there is fine.
func (in *Installer) removeUpdaterUnits(s ServiceSpec) error {
	_ = in.Init.Stop(UpdatePathUnit)
	_ = in.Init.Disable(UpdatePathUnit)
	_ = in.Init.Stop(UpdateServiceUnit)
	// A unit that failed stays listed as failed, file or not, until reset.
	_ = in.Init.ResetFailed(UpdatePathUnit)
	_ = in.Init.ResetFailed(UpdateServiceUnit)
	for _, p := range []string{s.UpdatePathUnitPath(), s.UpdateServiceUnitPath()} {
		if err := in.removeFile(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("service: remove unit %s: %w", p, err)
		}
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

// checkBinDir refuses dir unless only root can change what it resolves to
// (update.CheckRootOnly). A dir that does not exist yet is judged by its
// deepest existing ancestor, under which root creates the rest.
func checkBinDir(dir string) error {
	for {
		_, err := os.Lstat(dir)
		if err == nil {
			return update.CheckRootOnly(dir)
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, fs.ErrNotExist) || parent == dir {
			return err
		}
		dir = parent
	}
}

// ensureBinDir creates the bin directory and any missing parents with mode
// 0755, whatever the umask, so the service user can reach the binary. It
// never chmods a directory that already existed: that is the operator's
// (often /usr/local/bin). It relies on checkBinDir having passed, so nobody
// but root can swap a directory it creates for a link before the chmod.
func ensureBinDir(path string) error {
	var missing []string
	for p := path; ; {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	for _, p := range missing {
		if err := os.Chmod(p, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// isSymlink reports whether path is a symlink itself.
func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
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
		// flat files (config, run lock, certificate, key, pin marker) apart from
		// the update staging directory, which ensureStagingDir handles, and refusing
		// to recurse closes a TOCTOU where an unprivileged user swaps a
		// subdirectory for a symlink between the walk's stat and its read, which
		// would otherwise let the chown escape to a linked-to tree.
		if p != root && d.IsDir() {
			return filepath.SkipDir
		}
		return lchown(p, uid, gid)
	})
}

// ensureStagingDir creates UpdateDirName inside stateDir with mode 0700 and
// hands it to uid:gid. The state directory belongs to the service user, who
// could have planted a link or a file at that name, so the name is resolved
// through an os.Root on the state directory and anything but a real directory
// is refused: root never chmods or chowns something else in its place.
func ensureStagingDir(stateDir string, uid, gid int) error {
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(UpdateDirName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if fi, err := root.Lstat(UpdateDirName); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (%s); remove it and re-run the install", UpdateDirName, fi.Mode().Type())
	}
	// Act on an open handle, not the name: the name can be swapped after the
	// check, but a directory handle cannot turn into a link or a hard link to
	// a file.
	f, err := root.OpenFile(UpdateDirName, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o700); err != nil {
		return err
	}
	return f.Chown(uid, gid)
}

// copyFile copies src to dst atomically with the given mode, reading the whole
// file into memory (the binary is small). It uses atomicfile so a concurrent
// reader never sees a partial binary, and copying the running binary onto its
// own destination is safe (the source is fully read before the rename).
//
// The copy replaces the entry at dst rather than writing through it
// (atomicfile.Replace): on a bin directory someone else can write, a link
// planted at dst would otherwise have root overwrite a file of their choosing,
// and a file they own there would keep its owner.
func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src) //nolint:gosec // src is the running binary path from os.Executable
	if err != nil {
		return err
	}
	return atomicfile.Replace(dst, data, perm)
}
