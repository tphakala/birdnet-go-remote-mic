//go:build linux

package main

import (
	"errors"
	"testing"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
)

// devMissing is an ALSA id no test host exposes, so a real open fails fast.
const devMissing = "hw:9,9"

// swapDeviceInUse substitutes the busy-check seam and returns a restore func.
func swapDeviceInUse(fn func(string, int) bool) func() {
	prev := deviceInUse
	deviceInUse = fn
	return func() { deviceInUse = prev }
}

// TestOpenDeviceRetrySkipsBusyDevice verifies the non-blocking busy gate: a
// device that stays busy is reported as ErrDeviceInUse without ever attempting
// the blocking capture open, so a contended device does not stall the
// capture-open phase (Forgejo #19). If the gate did not fire, openDevice would
// run against nonexistent hardware and return a different (open) error.
func TestOpenDeviceRetrySkipsBusyDevice(t *testing.T) {
	restore := swapDeviceInUse(func(string, int) bool { return true })
	defer restore()

	dev := &config.Device{Name: "busy", Device: devMissing, Channels: []int{1}, Rate: 48000, Format: "s16"}
	_, err := openDeviceRetry(dev, 1, levels.NewHub())
	if !errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("openDeviceRetry error = %v, want ErrDeviceInUse", err)
	}
}

// TestOpenDeviceRetryPassesGateWhenFree verifies the gate is transparent when the
// device is not busy: the retry proceeds to the real open (which fails here
// against nonexistent hardware), so the surfaced error is the open error, not the
// busy sentinel. This guards against the gate falsely skipping a free device.
func TestOpenDeviceRetryPassesGateWhenFree(t *testing.T) {
	restore := swapDeviceInUse(func(string, int) bool { return false })
	defer restore()

	dev := &config.Device{Name: "free", Device: devMissing, Channels: []int{1}, Rate: 48000, Format: "s16"}
	_, err := openDeviceRetry(dev, 1, levels.NewHub())
	if err == nil {
		t.Fatal("openDeviceRetry on nonexistent hardware should fail")
	}
	if errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("openDeviceRetry error = %v, want a non-busy open error when the gate reports free", err)
	}
}

// TestOpenDeviceRetryGatesOnResolvedOpenCount verifies the busy gate probes at
// the channel count the caller resolved once (and will open at), rather than
// re-resolving it per attempt: the count reaching the seam is the one passed in.
func TestOpenDeviceRetryGatesOnResolvedOpenCount(t *testing.T) {
	var seen []int
	restore := swapDeviceInUse(func(_ string, count int) bool {
		seen = append(seen, count)
		return true
	})
	defer restore()

	dev := &config.Device{Name: "stereo", Device: devMissing, Channels: []int{2}, Rate: 48000, Format: "s16"}
	if _, err := openDeviceRetry(dev, 2, levels.NewHub()); !errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("openDeviceRetry error = %v, want ErrDeviceInUse", err)
	}
	if len(seen) == 0 {
		t.Fatal("the busy gate was never consulted")
	}
	for _, c := range seen {
		if c != 2 {
			t.Fatalf("busy gate probed at %d channels, want the resolved count 2", c)
		}
	}
}
