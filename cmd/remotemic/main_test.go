//go:build linux

package main

import (
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/service"
)

// TestMain keeps every test off the host's installed unit: a test run on an
// installed appliance would otherwise resolve configs to /etc/remote-mic and
// take service defaults from it. Tests that exercise the unit stub
// installedConfig or installedSpec themselves. It also keeps them off the
// sandbox of the process running them: a runner unit with PrivateTmp=true
// would otherwise add the hidden-path note to every failed file open under
// t.TempDir(). Tests of that note stub sandboxHiddenDir themselves.
func TestMain(m *testing.M) {
	// A runner that systemd starts (a CI runner service) passes INVOCATION_ID
	// down, which makes serve behave as a unit's start; a set
	// REMOTEMIC_CONFIG would override every resolution. Tests set either
	// themselves when they need it.
	_ = os.Unsetenv("INVOCATION_ID")
	_ = os.Unsetenv(configEnv)
	installedConfig = func() (string, string, error) { return "", "", nil }
	installedSpec = func() (service.ServiceSpec, error) { return service.ServiceSpec{}, nil }
	prodSandboxHiddenDir = sandboxHiddenDir
	sandboxHiddenDir = func(string) (string, bool) { return "", false }
	m.Run()
}

// prodSandboxHiddenDir is the sandboxHiddenDir the binary runs with, kept by
// TestMain before it stubs the seam.
var prodSandboxHiddenDir func(string) (string, bool)

// TestSandboxHiddenDirSeamReadsTheMountTable pins the seam's production value:
// the hidden-path note follows this process's mount table, not the lexical
// prediction service.HiddenDir makes for the released units, which has the
// same signature.
func TestSandboxHiddenDirSeamReadsTheMountTable(t *testing.T) {
	t.Parallel()
	got, want := reflect.ValueOf(prodSandboxHiddenDir).Pointer(), reflect.ValueOf(service.SandboxHiddenDir).Pointer()
	if got != want {
		t.Errorf("sandboxHiddenDir is %s, want service.SandboxHiddenDir", runtime.FuncForPC(got).Name())
	}
}
