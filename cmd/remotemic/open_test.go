//go:build linux

package main

import (
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
)

// devMissing is an ALSA id no test host exposes, so a real open fails fast.
const devMissing = "hw:9,9"

// swapResolveOpenChannels substitutes the channel-resolution seam and returns a
// restore func, so a test can drive the per-attempt resolution without probing
// real hardware.
func swapResolveOpenChannels(fn func(string, []int) int) func() {
	prev := resolveOpenChannels
	resolveOpenChannels = fn
	return func() { resolveOpenChannels = prev }
}

// swapOpenCaptureAt substitutes the capture-open seam and returns a restore
// func, so a test can open a device without hardware.
func swapOpenCaptureAt(fn func(*config.Device, int) (audio.Source, capture.Format, error)) func() {
	prev := openCaptureAt
	openCaptureAt = fn
	return func() { openCaptureAt = prev }
}

// TestOpenDeviceRetryReportsBusyDevice pins that a device the capture open
// reports as held by another process (ErrDeviceInUse, which the library's
// O_NONBLOCK open returns at once) is retried the full budget, not treated as
// permanent, and surfaces an error that still matches ErrDeviceInUse so the
// device is reported skipped and retried on backoff.
func TestOpenDeviceRetryReportsBusyDevice(t *testing.T) {
	defer swapResolveOpenChannels(func(string, []int) int { return 1 })()
	calls := 0
	defer swapOpenCaptureAt(func(*config.Device, int) (audio.Source, capture.Format, error) {
		calls++
		return nil, 0, capture.ErrDeviceInUse
	})()

	dev := &config.Device{Name: "busy", Device: devMissing, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Channels: []int{1}}}}
	synctest.Test(t, func(t *testing.T) {
		_, err := openDeviceRetry(dev, levels.NewHub())
		if !errors.Is(err, capture.ErrDeviceInUse) {
			t.Fatalf("openDeviceRetry error = %v, want ErrDeviceInUse", err)
		}
		if !strings.Contains(err.Error(), "in use") {
			t.Errorf("openDeviceRetry error = %q, want text naming the device in use", err)
		}
	})
	if calls != 5 {
		t.Errorf("open attempts = %d, want 5 (a busy device is retried, not permanent)", calls)
	}
}

// TestOpenDeviceRetryOpensAtResolvedCount verifies every open attempt uses the
// count the resolver returns for that attempt.
func TestOpenDeviceRetryOpensAtResolvedCount(t *testing.T) {
	defer swapResolveOpenChannels(func(string, []int) int { return 2 })()
	var seen []int
	defer swapOpenCaptureAt(func(_ *config.Device, openCh int) (audio.Source, capture.Format, error) {
		seen = append(seen, openCh)
		return nil, 0, capture.ErrDeviceInUse
	})()

	dev := &config.Device{Name: "stereo", Device: devMissing, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Channels: []int{2}}}}
	synctest.Test(t, func(t *testing.T) {
		if _, err := openDeviceRetry(dev, levels.NewHub()); !errors.Is(err, capture.ErrDeviceInUse) {
			t.Fatalf("openDeviceRetry error = %v, want ErrDeviceInUse", err)
		}
	})
	if len(seen) == 0 {
		t.Fatal("the capture open was never attempted")
	}
	for _, c := range seen {
		if c != 2 {
			t.Fatalf("open attempted at %d channels, want the resolved count 2", c)
		}
	}
}

// TestOpenDeviceRetryReresolvesAfterCardFrees is the closure test for the
// per-attempt resolution regression. The card is transiently busy at the first
// probe, so the resolver falls back to max(selection)=1 (a count a stereo-only
// card cannot open); once it frees, the resolver reports the true count 2. Because
// the resolution happens per attempt rather than once up front, the later attempt
// opens at 2 and succeeds, so the device is not permanently skipped. Pre-fix, the
// count was pinned from the single up-front resolution, so a card freed between
// attempts stayed stuck at the wrong count and the device was skipped.
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
	defer swapOpenCaptureAt(func(_ *config.Device, openCh int) (audio.Source, capture.Format, error) {
		seen = append(seen, openCh)
		if openCh == 1 {
			return nil, 0, capture.ErrDeviceInUse // busy only at the wrong fallback count
		}
		return newOverrunCapture(0), capture.FormatS16LE, nil
	})()

	dev := &config.Device{Name: "stereo", Device: devMissing, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Path: "/stereo", Mode: config.ModePCM, Channels: []int{1, 2}}}}
	synctest.Test(t, func(t *testing.T) {
		rt, err := openDeviceRetry(dev, levels.NewHub())
		if err != nil {
			t.Fatalf("device stayed skipped after the card freed: %v", err)
		}
		_ = rt.fanout.Close()
	})
	if !slices.Equal(seen, []int{1, 2}) {
		t.Fatalf("open counts = %v, want the fallback 1 then the corrected 2", seen)
	}
}

