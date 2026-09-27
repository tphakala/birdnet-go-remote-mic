//go:build linux

package main

import "testing"

// TestMain keeps every test off the host's installed unit: a test run on an
// installed appliance would otherwise resolve configs to /etc/remote-mic. Tests
// that exercise the unit source stub installedConfig themselves.
func TestMain(m *testing.M) {
	installedConfig = func() (string, string, error) { return "", "", nil }
	m.Run()
}
