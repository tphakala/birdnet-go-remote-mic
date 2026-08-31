//go:build linux

package audio

import (
	"errors"
	"slices"
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

func TestProbeChannelsFlexibleDevice(t *testing.T) {
	// A device that accepts both mono and stereo (e.g. an ALSA Loopback): every
	// channel/format query succeeds, so both counts are reported.
	restore := swapSupportedRates(func(_ string, _ int, _ capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{Rates: []int{48000}}, nil
	})
	defer restore()

	if got := ProbeChannels(testDevID, []int{1, 2}); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("ProbeChannels = %v, want [1 2]", got)
	}
}

func TestProbeChannelsMonoOnly(t *testing.T) {
	// A mono-only device (e.g. the 384 kHz AudioMoth): mono probes succeed, stereo
	// is rejected as *BadFormatError at every format, so only mono is reported.
	restore := swapSupportedRates(func(_ string, ch int, f capture.Format) (capture.RateSupport, error) {
		if ch == 1 {
			return capture.RateSupport{Rates: []int{384000}}, nil
		}
		return capture.RateSupport{}, &capture.BadFormatError{Channels: ch, Format: f}
	})
	defer restore()

	if got := ProbeChannels(testDevID, []int{1, 2}); !slices.Equal(got, []int{1}) {
		t.Errorf("ProbeChannels = %v, want [1]", got)
	}
}

func TestProbeChannelsStereoOnly(t *testing.T) {
	// A stereo-only device (e.g. the ZOOM AMS-24): mono is rejected, stereo is
	// accepted (here only in S32, mirroring a 24/32-bit-only interface).
	restore := swapSupportedRates(func(_ string, ch int, f capture.Format) (capture.RateSupport, error) {
		if ch == 2 && f == capture.FormatS32LE {
			return capture.RateSupport{Rates: []int{48000, 96000}}, nil
		}
		return capture.RateSupport{}, &capture.BadFormatError{Channels: ch, Format: f}
	})
	defer restore()

	if got := ProbeChannels(testDevID, []int{1, 2}); !slices.Equal(got, []int{2}) {
		t.Errorf("ProbeChannels = %v, want [2]", got)
	}
}

func TestProbeChannelsSupportedAtNonStandardRate(t *testing.T) {
	// A channel/format combo the hardware supports only at a rate outside the
	// standard probe set: SupportedRates returns nil error with an empty rate
	// slice. ProbeChannels keys on the nil error, so the channel count is still
	// reported (it is not coupled to the discrete rate probes).
	restore := swapSupportedRates(func(_ string, _ int, _ capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{Rates: nil}, nil
	})
	defer restore()

	if got := ProbeChannels(testDevID, []int{1, 2}); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("ProbeChannels = %v, want [1 2] (nil error means the count is supported)", got)
	}
}

func TestProbeChannelsNilWhenUnavailable(t *testing.T) {
	// Device busy or gone at every query: report nil so the UI falls back to the
	// static [1, 2] list rather than a misleading empty set.
	restore := swapSupportedRates(func(string, int, capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{}, capture.ErrDeviceInUse
	})
	defer restore()

	if got := ProbeChannels("hw:9,0", []int{1, 2}); got != nil {
		t.Errorf("ProbeChannels = %v, want nil on unavailable device", got)
	}
}

func TestDeviceInUseWhenBusy(t *testing.T) {
	// Every format query fails with ErrDeviceInUse (the O_NONBLOCK open hit EBUSY):
	// the device is held exclusively.
	restore := swapSupportedRates(func(string, int, capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{}, capture.ErrDeviceInUse
	})
	defer restore()

	if !DeviceInUse(testDevID, 1) {
		t.Error("DeviceInUse = false, want true when every query reports ErrDeviceInUse")
	}
}

func TestDeviceInUseFalseWhenOpenable(t *testing.T) {
	// A successful query proves the O_NONBLOCK open worked, so the device is not
	// held exclusively.
	restore := swapSupportedRates(func(string, int, capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{Rates: []int{48000}}, nil
	})
	defer restore()

	if DeviceInUse(testDevID, 1) {
		t.Error("DeviceInUse = true, want false when the device opens")
	}
}

func TestDeviceInUseFalseOnBadFormat(t *testing.T) {
	// *BadFormatError is only reachable after a successful O_NONBLOCK open (the
	// refine rejects the format, not the open), so the device is not busy; let the
	// real open decide.
	restore := swapSupportedRates(func(_ string, ch int, f capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{}, &capture.BadFormatError{Channels: ch, Format: f}
	})
	defer restore()

	if DeviceInUse(testDevID, 2) {
		t.Error("DeviceInUse = true, want false on *BadFormatError (device is openable)")
	}
}

func TestDeviceInUseFalseWhenGone(t *testing.T) {
	// A missing device is not held exclusively; the real open reports it gone.
	restore := swapSupportedRates(func(string, int, capture.Format) (capture.RateSupport, error) {
		return capture.RateSupport{}, capture.ErrDeviceGone
	})
	defer restore()

	if DeviceInUse("hw:9,0", 1) {
		t.Error("DeviceInUse = true, want false when the device is gone")
	}
}

func TestDeviceInUseFalseWhenAnyFormatOpens(t *testing.T) {
	// One format reports busy, another opens: not held exclusively. This pins the
	// loop's accumulation semantics (a later non-busy result wins over an earlier
	// ErrDeviceInUse), distinguishing "busy = every format EBUSY" from "busy = any
	// format EBUSY". In practice EBUSY is uniform (raised at open, before the
	// per-format refine), so this mixed case is defensive.
	restore := swapSupportedRates(func(_ string, _ int, f capture.Format) (capture.RateSupport, error) {
		if f == capture.FormatS16LE {
			return capture.RateSupport{}, capture.ErrDeviceInUse
		}
		return capture.RateSupport{Rates: []int{48000}}, nil
	})
	defer restore()

	if DeviceInUse(testDevID, 1) {
		t.Error("DeviceInUse = true, want false when at least one format opens")
	}
}
