//go:build linux

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// testInstaller wires an Installer whose every side effect logs into events, so
// a test asserts the exact ordering across user, filesystem, and systemd steps.
// userThere toggles the user-already-exists path.
func testInstaller(events *[]string, init *fakeInit, userThere *bool) *Installer {
	return &Installer{
		Spec: ServiceSpec{},
		Init: init,
		Run: func(name string, args ...string) ([]byte, error) {
			*events = append(*events, "run "+call{name: name, args: args}.line())
			return nil, nil
		},
		Plat:       Platform{Family: FamilyDebian},
		selfExe:    func() (string, error) { return "/home/pi/remote-mic", nil },
		userExists: func(string) bool { return *userThere },
		lookupUser: func(string) (int, int, error) { return 990, 990, nil },
		ensureDir:  func(p string, _ os.FileMode) error { *events = append(*events, "mkdir "+p); return nil },
		binDirOK:   func(d string) error { *events = append(*events, "bindir "+d); return nil },
		makeBinDir: func(p string) error { *events = append(*events, "mkbindir "+p); return nil },
		isLink:     func(string) bool { return false },
		chownTree: func(root string, uid, gid int) error {
			*events = append(*events, fmt.Sprintf("chown %s %d:%d", root, uid, gid))
			return nil
		},
		copyFile: func(src, dst string, _ os.FileMode) error {
			*events = append(*events, "copy "+src+" -> "+dst)
			return nil
		},
		writeFile: func(path string, _ []byte, _ os.FileMode) error { *events = append(*events, "write "+path); return nil },
		rootOnly:  func(dir string) error { *events = append(*events, "rootonly "+dir); return nil },
		stagingDir: func(stateDir string, uid, gid int) error {
			*events = append(*events, fmt.Sprintf("staging %s %d:%d", stateDir, uid, gid))
			return nil
		},
		removeFile: func(path string) error { *events = append(*events, "remove "+path); return nil },
		warn:       io.Discard,
	}
}

func TestInstallSequence(t *testing.T) {
	// NologinShell probes the filesystem; pin it so the useradd shell is stable.
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = func(p string) bool { return p == nologinPath }

	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := false
	in := testInstaller(&events, init, &userThere)

	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantSeq(t, events, []string{
		"bindir /usr/local/bin",
		evGroupadd,
		"run useradd --system --no-create-home --shell /usr/sbin/nologin --gid remote-mic remote-mic",
		"run usermod --append --groups audio remote-mic",
		"mkbindir /usr/local/bin",
		"copy /home/pi/remote-mic -> /usr/local/bin/remote-mic",
		"write /etc/systemd/system/remote-mic.service",
		"mkdir /etc/remote-mic",
		evChownConfig,
		"mkdir /var/lib/remote-mic",
		"chown /var/lib/remote-mic 990:990",
		"rootonly /usr/local/bin/remote-mic",
		"write /etc/systemd/system/remote-mic-update.service",
		"write /etc/systemd/system/remote-mic-update.path",
		"staging /var/lib/remote-mic 990:990",
		evReload,
		evEnableNowApp,
		evResetUpdater,
		evResetPath,
		"enable --now remote-mic-update.path",
	})
}

// TestInstallChownBeforeStart is the regression guard for the review's ordering
// finding: the config directory must be owned by the service user before the
// unit starts, or first-provision and the run lock fail on a root-owned dir.
func TestInstallChownBeforeStart(t *testing.T) {
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = func(p string) bool { return p == nologinPath }

	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := false
	if err := testInstaller(&events, init, &userThere).Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	chownIdx, enableIdx := -1, -1
	for i, e := range events {
		if e == evChownConfig {
			chownIdx = i
		}
		if e == evEnableNowApp {
			enableIdx = i
		}
	}
	if chownIdx < 0 || enableIdx < 0 {
		t.Fatalf("missing events: chown=%d enable=%d in %v", chownIdx, enableIdx, events)
	}
	if chownIdx > enableIdx {
		t.Errorf("chown (%d) must precede enable/start (%d)", chownIdx, enableIdx)
	}
}