// TestOpenDeviceRetryStopsOnPermanentError pins that an open failure that cannot
// change between attempts (here a malformed stable id the library rejects) is
// returned after one attempt instead of spending the retry budget.
func TestOpenDeviceRetryStopsOnPermanentError(t *testing.T) {
	attempts := 0
	defer swapResolveOpenChannels(func(string, []int) int { attempts++; return 1 })()

	dev := &config.Device{Name: "bad", Device: "usb:zz", Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Channels: []int{1}}}}
	_, err := openDeviceRetry(dev, levels.NewHub())
	if _, ok := errors.AsType[*capture.BadDeviceError](err); !ok {
		t.Fatalf("openDeviceRetry error = %v, want *capture.BadDeviceError", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 for a permanent error", attempts)
	}
}

func TestPermanentOpenError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&capture.DeviceNotFoundError{ID: "x"}, true},
		{&capture.AmbiguousDeviceError{ID: "x"}, true},
		{&capture.BadDeviceError{Value: "x"}, true},
		{&capture.ConfigError{Field: "rate", Reason: "unset"}, true},
		// A rate or format rejection is not permanent: openDeviceRetry re-resolves
		// the channel count each attempt, and the accepted rates and formats can
		// depend on it, so a later attempt can succeed.
		{&capture.BadRateError{Requested: 48000}, false},
		{&capture.BadFormatError{Channels: 1}, false},
		{&capture.GeometryError{Rate: 48000}, false},
		{capture.ErrDeviceInUse, false},
		{capture.ErrDeviceGone, false},
		{errors.New("EIO"), false},
	}
	for _, c := range cases {
		if got := permanentOpenError(c.err); got != c.want {
			t.Errorf("permanentOpenError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// overrunCapture is a capture source that ends at once and reports a settable
// overrun count, standing in for the hardware capture openDevice opens and for
// a runtime's capture in the status and counter tests.
type overrunCapture struct{ n atomic.Uint64 }

// newOverrunCapture returns an overrunCapture reporting n overruns.
func newOverrunCapture(n uint64) *overrunCapture {
	c := &overrunCapture{}
	c.n.Store(n)
	return c
}

func (c *overrunCapture) Negotiated() (rate, channels int) { return 48000, 1 }
func (c *overrunCapture) Read() (audio.Period, error)      { return audio.Period{}, io.EOF }
func (c *overrunCapture) Close() error                     { return nil }
func (c *overrunCapture) Overruns() uint64                 { return c.n.Load() }

// openDevice keeps the (metered) base capture as the runtime's source, so the
// capture's overrun count reaches the API and the host monitor; a runtime
// pointed at a per-stream source instead would silently report zero.
func TestOpenDeviceRuntimeReadsCaptureOverruns(t *testing.T) {
	fake := newOverrunCapture(4)
	defer swapOpenCaptureAt(func(*config.Device, int) (audio.Source, capture.Format, error) {
		return fake, capture.FormatS16LE, nil
	})()

	dev := &config.Device{Name: "yard", Device: devMissing, Rate: 48000, Format: testFmtS16,
		Streams: []config.Stream{{Path: "/yard", Mode: config.ModePCM, Channels: []int{1}}}}
	rt, err := openDevice(dev, 1, levels.NewHub())
	if err != nil {
		t.Fatalf("openDevice: %v", err)
	}
	defer func() { _ = rt.fanout.Close() }()
	if got := rt.overruns(); got != 4 {
		t.Errorf("runtime overruns = %d, want the capture's 4", got)
	}
	fake.n.Store(9)
	if got := rt.status().Overruns; got != 9 {
		t.Errorf("status Overruns = %d, want the capture's live 9", got)
	}
}
