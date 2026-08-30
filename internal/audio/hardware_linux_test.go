//go:build linux

package audio

import (
	"errors"
	"testing"

	capture "github.com/tphakala/go-audio-capture"
)

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

func TestProbeRatesKeepsSupported(t *testing.T) {
	supported := map[int]bool{48000: true, 96000: true}
	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		if supported[cfg.Rate] {
			return &stubStream{neg: capture.Config{Rate: cfg.Rate, Channels: cfg.Channels}}, nil
		}
		return nil, &capture.BadRateError{Requested: cfg.Rate}
	}
	defer func() { openStream = prev }()

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

func TestProbeRatesDropsNegotiatedMismatch(t *testing.T) {
	// An open that succeeds but negotiates a different rate must not be counted.
	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		return &stubStream{neg: capture.Config{Rate: 48000, Channels: cfg.Channels}}, nil
	}
	defer func() { openStream = prev }()

	if got := ProbeRates(testDevID, 1, []int{96000}); len(got) != 0 {
		t.Errorf("ProbeRates kept a negotiated mismatch: %v", got)
	}
}

func TestProbeRatesFallsBackWhenDeviceUnavailable(t *testing.T) {
	prev := openStream
	openStream = func(capture.Config) (captureStream, error) {
		return nil, errors.New("device busy")
	}
	defer func() { openStream = prev }()

	if got := ProbeRates("hw:9,0", 1, []int{48000, 96000}); got != nil {
		t.Errorf("ProbeRates = %v, want nil fallback on unavailable device", got)
	}
}

func TestProbeRatesClosesEveryProbe(t *testing.T) {
	var opened []*stubStream
	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		s := &stubStream{neg: capture.Config{Rate: cfg.Rate, Channels: cfg.Channels}}
		opened = append(opened, s)
		return s, nil
	}
	defer func() { openStream = prev }()

	ProbeRates(testDevID, 1, []int{48000, 96000})
	for i, s := range opened {
		if !s.closed {
			t.Errorf("probe %d left the stream open", i)
		}
	}
}
