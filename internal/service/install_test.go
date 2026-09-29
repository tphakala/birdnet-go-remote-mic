//go:build linux

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
		trustedBin: func(string) error { return nil },
		lockBin:    func(string, func()) (func(), error) { return func() {}, nil },
		binVersion: func(string) (string, bool, error) { return "", false, nil },
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
		"mkbindir /usr/local/bin",
		evGroupadd,
		"run useradd --system --no-create-home --shell /usr/sbin/nologin --gid remote-mic remote-mic",
		"run usermod --append --groups audio remote-mic",
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
		"mkbindir /usr/local/bin",
		evGroupadd,
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

// TestInstallDowngradeGuard pins that install refuses to replace a newer
// installed binary before it writes anything, and that every case that
// cannot be compared or run goes ahead as the repair path. Removing the
// guard makes the newer-installed cases fail.
func TestInstallDowngradeGuard(t *testing.T) {
	t.Parallel()
	const v030, v040 = "v0.3.0", "v0.4.0"
	tests := []struct {
		name      string
		installed string
		present   bool
		runErr    error
		running   string
		allow     bool
		untrusted error  // trustedBin's verdict; nil means a root-only regular file
		wantErr   string // substring; "" means the install proceeds
		wantWarn  string
	}{
		{name: "newer installed refuses", installed: v040, present: true, running: v030, wantErr: "is " + v040 + ", newer than this binary (" + v030 + ")"},
		{name: "newer installed prerelease refuses", installed: "v0.4.0-rc.1", present: true, running: v030, wantErr: "is v0.4.0-rc.1, newer than this binary"},
		{name: "newer installed with allow-downgrade proceeds", installed: v040, present: true, running: v030, allow: true},
		{name: "same version proceeds", installed: v030, present: true, running: v030},
		{name: "older installed proceeds", installed: "v0.2.0", present: true, running: v030},
		{name: "nothing installed proceeds", running: v030},
		{name: "unrunnable installed binary proceeds with a warning", present: true, runErr: errors.New("exec format error"), running: v030, wantWarn: "cannot tell the version"},
		{name: "development build proceeds with a warning", installed: v040, present: true, running: "dev", wantWarn: "cannot compare"},
		{name: "installed binary others can change is not run", installed: v040, present: true, running: v030, untrusted: errors.New("owned by uid 1000"), wantWarn: "not running the installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var events []string
			userThere := true
			in := testInstaller(&events, &fakeInit{events: &events, present: true}, &userThere)
			in.Version, in.AllowDowngrade = tt.running, tt.allow
			in.trustedBin = func(string) error { return tt.untrusted }
			in.binVersion = func(path string) (string, bool, error) {
				if tt.untrusted != nil {
					t.Error("ran the installed binary although it is not root-only")
				}
				if path != DefaultBinPath {
					t.Errorf("version read from %q, want %q", path, DefaultBinPath)
				}
				return tt.installed, tt.present, tt.runErr
			}
			var warn strings.Builder
			in.warn = &warn
			err := in.Install(true)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "--allow-downgrade") || !strings.Contains(err.Error(), "sudo "+DefaultBinPath+" service install") {
					t.Fatalf("Install error = %v, want one containing %q, the installed binary's command and --allow-downgrade", err, tt.wantErr)
				}
				for _, e := range events {
					if strings.HasPrefix(e, "copy ") || strings.HasPrefix(e, "write ") || strings.HasPrefix(e, "run ") {
						t.Errorf("refused install still did %q", e)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if !slices.Contains(events, "copy /home/pi/remote-mic -> /usr/local/bin/remote-mic") {
				t.Errorf("install did not copy the binary: %v", events)
			}
			if tt.wantWarn == "" && warn.Len() != 0 {
				t.Errorf("unexpected warning %q", warn.String())
			}
			if tt.wantWarn != "" && !strings.Contains(warn.String(), tt.wantWarn) {
				t.Errorf("warning %q, want it to contain %q", warn.String(), tt.wantWarn)
			}
		})
	}
}

// TestInstalledVersion pins the parse of `remote-mic version` output and the
// absent-binary case, against a real script standing in for the binary.
func TestInstalledVersion(t *testing.T) {
	t.Parallel()
	script := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "remote-mic")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // a test script must be executable
			t.Fatal(err)
		}
		return p
	}
	if v, present, err := installedVersion(script(t, "echo remote-mic v0.4.0\necho more\n")); v != "v0.4.0" || !present || err != nil {
		t.Errorf("got %q, %t, %v; want v0.4.0, true, nil", v, present, err)
	}
	if _, present, err := installedVersion(script(t, "echo something else\n")); !present || err == nil {
		t.Errorf("unexpected output: present=%t err=%v, want present and an error", present, err)
	}
	if _, present, err := installedVersion(script(t, "exit 3\n")); !present || err == nil {
		t.Errorf("failing binary: present=%t err=%v, want present and an error", present, err)
	}
	if _, present, _ := installedVersion(filepath.Join(t.TempDir(), "absent")); present {
		t.Error("an absent binary reported as present")
	}
	// A path that cannot be looked at (a directory component is a file) is
	// not "absent": the guard must report it, not skip it.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := installedVersion(filepath.Join(file, "remote-mic")); !present || err == nil {
		t.Errorf("unreadable path: present=%t err=%v, want present and an error", present, err)
	}
}

