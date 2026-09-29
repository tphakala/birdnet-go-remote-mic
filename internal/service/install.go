//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	// Version is the running binary's version, which the downgrade guard
	// compares with the installed binary's.
	Version string
	// AllowDowngrade lets install replace a newer installed binary.
	AllowDowngrade bool

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
	// dirsOK refuses a config or state directory that is the bin directory
	// or a directory above it (see checkNotOverBinDir).
	dirsOK func(s ServiceSpec) error
	// trustedBin refuses to run the binary at path unless it is a regular
	// file only root owns and can write (update.CheckRootOnlyFile): install
	// runs as root, so it must not execute a file someone else can change or
	// a link that leads elsewhere.
	trustedBin func(path string) error
	// lockBin takes the lock the root updater also takes around replacing the
	// binary (update.LockBin) and returns its release; waiting is called once
	// if another process holds it.
	lockBin func(path string, waiting func()) (release func(), err error)
	// binVersion reports the version the binary at path prints, and whether
	// a binary is there at all (see installedVersion).
	binVersion func(path string) (version string, present bool, err error)
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
	// lexists reports whether anything, a link included, is at path.
	lexists func(path string) (bool, error)
	// syncDir makes the removals in dir durable (atomicfile.SyncDir).
	syncDir func(dir string)
	// warn receives a warning that does not fail the install.
	warn io.Writer
}

// NewInstaller builds an Installer for spec with the production init system,
// command runner, detected platform, and real filesystem, user and
// installed-version operations (running the installed binary only when it is
// a root-only regular file) and the lock shared with the root updater. The caller sets Version and AllowDowngrade.
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
		dirsOK:     checkNotOverBinDir,
		trustedBin: update.CheckRootOnlyFile,
		lockBin:    lockBin,
		binVersion: installedVersion,
		isLink:     isSymlink,
		chownTree:  chownTree,
		copyFile:   copyFile,
		writeFile:  atomicfile.Write,
		stagingDir: ensureStagingDir,
		rootOnly:   update.CheckRootOnlyFile,
		removeFile: os.Remove,
		lexists:    lexists,
		syncDir:    atomicfile.SyncDir,
		warn:       os.Stderr,
	}
}

