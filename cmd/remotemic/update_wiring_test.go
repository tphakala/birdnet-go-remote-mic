//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// TestDetectInstallChecksTheServiceBinaryIsRootOnly pins that detectInstall
// hands update.CheckRootOnlyFile to the install detection. A nil check refuses
// every service binary as unchecked, which switches the one-button update off
// for every service install.
func TestDetectInstallChecksTheServiceBinaryIsRootOnly(t *testing.T) {
	dir := t.TempDir()
	bin, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(bin, "remote-mic")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	origExe, origPaths, origState := runningExe, installedBinPaths, installedStateDir
	t.Cleanup(func() { runningExe, installedBinPaths, installedStateDir = origExe, origPaths, origState })
	installedStateDir = func() string { return "" }
	runningExe = func() (string, error) { return bin, nil }
	installedBinPaths = func() (string, string, error) { return bin, bin, nil }

	want := ""
	if err := update.CheckRootOnlyFile(bin); err != nil {
		want = err.Error()
	}
	inst := detectInstall(dir, t.Logf)
	if inst.Method != update.MethodService {
		t.Fatalf("method %q, want %q", inst.Method, update.MethodService)
	}
	if inst.ServiceBinUnsafe != want {
		t.Errorf("ServiceBinUnsafe %q, want %q (the result of update.CheckRootOnlyFile)", inst.ServiceBinUnsafe, want)
	}
	if inst.CanApply != (want == "") {
		t.Errorf("CanApply %t with ServiceBinUnsafe %q", inst.CanApply, inst.ServiceBinUnsafe)
	}
}

// TestInstallDetectorLogsEachProblemOnce pins that the detector, which the
// update manager calls again on every status read, logs a problem once and
// not once per read, and logs it again after it cleared and came back.
func TestInstallDetectorLogsEachProblemOnce(t *testing.T) {
	dir := t.TempDir()
	bin, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(bin, "remote-mic")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	origExe, origPaths, origState := runningExe, installedBinPaths, installedStateDir
	t.Cleanup(func() { runningExe, installedBinPaths, installedStateDir = origExe, origPaths, origState })
	installedStateDir = func() string { return "" }
	runningExe = func() (string, error) { return bin, nil }
	var unitErr error
	installedBinPaths = func() (string, string, error) { return bin, bin, unitErr }

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	d := &installDetector{dir: dir}
	const problem = "cannot read the installed units"
	step := func(name string, err error, want int) {
		t.Helper()
		unitErr = err
		buf.Reset()
		d.detect()
		if got := strings.Count(buf.String(), problem); got != want {
			t.Errorf("%s: %d log lines about the units, want %d:\n%s", name, got, want, buf.String())
		}
	}
	unreadable := errors.New("permission denied")
	step("first failure", unreadable, 1)
	step("same failure again", unreadable, 0)
	step("same failure a third time", unreadable, 0)
	step("cleared", nil, 0)
	step("failure returns", unreadable, 1)
}

// TestNewUpdateManagerReadsTheInstallationAgain pins that the manager
// newUpdateManager builds re-reads the installation on a press of Update: a
// service install run after startup shows in the status the press returns.
// It keys on Method, which does not depend on who owns the test binary.
func TestNewUpdateManagerReadsTheInstallationAgain(t *testing.T) {
	dir := t.TempDir()
	bin, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(bin, "remote-mic")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	origExe, origPaths, origState := runningExe, installedBinPaths, installedStateDir
	t.Cleanup(func() { runningExe, installedBinPaths, installedStateDir = origExe, origPaths, origState })
	installedStateDir = func() string { return "" }
	runningExe = func() (string, error) { return bin, nil }
	installedBinPaths = func() (string, string, error) { return "", "", nil }

	m := newUpdateManager(t.Context(), filepath.Join(dir, "update"), nil)
	if m == nil {
		t.Fatal("newUpdateManager returned nil")
	}
	if got := m.Status().Install.Method; got != update.MethodManual {
		t.Fatalf("method %q at startup, want %q", got, update.MethodManual)
	}
	installedBinPaths = func() (string, string, error) { return bin, bin, nil }
	st, _ := m.StartApply()
	if st.Install.Method != update.MethodService {
		t.Errorf("method %q after the install changed, want %q", st.Install.Method, update.MethodService)
	}
}

