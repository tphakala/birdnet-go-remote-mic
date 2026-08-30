//go:build linux

package audio

import (
	"errors"
	"testing"

	capture "github.com/tphakala/go-audio-capture"
)

func TestHardwareNamesSuccess(t *testing.T) {
	prev := enumerateDevices
	enumerateDevices = func() ([]capture.DeviceInfo, error) {
		return []capture.DeviceInfo{{ID: testDevID, Name: "Foo Card, USB Audio"}}, nil
	}
	defer func() { enumerateDevices = prev }()

	got, err := HardwareNames()
	if err != nil {
		t.Fatalf("HardwareNames: %v", err)
	}
	if got[testDevID] != "Foo Card" {
		t.Errorf("names[%q] = %q, want %q", testDevID, got[testDevID], "Foo Card")
	}
}

func TestHardwareNamesPropagatesError(t *testing.T) {
	prev := enumerateDevices
	enumerateDevices = func() ([]capture.DeviceInfo, error) { return nil, errors.New("enumerate failed") }
	defer func() { enumerateDevices = prev }()

	if _, err := HardwareNames(); err == nil {
		t.Error("HardwareNames should propagate the enumeration error")
	}
}

func TestHardwareNamesFrom(t *testing.T) {
	t.Parallel()
	devs := []capture.DeviceInfo{
		{ID: testDevID, Name: "Scarlett 2i2 USB, USB Audio"},
		{ID: "hw:2,0", Name: testCardName},
	}
	got := hardwareNamesFrom(devs)
	want := map[string]string{
		testDevID: "Scarlett 2i2 USB",
		"hw:2,0":  testCardName,
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for id, label := range want {
		if got[id] != label {
			t.Errorf("names[%q] = %q, want %q", id, got[id], label)
		}
	}
}

func swapSupportedRates(fn func(string, int, capture.Format) (capture.RateSupport, error)) func() {
	prev := supportedRatesFn
	supportedRatesFn = fn
	return func() { supportedRatesFn = prev }
}

func TestProbeRatesUnionOfFormatsFilteredToCandidates(t *testing.T) {
	// S16 offers 48 kHz; S32 additionally offers 96 kHz and 192 kHz. The union,
	// intersected with candidates (which omit 192 kHz), yields 48 and 96 kHz in
	// candidate order.
	restore := swapSupportedRates(func(_ string, _ int, f capture.Format) (capture.RateSupport, error) {
		switch f {
		case capture.FormatS16LE:
			return capture.RateSupport{Rates: []int{48000}}, nil
		default:
			return capture.RateSupport{Rates: []int{96000, 192000}}, nil
		}
	})
	defer restore()

	got := ProbeRates(testDevID, 1, []int{16000, 48000, 96000, 384000})
	want := []int{48000, 96000}
	if len(got) != len(want) {
		t.Fatalf("ProbeRates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ProbeRates = %v, want %v", got, want)
		}
	}
}

func TestProbeRatesS32OnlyDevice(t *testing.T) {
	// An S32-only device (e.g. the ZOOM AMS-24) rejects S16 with *BadFormatError;
	// its S32 rates must still be reported so the UI is not empty for it.
	restore := swapSupportedRates(func(_ string, ch int, f capture.Format) (capture.RateSupport, error) {
		if f == capture.FormatS16LE {
			return capture.RateSupport{}, &capture.BadFormatError{Channels: ch, Format: f}
		}
		return capture.RateSupport{Rates: []int{44100, 48000, 96000}}, nil
	})
	defer restore()

	got := ProbeRates(testDevID, 2, []int{16000, 44100, 48000, 96000, 384000})
	want := []int{44100, 48000, 96000}
	if len(got) != len(want) {
		t.Fatalf("ProbeRates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ProbeRates = %v, want %v", got, want)
		}
	}
}

func TestProbeRatesFallsBackWhenDeviceUnavailable(t *testing.T) {
	// Both format queries fail (device busy or gone): report nil so the caller
	// falls back to the static rate list.
	restore := swapSupportedRates(func(string, int, capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{}, capture.ErrDeviceInUse
	})
	defer restore()

	if got := ProbeRates("hw:9,0", 1, []int{48000, 96000}); got != nil {
		t.Errorf("ProbeRates = %v, want nil fallback on unavailable device", got)
	}
}

func TestProbeRatesNilWhenNoCandidateMatches(t *testing.T) {
	// The device is reachable but supports only rates outside the candidate set:
	// return nil so the UI falls back rather than showing an empty dropdown.
	restore := swapSupportedRates(func(string, int, capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{Rates: []int{8000, 11025}}, nil
	})
	defer restore()

	if got := ProbeRates(testDevID, 1, []int{16000, 48000, 96000}); got != nil {
		t.Errorf("ProbeRates = %v, want nil when no candidate is supported", got)
	}
}