// Install creates the service user, installs the binary, the unit and the root
// updater's path and service units, hands the config, state and update staging
// directories to the service user, then reloads systemd and enables the unit
// and the updater's path unit (starting both too when now is true). A bin
// directory (or a directory above it) that anyone but root can write is
// refused before anything is written, a config or state directory that is the
// bin directory or one above it (checkNotOverBinDir) before anything but the
// bin directory is, and an install that would replace
// a newer installed binary with this older one (see checkDowngrade). From the
// version check to the end it holds the lock the root updater takes around
// replacing the binary (update.LockBin), waiting for an update in progress and
// failing if one outlasts the wait (with a hint to try again) or if the lock
// cannot be taken at all. Around replacing the binary it clears an update that
// was cut off: the kept copy before, the journal after (dropKeptCopy,
// dropJournal). The ownership handover refuses a file with a second hard link (chownTree). Since the root updater runs the
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
	// The lock file lives in the bin directory, so it has to exist first.
	if err := in.makeBinDir(filepath.Dir(s.BinPath)); err != nil {
		return fmt.Errorf("service: create %s: %w", filepath.Dir(s.BinPath), err)
	}
	// The config and state directories are handed to the service user below;
	// one that is the bin directory, or holds it, would hand that over too.
	if err := in.dirsOK(s); err != nil {
		return fmt.Errorf("service: refusing to install: %w", err)
	}
	// The root updater replaces the same binary; hold its lock from the
	// version check to the end, so an update cannot land between the check
	// and the copy or be overwritten by it.
	release, err := in.lockBin(s.BinPath, func() {
		_, _ = fmt.Fprintf(in.warn, "waiting for an update of %s in progress to finish\n", s.BinPath)
	})
	if err != nil {
		if errors.Is(err, update.ErrBinBusy) {
			return fmt.Errorf("service: %s: %w; try again in a few minutes", s.BinPath, err)
		}
		return fmt.Errorf("service: lock %s: %w", s.BinPath, err)
	}
	defer release()
	if err := in.checkDowngrade(s.BinPath); err != nil {
		return err
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
	if err := in.installBinary(self, s.BinPath); err != nil {
		return err
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
	// The installed file is checked, not just its directory, and after the
	// ownership handover, as a backstop to checkNotOverBinDir. An install
	// that fails before here leaves an earlier install's updater
	// units in place; the updater makes this same check before it acts.
	updater := true
	if err := in.rootOnly(s.BinPath); err != nil {
		updater = false
		_, _ = fmt.Fprintf(in.warn, "warning: installing without automatic updates: the root updater would run %s, but %v; make the binary and its directories writable only by root (or install it somewhere only root can write) and re-run %s\n", s.BinPath, err, rerunCommand(self, s.BinPath))
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

// installBinary replaces the binary at bin with self, clearing the update
// that was cut off around it (dropKeptCopy before, dropJournal after).
func (in *Installer) installBinary(self, bin string) error {
	replacesLink := in.isLink(bin)
	interrupted, err := in.dropKeptCopy(bin)
	if err != nil {
		return err
	}
	if err := in.copyFile(self, bin, 0o755); err != nil {
		err = fmt.Errorf("service: install binary to %s: %w", bin, err)
		if interrupted {
			err = fmt.Errorf("%w (the kept copy of the interrupted update was already removed, so that update will not be rolled back)", err)
		}
		return err
	}
	if replacesLink {
		_, _ = fmt.Fprintf(in.warn, "warning: %s was a symlink; it was replaced by the binary itself, not written through, so the file it pointed to is unchanged\n", bin)
	}
	if interrupted {
		return in.dropJournal(bin)
	}
	return nil
}

// dropKeptCopy removes the kept copy of an update that was cut off before it
// was confirmed healthy, when its journal is present, and reports whether it
// was. It runs before the binary is replaced, under the lock, so that nothing
// later can roll the install back: a crash or failure after this point leaves
// a journal with no kept copy, which the updater reports and leaves the
// installed binary alone for. Left in place, the pair would roll the next
// updater start back to a copy older than what install writes. The removal is
// synced before the binary is replaced. A failure here aborts the install
// before the binary is touched.
func (in *Installer) dropKeptCopy(bin string) (interrupted bool, err error) {
	present, err := in.lexists(bin + update.JournalSuffix)
	if err != nil {
		return false, fmt.Errorf("service: look for the journal of an interrupted update: %w", err)
	}
	if !present {
		return false, nil
	}
	if err := in.removeFile(bin + update.PrevSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("service: remove the kept copy of an interrupted update: %w", err)
	}
	// Only the later rename is synced by the copy: without this, a power cut
	// could persist the new binary but not the removal, and the next updater
	// start would roll the fresh install back to the kept copy.
	in.syncDir(filepath.Dir(bin))
	return true, nil
}

// dropJournal removes the journal of the interrupted update whose kept copy
// dropKeptCopy removed, once the binary is replaced, and says the update will
// not be rolled back.
func (in *Installer) dropJournal(bin string) error {
	if err := in.removeFile(bin + update.JournalSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: remove the journal of an interrupted update: %w", err)
	}
	in.syncDir(filepath.Dir(bin))
	_, _ = fmt.Fprintf(in.warn, "warning: an earlier update of %s was interrupted before it was confirmed healthy; this install replaces the binary, so that update will not be rolled back\n", bin)
	return nil
}

// lexists reports whether something, a dangling link included, is at path.
func lexists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// rerunCommand is the command to re-run the install with. It names the binary
// this install ran from, since a bare command name could resolve to another
// copy on PATH, unless that is the installed binary at bin: the one that just
// failed the root-only check, which root must not be told to run, or a path
// that no longer exists (os.Executable drops the " (deleted)" suffix Linux adds
// to the link of a removed executable, so the path it returns can be gone).
func rerunCommand(self, bin string) string {
	if _, err := os.Stat(self); err != nil || sameFile(self, bin) {
		return "service install, run from the release binary you installed from"
	}
	return "sudo " + update.ShellQuote(self) + " service install"
}

// sameFile reports whether a and b name one file, links and all.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
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

// checkNotOverBinDir refuses a config or state directory that is, through a
// link or a mount, a directory on the way to the bin directory: the bin
// directory itself, a directory above it, or one a symlink on its path leads
// through (update.PathDirs). Install hands both to the service user and purge
// deletes both, and the owner of any directory on that way can change which
// binary the root updater runs. Directories are compared by identity, not by
// name, which a link or bind mount gets past. A config or state directory
// that does not exist yet is skipped (install creates it fresh), and so is
// the check when the bin directory is missing (nothing to reach).
func checkNotOverBinDir(s ServiceSpec) error {
	type owned struct {
		label, path string
		info        fs.FileInfo
	}
	var dirs []owned
	for _, d := range []owned{{label: "config directory", path: s.ConfigDir()}, {label: "state directory", path: s.StateDir}} {
		info, err := os.Stat(d.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check the %s %s: %w", d.label, d.path, err)
		}
		d.info = info
		dirs = append(dirs, d)
	}
	binDir := filepath.Dir(s.BinPath)
	onPath, err := update.PathDirs(binDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve the bin directory: %w", err)
	}
	for _, p := range onPath {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("resolve the bin directory: %w", err)
		}
		for _, d := range dirs {
			if os.SameFile(d.info, info) {
				return fmt.Errorf("the %s %s is %s, which is on the way to the bin directory %s; the service user could change the binary the root updater runs", d.label, d.path, p, binDir)
			}
		}
	}
	return nil
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

// lchown and fchown are seams so chownTree's traversal (which paths it
// touches, and that it does not descend) is testable without a second uid.
var (
	lchown = os.Lchown
	fchown = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
)

// chownTree chowns root and its immediate regular files to uid/gid, without
// descending into subdirectories. Staying shallow matches the flat layout
// (config, lock, cert, key, pin) and closes a TOCTOU where an unprivileged
// user swaps a subdirectory for a symlink between the walk's stat and its
// read, which would otherwise let the chown escape to a linked-to tree. Other
// entries (links, sockets, devices) are left alone: owning a link grants
// nothing, and chowning it by name would race with a swap. A file with a
// second hard link is refused (see chownFile).
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return lchown(p, uid, gid)
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return chownFile(p, uid, gid)
	})
}

