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

// swapResolveOpenChannels substitutes the channel-resolution seam and returns a
// restore func, so a test can drive the per-attempt resolution without probing
// real hardware.
func swapResolveOpenChannels(fn func(string, []int) int) func() {
	prev := resolveOpenChannels
	resolveOpenChannels = fn
	return func() { resolveOpenChannels = prev }
}

// TestOpenDeviceRetrySkipsBusyDevice verifies the non-blocking busy gate: a
// device that stays busy is reported as ErrDeviceInUse without ever attempting
// the blocking capture open, so a contended device does not stall the
// capture-open phase (#19). If the gate did not fire, openDevice would
// run against nonexistent hardware and return a different (open) error.
func TestOpenDeviceRetrySkipsBusyDevice(t *testing.T) {
	defer swapResolveOpenChannels(func(string, []int) int { return 1 })()
	restore := swapDeviceInUse(func(string, int) bool { return true })
	defer restore()

	dev := &config.Device{Name: "busy", Device: devMissing, Channels: []int{1}, Rate: 48000, Format: testFmtS16}
	_, err := openDeviceRetry(dev, levels.NewHub())
	if !errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("openDeviceRetry error = %v, want ErrDeviceInUse", err)
	}
}

// TestOpenDeviceRetryPassesGateWhenFree verifies the gate is transparent when the
// device is not busy: the retry proceeds to the real open (which fails here
// against nonexistent hardware), so the surfaced error is the open error, not the
// busy sentinel. This guards against the gate falsely skipping a free device.
func TestOpenDeviceRetryPassesGateWhenFree(t *testing.T) {
	defer swapResolveOpenChannels(func(string, []int) int { return 1 })()
	restore := swapDeviceInUse(func(string, int) bool { return false })
	defer restore()

	dev := &config.Device{Name: "free", Device: devMissing, Channels: []int{1}, Rate: 48000, Format: testFmtS16}
	_, err := openDeviceRetry(dev, levels.NewHub())
	if err == nil {
		t.Fatal("openDeviceRetry on nonexistent hardware should fail")
	}
	if errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("openDeviceRetry error = %v, want a non-busy open error when the gate reports free", err)
	}
}

// TestOpenDeviceRetryGatesOnResolvedOpenCount verifies the busy gate probes at
// the count the resolver returns for this attempt, so the gate and the open
// within one attempt agree.
func TestOpenDeviceRetryGatesOnResolvedOpenCount(t *testing.T) {
	defer swapResolveOpenChannels(func(string, []int) int { return 2 })()
	var seen []int
	restore := swapDeviceInUse(func(_ string, count int) bool {
		seen = append(seen, count)
		return true
	})
	defer restore()

	dev := &config.Device{Name: "stereo", Device: devMissing, Channels: []int{2}, Rate: 48000, Format: testFmtS16}
	if _, err := openDeviceRetry(dev, levels.NewHub()); !errors.Is(err, capture.ErrDeviceInUse) {
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

// TestOpenDeviceRetryReresolvesAfterCardFrees is the closure test for the
// per-attempt resolution regression. The card is transiently busy at the first
// probe, so the resolver falls back to max(selection)=1 (a count a stereo-only
// card cannot open); once it frees, the resolver reports the true count 2. Because
// the resolution happens per attempt rather than once up front, the later attempts
// gate and open at 2, so the device is not permanently skipped. Pre-fix, the count
// was pinned from the single up-front resolution, so a card freed between attempts
// stayed stuck at the wrong count and the device was skipped.
func TestOpenDeviceRetryReresolvesAfterCardFrees(t *testing.T) {
	calls := 0
	defer swapResolveOpenChannels(func(string, []int) int {
		calls++
		if calls == 1 {
			return 1 // busy-window fallback: max(selection)
		}
		return 2 // the card has freed; the real count
	})()

	var seen []int
	restore := swapDeviceInUse(func(_ string, count int) bool {
		seen = append(seen, count)
		return count == 1 // busy only at the wrong fallback count
	})
	defer restore()

	dev := &config.Device{Name: "stereo", Device: devMissing, Channels: []int{1, 2}, Rate: 48000, Format: testFmtS16}
	_, err := openDeviceRetry(dev, levels.NewHub())
	// Once the count corrects to 2 the gate reports free and the open is attempted
	// at 2; it fails here only because devMissing is not real hardware. The point
	// is the device is no longer skipped as busy.
	if errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("device stayed skipped as busy after the card freed: %v", err)
	}
	if len(seen) < 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("busy gate counts = %v, want the fallback 1 then the corrected 2", seen)
	}
}