// stateDirFixture stubs the seams detectInstall reads so that a service
// install with a root-only binary is found, whoever owns the test's temp
// directory, and returns the unit's state directory through *unit.
func stateDirFixture(t *testing.T) (bin string, unit *string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(root, "remote-mic")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	origExe, origPaths, origState, origRoot := runningExe, installedBinPaths, installedStateDir, rootOnlyFile
	t.Cleanup(func() {
		runningExe, installedBinPaths, installedStateDir, rootOnlyFile = origExe, origPaths, origState, origRoot
	})
	runningExe = func() (string, error) { return bin, nil }
	installedBinPaths = func() (string, string, error) { return bin, bin, nil }
	rootOnlyFile = func(string) error { return nil }
	unit = new(string)
	installedStateDir = func() string { return *unit }
	return bin, unit
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDetectInstallComparesTheUpdaterStateDir pins that the one-button
// update is offered only while the staging directory this process uses is the
// one the installed root updater watches, and that a state directory moved by
// a later service install gets the restart advice, not the advice to re-run
// install.
func TestDetectInstallComparesTheUpdaterStateDir(t *testing.T) {
	_, unit := stateDirFixture(t)
	root := t.TempDir()
	mkdirs(t, filepath.Join(root, "state", "update"), filepath.Join(root, "old", "update"), filepath.Join(root, "new", "update"))
	if err := os.Symlink(filepath.Join(root, "state"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	join := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	for _, tc := range []struct {
		name        string
		unit, dir   string
		canApply    bool
		stagingHint bool // want the hint that tells the operator to create the staging directory
		wantIn      []string
		wantNotIn   []string
	}{
		{name: "same directory", unit: join("state"), dir: join("state", "update"), canApply: true},
		{name: "unit path through a symlink", unit: join("link"), dir: join("state", "update"), canApply: true},
		{name: "process path through a symlink", unit: join("state"), dir: join("link", "update"), canApply: true},
		{name: "unit with a trailing slash", unit: join("state") + "/", dir: join("state", "update"), canApply: true},
		{
			name: "state directory moved", unit: join("new"), dir: join("old", "update"),
			wantIn:    []string{update.RestartService, join("old", "update"), join("new", "update")},
			wantNotIn: []string{"service install"},
		},
		{
			name: "state directory moved and the old staging directory gone", unit: join("new"), dir: join("gone", "update"),
			wantIn:    []string{update.RestartService, join("gone", "update"), join("new", "update")},
			wantNotIn: []string{"service install", "To create"},
		},
		{name: "unit state directory unknown", unit: "", dir: join("old", "update"), canApply: true},
		{name: "unit state directory unknown and staging directory missing", unit: "", dir: join("gone", "update"), stagingHint: true},
		{name: "same directory but no staging directory", unit: join("gone"), dir: join("gone", "update"), stagingHint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*unit = tc.unit
			var logged []string
			inst := detectInstall(tc.dir, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
			if inst.Method != update.MethodService {
				t.Fatalf("method %q, want %q", inst.Method, update.MethodService)
			}
			if inst.CanApply != tc.canApply {
				t.Errorf("CanApply %t, want %t (hint %q)", inst.CanApply, tc.canApply, inst.Hint)
			}
			if tc.canApply && inst.Hint != "" {
				t.Errorf("hint %q with the button on", inst.Hint)
			}
			for _, w := range tc.wantIn {
				if !strings.Contains(inst.Hint, w) {
					t.Errorf("hint %q does not contain %q", inst.Hint, w)
				}
			}
			for _, w := range tc.wantNotIn {
				if strings.Contains(inst.Hint, w) {
					t.Errorf("hint %q contains %q", inst.Hint, w)
				}
			}
			if want := stagingDirHint(inst, tc.dir); tc.stagingHint && inst.Hint != want {
				t.Errorf("hint %q, want the staging directory hint %q", inst.Hint, want)
			}
		})
	}
}

// TestDetectInstallMatchesARelativeStagingDir pins that a staging directory
// given relative to the working directory (a hand run with a relative
// --cert-dir or config path) matches the absolute state directory in the
// unit.
func TestDetectInstallMatchesARelativeStagingDir(t *testing.T) {
	_, unit := stateDirFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(root, "state", "update"))
	t.Chdir(root)
	*unit = filepath.Join(root, "state")
	inst := detectInstall(filepath.Join("state", "update"), t.Logf)
	if !inst.CanApply {
		t.Errorf("CanApply false for a relative staging directory that is the unit's, hint %q", inst.Hint)
	}
}

// TestInstallDetectorLogsAStateDirMismatchOnce pins that a moved state
// directory is logged once while it lasts, and again if it returns after the
// directories matched.
func TestInstallDetectorLogsAStateDirMismatchOnce(t *testing.T) {
	_, unit := stateDirFixture(t)
	root := t.TempDir()
	mkdirs(t, filepath.Join(root, "old", "update"), filepath.Join(root, "new", "update"))

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	d := &installDetector{dir: filepath.Join(root, "old", "update")}
	const problem = "the root updater watches"
	step := func(name, stateDir string, want int) {
		t.Helper()
		*unit = stateDir
		buf.Reset()
		d.detect()
		if got := strings.Count(buf.String(), problem); got != want {
			t.Errorf("%s: %d log lines about the state directory, want %d:\n%s", name, got, want, buf.String())
		}
	}
	step("first mismatch", filepath.Join(root, "new"), 1)
	step("same mismatch again", filepath.Join(root, "new"), 0)
	step("match", filepath.Join(root, "old"), 0)
	step("mismatch returns", filepath.Join(root, "new"), 1)
}