// chownFile hands the regular file at p to uid/gid. The service user owns the
// directory p is in and can swap or hard link entries there, so the check and
// the chown act on one open handle, not the name: a file that another name
// also reaches (a link to something elsewhere on the host, such as a file only
// root should own) would otherwise be handed over too. A file that vanished
// meanwhile is skipped.
func chownFile(p string, uid, gid int) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // p is an entry of the config or state directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%s has more than one hard link; remove the extra link and re-run the install", p)
	}
	return fchown(f, uid, gid)
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

// checkDowngrade refuses to replace the binary at path with an older one: an
// unpacked tarball stays at the version it was extracted at while the
// installed copy updates itself, and re-running install from the old copy
// would otherwise write it over the newer one and report success. An
// installed binary that is absent, will not run, or names a version that
// cannot be compared with this one (a development build) is the repair case:
// install goes ahead, with a warning where something looked wrong. An
// installed binary that is not a root-only regular file is never run (root
// would be executing something another account can change, or a link to it):
// its version is unknown, so install goes ahead and replaces it.
func (in *Installer) checkDowngrade(path string) error {
	if err := in.trustedBin(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			_, _ = fmt.Fprintf(in.warn, "warning: not running the installed %s to read its version (%v); replacing it\n", path, err)
		}
		return nil
	}
	installed, present, err := in.binVersion(path)
	if !present {
		return nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(in.warn, "warning: cannot tell the version of the installed %s (%v); replacing it\n", path, err)
		return nil
	}
	newer, err := update.Ahead(installed, in.Version)
	if err != nil {
		_, _ = fmt.Fprintf(in.warn, "warning: cannot compare the installed %s (%s) with this binary (%s): %v; replacing it\n", path, installed, in.Version, err)
		return nil
	}
	if newer && !in.AllowDowngrade {
		return fmt.Errorf("service: %s is %s, newer than this binary (%s); run the installed one (sudo %s service install) or pass --allow-downgrade", path, installed, in.Version, update.ShellQuote(path))
	}
	return nil
}

// lockBin takes the bin path's lock for up to update.InstallBinLockWait.
func lockBin(path string, waiting func()) (release func(), err error) {
	return update.LockBin(context.Background(), path, update.InstallBinLockWait, waiting)
}

// installedVersion runs the binary at path and reads its version from the
// first line of `version` output, `remote-mic <version>`, which is a
// contract between versions. present is false when nothing is at path.
func installedVersion(path string) (version string, present bool, err error) {
	if _, err := os.Stat(path); err != nil {
		return "", !errors.Is(err, fs.ErrNotExist), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output() //nolint:gosec // the binary this install is about to replace, at the validated bin path
	if err != nil {
		return "", true, fmt.Errorf("run %s version: %w", path, err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	name, v, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok || name != "remote-mic" || v == "" {
		return "", true, fmt.Errorf("unexpected version output %q", line)
	}
	return v, true, nil
}
