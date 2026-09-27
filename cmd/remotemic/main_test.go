//go:build linux

package main

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/service"
)

// TestMain keeps every test off the host's installed unit: a test run on an
// installed appliance would otherwise resolve configs to /etc/remote-mic and
// take service defaults from it. Tests that exercise the unit stub
// installedConfig or installedSpec themselves.
func TestMain(m *testing.M) {
	installedConfig = func() (string, string, error) { return "", "", nil }
	installedSpec = func() (service.ServiceSpec, error) { return service.ServiceSpec{}, nil }
	m.Run()
}