func TestInstallUserExistsSkipsCreation(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	if err := testInstaller(&events, init, &userThere).Install(false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	var groupadd bool
	for _, e := range events {
		switch e {
		case evGroupadd:
			groupadd = true
		case "run useradd --system --no-create-home --shell /usr/sbin/nologin --gid remote-mic remote-mic",
			"run usermod --append --groups audio remote-mic":
			t.Errorf("existing user should not trigger user creation: saw %q", e)
		}
	}
	// The group is ensured even for a pre-existing user, so the unit's Group=
	// always resolves.
	if !groupadd {
		t.Error("groupadd must run even when the user exists, to ensure the group")
	}
	// now=false enables without starting, the appliance and the updater's
	// path unit alike.
	wantSeq(t, events[len(events)-4:], []string{
		"enable remote-mic.service",
		evResetUpdater,
		evResetPath,
		"enable remote-mic-update.path",
	})
}

// TestChownTreeStaysShallow proves the TOCTOU fix: chownTree touches the root
// and its immediate flat files but does not descend into a subdirectory.
func TestChownTreeStaysShallow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := lchown
	t.Cleanup(func() { lchown = orig })
	var visited []string
	lchown = func(p string, _, _ int) error {
		visited = append(visited, p)
		return nil
	}
	if err := chownTree(root, 990, 990); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	for _, p := range visited {
		if p == filepath.Join(sub, "inside") {
			t.Errorf("chownTree descended into a subdirectory: touched %q", p)
		}
	}
	// It must still touch the root and its immediate file.
	wantTouched := map[string]bool{root: false, filepath.Join(root, "config.yaml"): false}
	for _, p := range visited {
		if _, ok := wantTouched[p]; ok {
			wantTouched[p] = true
		}
	}
	for p, touched := range wantTouched {
		if !touched {
			t.Errorf("chownTree did not touch %q", p)
		}
	}
}

func TestInstallRefusesWithoutSystemd(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: false}
	userThere := false
	err := testInstaller(&events, init, &userThere).Install(true)
	if err == nil {
		t.Fatal("Install without systemd = nil error, want refusal")
	}
	if len(events) != 0 {
		t.Errorf("no side effects expected before the systemd check, got %v", events)
	}
}

// TestEnsureStagingDir pins the real staging-directory step: it creates the
// directory 0700 inside the state directory, tightens an existing one, and
// refuses a symlink the service user planted there, leaving its target alone.
func TestEnsureStagingDir(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid() // Lchown to ourselves works unprivileged
	state := t.TempDir()
	if err := ensureStagingDir(state, uid, gid); err != nil {
		t.Fatalf("create: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(state, UpdateDirName))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("staging dir %v, %v; want a 0700 directory", fi, err)
	}
	if err := os.Chmod(filepath.Join(state, UpdateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureStagingDir(state, uid, gid); err != nil {
		t.Fatalf("existing: %v", err)
	}
	if fi, _ := os.Lstat(filepath.Join(state, UpdateDirName)); fi.Mode().Perm() != 0o700 {
		t.Errorf("existing staging dir mode %v, want 0700", fi.Mode().Perm())
	}

	state = t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(state, UpdateDirName)); err != nil {
		t.Fatal(err)
	}
	if err := ensureStagingDir(state, uid, gid); err == nil {
		t.Error("a planted symlink was accepted as the staging directory")
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o755 {
		t.Errorf("the link target's mode changed to %v", fi.Mode().Perm())
	}

	state = t.TempDir()
	if err := os.WriteFile(filepath.Join(state, UpdateDirName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureStagingDir(state, uid, gid); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a planted file: got %v", err)
	}
}

// TestInstallWithoutUpdaterOnUntrustedBin pins that a binary or bin directory
// that is not root-only still gets the appliance, with a warning, but no updater:
// updater units an earlier install left are stopped and removed, and no
// staging directory is made.
func TestInstallWithoutUpdaterOnUntrustedBin(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.rootOnly = func(string) error { return errors.New("/opt is writable by group 50") }
	var warn strings.Builder
	in.warn = &warn
	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := warn.String(); !strings.Contains(got, "without automatic updates") || !strings.Contains(got, "writable by group 50") {
		t.Errorf("warning %q, want it to name the reason updates are off", got)
	}
	wantSeq(t, events, []string{
		"bindir /usr/local/bin",
		evGroupadd,
		"mkbindir /usr/local/bin",
		"copy /home/pi/remote-mic -> /usr/local/bin/remote-mic",
		"write /etc/systemd/system/remote-mic.service",
		"mkdir /etc/remote-mic",
		evChownConfig,
		"mkdir /var/lib/remote-mic",
		"chown /var/lib/remote-mic 990:990",
		evStopPath,
		evDisablePath,
		evStopUpdater,
		evResetPath,
		evResetUpdater,
		"remove /etc/systemd/system/remote-mic-update.path",
		"remove /etc/systemd/system/remote-mic-update.service",
		evReload,
		evEnableNowApp,
	})
}

// TestInstallIgnoresResetFailedErrors pins that a reset-failed that fails
// (nothing to reset, say) does not fail the install.
func TestInstallIgnoresResetFailedErrors(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true, resetErr: errors.New("unit not loaded")}
	userThere := true
	if err := testInstaller(&events, init, &userThere).Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if last := events[len(events)-1]; last != "enable --now remote-mic-update.path" {
		t.Errorf("last event %q, want the path unit enabled", last)
	}
}

// TestCopyFileReplacesPlantedLink pins the fix for a link planted at the bin
// path of a directory others can write: the install must replace the link
// with the binary, never overwrite the file it points to.
func TestCopyFileReplacesPlantedLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "self")
	if err := os.WriteFile(src, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "bin", "remote-mic")
	if err := ensureBinDir(filepath.Dir(dst)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("link target = %q, want it untouched (keep)", b)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("bin path is %v, want a regular file", fi.Mode().Type())
	}
	if b, _ := os.ReadFile(dst); string(b) != "binary" {
		t.Errorf("bin path content = %q, want binary", b)
	}
}

// TestEnsureBinDirLeavesExistingMode pins that the bin directory is created
// but an existing one is not chmodded, since it is the operator's. Not
// parallel: it sets the process umask.
func TestEnsureBinDirLeavesExistingMode(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureBinDir(dir); err != nil {
		t.Fatalf("ensureBinDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("existing bin dir mode = %v, want 0700 unchanged", got)
	}
	// Created directories get 0755 despite the 077 umask, so the service
	// user can reach the binary.
	base := t.TempDir()
	missing := filepath.Join(base, "a", "b")
	if err := ensureBinDir(missing); err != nil {
		t.Fatalf("ensureBinDir(missing): %v", err)
	}
	if fi, err := os.Stat(base); err != nil {
		t.Fatal(err)
	} else if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("existing ancestor %s: mode %v, want 0700 unchanged", base, got)
	}
	for _, p := range []string{filepath.Join(base, "a"), missing} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); !fi.IsDir() || got != 0o755 {
			t.Errorf("created %s: mode %v, want a 0755 directory", p, fi.Mode())
		}
	}
}

