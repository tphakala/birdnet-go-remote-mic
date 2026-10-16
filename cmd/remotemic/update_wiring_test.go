//go:build linux

package main

import (
	"bytes"
	"errors"
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
	origExe, origPaths := runningExe, installedBinPaths
	t.Cleanup(func() { runningExe, installedBinPaths = origExe, origPaths })
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
	origExe, origPaths := runningExe, installedBinPaths
	t.Cleanup(func() { runningExe, installedBinPaths = origExe, origPaths })
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
