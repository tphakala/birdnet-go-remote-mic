//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	capture "github.com/tphakala/go-audio-capture"
	wavpcm "github.com/tphakala/go-wav/pcm"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/filesource"
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
func swapOpenCaptureAt(fn func(*config.Device, int, audio.Geometry) (audio.Source, capture.Format, error)) func() {
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
	defer swapOpenCaptureAt(func(*config.Device, int, audio.Geometry) (audio.Source, capture.Format, error) {
		calls++
		return nil, 0, capture.ErrDeviceInUse
	})()

	dev := &config.Device{Name: "busy", Device: devMissing, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Channels: []int{1}}}}
	synctest.Test(t, func(t *testing.T) {
		_, err := openDeviceRetry(dev, 0, levels.NewHub())
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
	defer swapOpenCaptureAt(func(_ *config.Device, openCh int, _ audio.Geometry) (audio.Source, capture.Format, error) {
		seen = append(seen, openCh)
		return nil, 0, capture.ErrDeviceInUse
	})()

	dev := &config.Device{Name: "stereo", Device: devMissing, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Channels: []int{2}}}}
	synctest.Test(t, func(t *testing.T) {
		if _, err := openDeviceRetry(dev, 0, levels.NewHub()); !errors.Is(err, capture.ErrDeviceInUse) {
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
	defer swapOpenCaptureAt(func(_ *config.Device, openCh int, _ audio.Geometry) (audio.Source, capture.Format, error) {
		seen = append(seen, openCh)
		if openCh == 1 {
			return nil, 0, capture.ErrDeviceInUse // busy only at the wrong fallback count
		}
		return newOverrunCapture(0), capture.FormatS16LE, nil
	})()

	dev := &config.Device{Name: "stereo", Device: devMissing, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Path: "/stereo", Mode: config.ModePCM, Channels: []int{1, 2}}}}
	synctest.Test(t, func(t *testing.T) {
		rt, err := openDeviceRetry(dev, 0, levels.NewHub())
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
	_, err := openDeviceRetry(dev, 0, levels.NewHub())
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
	defer swapOpenCaptureAt(func(*config.Device, int, audio.Geometry) (audio.Source, capture.Format, error) {
		return fake, capture.FormatS16LE, nil
	})()

	dev := &config.Device{Name: "yard", Device: devMissing, Rate: 48000, Format: testFmtS16,
		Streams: []config.Stream{{Path: "/yard", Mode: config.ModePCM, Channels: []int{1}}}}
	rt, err := openDevice(dev, 1, audio.Geometry{}, levels.NewHub())
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

// grantingCapture is a capture that reports the geometry its "driver" granted,
// which may differ from the one requested.
type grantingCapture struct {
	overrunCapture
	granted audio.Geometry
}

func (c *grantingCapture) NegotiatedGeometry() audio.Geometry { return c.granted }

// TestOpenDeviceRetryGeometry pins the ladder's wiring into the capture open: a
// step becomes the requested period in frames at the device's rate (step 0 the
// zero Geometry, the library default), and the runtime records what the driver
// granted, not what was asked.
func TestOpenDeviceRetryGeometry(t *testing.T) {
	cases := []struct {
		name string
		rate int
		step int
		want audio.Geometry
	}{
		{"default", 48000, 0, audio.Geometry{}},
		{"step 2 at 48 kHz", 48000, 2, audio.Geometry{PeriodFrames: 1920, Periods: 8}},
		{"step 3 at 44.1 kHz", 44100, 3, audio.Geometry{PeriodFrames: 1764, Periods: 16}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer swapResolveOpenChannels(func(string, []int) int { return 1 })()
			granted := audio.Geometry{PeriodFrames: 2048, Periods: 4}
			var asked audio.Geometry
			defer swapOpenCaptureAt(func(_ *config.Device, _ int, geo audio.Geometry) (audio.Source, capture.Format, error) {
				asked = geo
				return &grantingCapture{granted: granted}, capture.FormatS16LE, nil
			})()
			dev := &config.Device{Name: "g", Device: devMissing, Rate: tc.rate, Format: testFmtS16, Streams: []config.Stream{{Path: "/g", Mode: config.ModePCM, Channels: []int{1}}}}
			rt, err := openDeviceRetry(dev, tc.step, levels.NewHub())
			if err != nil {
				t.Fatalf("openDeviceRetry: %v", err)
			}
			defer func() { _ = rt.fanout.Close() }()
			if asked != tc.want {
				t.Errorf("requested geometry = %+v, want %+v", asked, tc.want)
			}
			if rt.geometry != granted {
				t.Errorf("runtime geometry = %+v, want the granted %+v", rt.geometry, granted)
			}
		})
	}
}

// geomSource is a source with a settable geometry that ends at once.
type geomSource struct{ rate, channels int }

func (g geomSource) Negotiated() (rate, channels int) { return g.rate, g.channels }
func (g geomSource) Read() (audio.Period, error)      { return audio.Period{}, io.EOF }
func (g geomSource) Close() error                     { return nil }

// TestOpenDeviceRetryOpensFileSourceOnce pins the file dispatch: a file id
// never reaches the ALSA seams, is attempted exactly once, gets channel bounds
// from the selection and the config cap, and a failed open returns the error
// itself (no typed-nil source in the interface).
func TestOpenDeviceRetryOpensFileSourceOnce(t *testing.T) {
	defer swapResolveOpenChannels(func(string, []int) int {
		t.Error("resolveOpenChannels called for a file device")
		return 1
	})()
	defer swapOpenCaptureAt(func(*config.Device, int, audio.Geometry) (audio.Source, capture.Format, error) {
		t.Error("openCaptureAt called for a file device")
		return nil, 0, errors.New("unreachable")
	})()
	var cfgs []filesource.Config
	var openErr error
	prev := openFileSource
	openFileSource = func(cfg filesource.Config) (audio.Source, error) {
		cfgs = append(cfgs, cfg)
		if openErr != nil {
			return nil, openErr
		}
		return geomSource{rate: cfg.Rate, channels: 3}, nil
	}
	defer func() { openFileSource = prev }()

	dev := &config.Device{Name: "yard", Device: "file:/srv/audio", Rate: 48000, Format: testFmtS16,
		Streams: []config.Stream{{Path: "/a", Mode: config.ModePCM, Channels: []int{1}}, {Path: "/b", Mode: config.ModePCM, Channels: []int{3, 2}}}}
	rt, err := openDeviceRetry(dev, 2, levels.NewHub())
	if err != nil {
		t.Fatalf("openDeviceRetry: %v", err)
	}
	_ = rt.fanout.Close()
	if rt.format != "s16" || rt.rate != 48000 || rt.channels != 3 {
		t.Errorf("runtime = %s %d Hz %d ch, want s16 48000 Hz 3 ch", rt.format, rt.rate, rt.channels)
	}
	want := filesource.Config{Path: "/srv/audio", Rate: 48000, MinChannels: 3, MaxChannels: config.MaxChannels}
	if len(cfgs) != 1 || cfgs[0].Path != want.Path || cfgs[0].Rate != want.Rate ||
		cfgs[0].MinChannels != want.MinChannels || cfgs[0].MaxChannels != want.MaxChannels {
		t.Fatalf("file source configs = %+v, want one with %+v", cfgs, want)
	}

	cfgs, openErr = nil, errors.New("no audio")
	rt, err = openDeviceRetry(dev, 0, levels.NewHub())
	if rt != nil || !errors.Is(err, openErr) {
		t.Fatalf("openDeviceRetry = %v, %v; want nil and the open error", rt, err)
	}
	if len(cfgs) != 1 {
		t.Errorf("failed open attempted %d times, want 1", len(cfgs))
	}
}

// TestOpenFileSourceReturnsNilInterfaceOnError pins the default opener: a
// failed open must give a nil audio.Source (not an interface holding a nil
// *filesource.Source), and a playable file opens with its own geometry.
func TestOpenFileSourceReturnsNilInterfaceOnError(t *testing.T) {
	src, err := openFileSource(filesource.Config{Path: filepath.Join(t.TempDir(), "missing.wav"), Rate: 48000, MinChannels: 1, MaxChannels: 2})
	if src != nil || err == nil {
		t.Fatalf("openFileSource(missing) = %v, %v; want a nil source and an error", src, err)
	}

	pcm := make([]byte, 48000/50*2*2) // one period of silent stereo
	var wav bytes.Buffer
	if err := wavpcm.EncodeInterleaved(&wav, wavpcm.Config{SampleRate: 48000, Channels: 2, BitDepth: 16}, pcm); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a.wav")
	if err := os.WriteFile(path, wav.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err = openFileSource(filesource.Config{Path: path, Rate: 48000, MinChannels: 1, MaxChannels: 2})
	if err != nil {
		t.Fatalf("openFileSource(wav): %v", err)
	}
	defer func() { _ = src.Close() }()
	if rate, ch := src.Negotiated(); rate != 48000 || ch != 2 {
		t.Errorf("Negotiated = (%d, %d), want (48000, 2)", rate, ch)
	}
}