// TestInstallRefusesLooseBinDir pins that a bin directory anyone but root
// could change is refused before install writes or creates anything.
func TestInstallRefusesLooseBinDir(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := false
	in := testInstaller(&events, init, &userThere)
	in.binDirOK = func(string) error { return errors.New("/opt is writable by group 50") }
	err := in.Install(true)
	if err == nil || !strings.Contains(err.Error(), "refusing to install to /usr/local/bin/remote-mic") {
		t.Fatalf("Install: got %v, want a refusal naming the bin path", err)
	}
	if len(events) != 0 {
		t.Errorf("events %q, want nothing done before the refusal", events)
	}
}

// TestCheckBinDir pins that a missing bin directory is judged by its deepest
// existing ancestor, and that a directory not owned by root is refused.
func TestCheckBinDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // owned by the test user, not root
	if os.Getuid() == 0 {
		t.Skip("running as root: the temp dir is root-owned")
	}
	// A missing dir under a root-only ancestor is judged by that ancestor.
	if update.CheckRootOnly("/") == nil {
		if err := checkBinDir("/remote-mic-missing-" + filepath.Base(dir) + "/bin"); err != nil {
			t.Errorf("checkBinDir under /: %v, want nil", err)
		}
	}
	err := checkBinDir(filepath.Join(dir, "missing", "bin"))
	if err == nil || errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "not root") {
		t.Errorf("checkBinDir under a user-owned dir: got %v, want a refusal of an existing ancestor not owned by root", err)
	}
}

// TestInstallWarnsWhenReplacingLink pins that an install over a symlinked bin
// path says the link was replaced, since an operator who linked it on purpose
// would otherwise not learn that the target no longer runs.
func TestInstallWarnsWhenReplacingLink(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.isLink = func(p string) bool { return p == "/usr/local/bin/remote-mic" }
	var warn strings.Builder
	in.warn = &warn
	if err := in.Install(false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := warn.String(); !strings.Contains(got, "/usr/local/bin/remote-mic was a symlink") {
		t.Errorf("warning %q, want it to say the symlink was replaced", got)
	}
}

func TestIsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{file: false, link: true, filepath.Join(dir, "missing"): false} {
		if got := isSymlink(p); got != want {
			t.Errorf("isSymlink(%s) = %t, want %t", filepath.Base(p), got, want)
		}
	}
}

// TestBinDirUnderAFile pins that a bin path through a regular file is an
// error, not a missing directory to walk past or create.
func TestBinDirUnderAFile(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(file, "bin")
	if err := checkBinDir(bin); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("checkBinDir: got %v, want ENOTDIR", err)
	}
	if err := ensureBinDir(bin); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("ensureBinDir: got %v, want ENOTDIR", err)
	}
}
