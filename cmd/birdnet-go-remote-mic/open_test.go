//go:build linux

package main

import (
	"errors"
	"testing"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
)

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

	dev := &config.Device{Name: "busy", Device: "hw:9,9", Channels: []int{1}, Rate: 48000, Format: "s16"}
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
	restore := swapDeviceInUse(func(string, int) bool { return false })
	defer restore()

	dev := &config.Device{Name: "free", Device: "hw:9,9", Channels: []int{1}, Rate: 48000, Format: "s16"}
	_, err := openDeviceRetry(dev, levels.NewHub())
	if err == nil {
		t.Fatal("openDeviceRetry on nonexistent hardware should fail")
	}
	if errors.Is(err, capture.ErrDeviceInUse) {
		t.Fatalf("openDeviceRetry error = %v, want a non-busy open error when the gate reports free", err)
	}
}
