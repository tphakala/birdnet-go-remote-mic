//go:build linux

package main

import (
	"net"
	"testing"
)

// TestMultipathTCPDisabled pins the "godebug multipathtcp=0" default in
// go.mod. Only listeners default to Multipath TCP on Linux; a zero
// net.Dialer already uses plain TCP, so it is not checked here.
func TestMultipathTCPDisabled(t *testing.T) {
	// A GODEBUG value in the environment overrides the compiled default,
	// so clear it to test the default itself.
	t.Setenv("GODEBUG", "")

	var lc net.ListenConfig
	if lc.MultipathTCP() {
		t.Error("net.ListenConfig.MultipathTCP() = true, want false")
	}
}
