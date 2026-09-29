//go:build linux

package main

import (
	"os"
	"path/filepath"
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
	inst := detectInstall(dir)
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
