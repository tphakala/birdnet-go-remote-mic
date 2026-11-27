//go:build linux

package main

import (
	"os"
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
	sandboxHiddenDir = func(string) (string, bool) { return "", false }
	m.Run()
}