// TestNewInstallerReadsInstalledVersion pins that the production installer
// reads the installed binary's version, so the downgrade guard is live.
func TestNewInstallerReadsInstalledVersion(t *testing.T) {
	t.Parallel()
	if NewInstaller(ServiceSpec{}).binVersion == nil {
		t.Error("NewInstaller left binVersion unset; the downgrade guard would panic or be skipped")
	}
}

// TestNewInstallerWiresTheTrustedBinaryCheck pins that the production installer
// has the root-only check before it runs the installed binary: a nil func would
// panic on every real install, and a no-op would let root run any file at the
// bin path. Only a file root owns passes, and a test's temporary file is not
// root's.
func TestNewInstallerWiresTheTrustedBinaryCheck(t *testing.T) {
	t.Parallel()
	in := NewInstaller(ServiceSpec{})
	if in.trustedBin == nil {
		t.Fatal("NewInstaller left trustedBin unset; checkDowngrade would panic")
	}
	if os.Geteuid() == 0 {
		t.Skip("root owns the temporary file, so it would pass the check")
	}
	f := filepath.Join(t.TempDir(), "remote-mic")
	if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := in.trustedBin(f); err == nil {
		t.Error("trustedBin accepted a file root does not own")
	}
	if in.lockBin == nil {
		t.Error("NewInstaller left lockBin unset; install would not serialize with the updater")
	}
}

// TestInstallHoldsTheLockAcrossTheDowngradeCheckAndCopy pins the order the
// lock exists for: taken before the installed version is read, released only
// after the copy.
func TestInstallHoldsTheLockAcrossTheDowngradeCheckAndCopy(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.lockBin = func(p string, _ func()) (func(), error) {
		events = append(events, "lock "+p)
		return func() { events = append(events, "unlock "+p) }, nil
	}
	in.binVersion = func(p string) (string, bool, error) {
		events = append(events, "version "+p)
		return "", false, nil
	}
	if err := in.Install(false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	idx := func(want string) int {
		for i, e := range events {
			if e == want {
				return i
			}
		}
		t.Fatalf("event %q missing from %v", want, events)
		return -1
	}
	lock, version, cp, unlock := idx("lock /usr/local/bin/remote-mic"), idx("version /usr/local/bin/remote-mic"), idx("copy /home/pi/remote-mic -> /usr/local/bin/remote-mic"), idx("unlock /usr/local/bin/remote-mic")
	if lock >= version || version >= cp || cp >= unlock {
		t.Errorf("want lock < version < copy < unlock, got %d %d %d %d in %v", lock, version, cp, unlock, events)
	}
}

// TestInstallStopsWhenTheLockIsBusy pins that install writes nothing when an
// update holds the binary's lock past the wait, and says to try again.
func TestInstallStopsWhenTheLockIsBusy(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.lockBin = func(string, func()) (func(), error) { return nil, update.ErrBinBusy }
	err := in.Install(true)
	if !errors.Is(err, update.ErrBinBusy) || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("Install: got %v, want ErrBinBusy with a retry hint", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "copy ") || strings.HasPrefix(e, "write ") || strings.HasPrefix(e, "run ") {
			t.Errorf("install acted without the lock: %q", e)
		}
	}
}

// TestInstallSaysWhenItWaitsForAnUpdate pins that a busy lock is reported to
// the operator, since the wait can last minutes.
func TestInstallSaysWhenItWaitsForAnUpdate(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	var warn strings.Builder
	in.warn = &warn
	in.lockBin = func(_ string, waiting func()) (func(), error) {
		waiting()
		return func() {}, nil
	}
	if err := in.Install(false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := warn.String(); !strings.Contains(got, "waiting for an update of /usr/local/bin/remote-mic") {
		t.Errorf("warning %q, want it to say install is waiting", got)
	}
}

// TestInstallStopsWhenTheBinDirCannotBeCreated pins that the lock file's
// directory is made before anything else is written.
func TestInstallStopsWhenTheBinDirCannotBeCreated(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.makeBinDir = func(string) error { return errors.New("read-only file system") }
	if err := in.Install(true); err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("Install: got %v, want the mkdir error", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "copy ") || strings.HasPrefix(e, "write ") || strings.HasPrefix(e, "run ") {
			t.Errorf("install acted after the bin dir failed: %q", e)
		}
	}
}

// TestProductionLockBinTakesTheUpdaterLock pins that the installer's lock is
// the one the updater takes: while it is held, update.LockBin finds it busy.
func TestProductionLockBinTakesTheUpdaterLock(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "remote-mic")
	release, err := lockBin(bin, nil)
	if err != nil {
		t.Fatalf("lockBin: %v", err)
	}
	if _, err := update.LockBin(t.Context(), bin, 0, nil); !errors.Is(err, update.ErrBinBusy) {
		t.Errorf("updater lock while install holds it: got %v, want ErrBinBusy", err)
	}
	release()
}

// TestInstallDoesNotSuggestRetryingAfterALockError pins that only a busy lock
// gets the retry hint: any other failure to take it (a link where the lock
// file belongs, a read-only directory) fails the same way next time.
func TestInstallDoesNotSuggestRetryingAfterALockError(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.lockBin = func(string, func()) (func(), error) {
		return nil, errors.New("open /usr/local/bin/remote-mic.lock: too many levels of symbolic links")
	}
	err := in.Install(true)
	if err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("Install: got %v, want the lock error", err)
	}
	if strings.Contains(err.Error(), "try again") {
		t.Errorf("error %q suggests retrying a failure that is not transient", err)
	}
}
