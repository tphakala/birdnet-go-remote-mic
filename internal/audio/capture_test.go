//go:build linux

package audio

import (
	"encoding/binary"
	"errors"
	"testing"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// stubStream is a captureStream that reports a fixed negotiated config.
type stubStream struct {
	neg     capture.Config
	started bool
	closed  bool
}

func (s *stubStream) Negotiated() capture.Config { return s.neg }
func (s *stubStream) Start() error               { s.started = true; return nil }
func (s *stubStream) Read(buf []byte) (int, error) {
	return len(buf) / 2, nil
}
func (s *stubStream) Close() error { s.closed = true; return nil }

func swapOpenStream(neg capture.Config) (stub *stubStream, restore func()) {
	stub = &stubStream{neg: neg}
	prev := openStream
	openStream = func(capture.Config) (captureStream, error) { return stub, nil }
	return stub, func() { openStream = prev }
}

func TestCaptureFormat(t *testing.T) {
	for _, f := range []string{testFmtS16, ""} {
		got, err := captureFormat(f)
		if err != nil || got != capture.FormatS16LE {
			t.Errorf("captureFormat(%q) = %v, %v; want FormatS16LE, nil", f, got, err)
		}
	}
	if _, err := captureFormat("s32"); err == nil {
		t.Error("captureFormat(\"s32\") = nil error, want unsupported-format error")
	}
}

func TestOpenCaptureRejectsUnknownFormat(t *testing.T) {
	// A format that config.Validate does not (yet) permit must fail loud rather
	// than silently fall back to S16LE and corrupt the byte math.
	if _, err := OpenCapture(&config.Device{Device: testDevID, Rate: 48000, Channels: []int{1}, Format: "s32"}); err == nil {
		t.Fatal("OpenCapture accepted format s32, want error")
	}
}

func TestOpenCapturePassesS16Format(t *testing.T) {
	var got capture.Format
	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		got = cfg.Format
		return &stubStream{neg: capture.Config{Rate: 48000, Channels: 1, PeriodFrames: 960}}, nil
	}
	defer func() { openStream = prev }()
	src, err := OpenCapture(&config.Device{Device: testDevID, Rate: 48000, Channels: []int{1}, Format: testFmtS16})
	if err != nil {
		t.Fatalf("OpenCapture: %v", err)
	}
	defer func() { _ = src.Close() }()
	if got != capture.FormatS16LE {
		t.Errorf("openStream got Format %v, want FormatS16LE", got)
	}
}

// twoChanStub negotiates 2 channels and emits interleaved S16 with a distinct
// value per channel (ch0=0x1111, ch1=0x2222), so a test can prove which channel
// the selecting source kept.
type twoChanStub struct {
	neg     capture.Config
	started bool
	closed  bool
}

func (s *twoChanStub) Negotiated() capture.Config { return s.neg }
func (s *twoChanStub) Start() error               { s.started = true; return nil }
func (s *twoChanStub) Read(buf []byte) (int, error) {
	frames := len(buf) / 4 // 2 channels x S16
	for f := range frames {
		binary.LittleEndian.PutUint16(buf[f*4:], 0x1111)
		binary.LittleEndian.PutUint16(buf[f*4+2:], 0x2222)
	}
	return frames, nil
}
func (s *twoChanStub) Close() error { s.closed = true; return nil }

func TestOpenCaptureExtractsSelectedChannel(t *testing.T) {
	// A stereo-only device (opens at 2 channels) with a single-channel selection
	// [1]: OpenCapture must open at 2 channels and wrap in a selecting source that
	// delivers 1 channel carrying channel 0's data. This exercises the actual
	// channel-reduction wiring (openCh from ResolveOpenChannels, n.Channels as the
	// source width, dev.Channels as the selection), not the passthrough path.
	restoreCh := swapSupportedRates(func(_ string, ch int, _ capture.Format) (capture.RateSupport, error) {
		if ch == 2 {
			return capture.RateSupport{Rates: []int{48000}}, nil
		}
		return capture.RateSupport{}, &capture.BadFormatError{Channels: ch}
	})
	defer restoreCh()

	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		if cfg.Channels != 2 {
			t.Errorf("openStream Channels = %d, want 2 (stereo-only open for a mono selection)", cfg.Channels)
		}
		return &twoChanStub{neg: capture.Config{Rate: 48000, Channels: 2, PeriodFrames: 3}}, nil
	}
	defer func() { openStream = prev }()

	src, err := OpenCapture(&config.Device{Device: testDevID, Rate: 48000, Channels: []int{1}, Format: testFmtS16})
	if err != nil {
		t.Fatalf("OpenCapture: %v", err)
	}
	defer func() { _ = src.Close() }()
	if _, ch := src.Negotiated(); ch != 1 {
		t.Fatalf("Negotiated channels = %d, want 1 (selecting source reduces 2->1)", ch)
	}
	p, err := src.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if p.Frames == 0 {
		t.Fatal("Read returned 0 frames")
	}
	for f := range p.Frames {
		if s := int16(binary.LittleEndian.Uint16(p.Buf[f*2:])); s != 0x1111 {
			t.Fatalf("frame %d sample = %#x, want 0x1111 (channel 0 extracted, not channel 1)", f, s)
		}
	}
}

// s32Stream is a captureStream that emits fixed interleaved S32LE sample data,
// for exercising OpenCapture's S32 fallback and the converting source.
type s32Stream struct {
	neg     capture.Config
	samples []int32
	closed  bool
}

func (s *s32Stream) Negotiated() capture.Config { return s.neg }
func (s *s32Stream) Start() error               { return nil }
func (s *s32Stream) Read(buf []byte) (int, error) {
	for i, v := range s.samples {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}
	return len(s.samples) / s.neg.Channels, nil
}
func (s *s32Stream) Close() error { s.closed = true; return nil }

func TestOpenCaptureFallsBackToS32AndDownconverts(t *testing.T) {
	// The device rejects S16 (here with a rate error, which must NOT short-circuit
	// the S32 attempt), then opens in S32. Each S32 sample is downconverted to its
	// top 16 bits before reaching the pipeline.
	fake := &s32Stream{
		neg:     capture.Config{Rate: 48000, Channels: 1, PeriodFrames: 2},
		samples: []int32{0x11112222, 0x33334444},
	}
	var tried []capture.Format
	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		tried = append(tried, cfg.Format)
		if cfg.Format == capture.FormatS16LE {
			return nil, &capture.BadRateError{Requested: cfg.Rate}
		}
		return fake, nil
	}
	defer func() { openStream = prev }()

	src, err := OpenCapture(&config.Device{Device: testDevID, Rate: 48000, Channels: []int{1}, Format: testFmtS16})
	if err != nil {
		t.Fatalf("OpenCapture: %v", err)
	}
	defer func() { _ = src.Close() }()

	if len(tried) != 2 || tried[0] != capture.FormatS16LE || tried[1] != capture.FormatS32LE {
		t.Fatalf("tried formats = %v, want [FormatS16LE FormatS32LE]", tried)
	}
	if r, ch := src.Negotiated(); r != 48000 || ch != 1 {
		t.Errorf("Negotiated = %d, %d; want 48000, 1", r, ch)
	}
	p, err := src.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if p.Frames != 2 {
		t.Errorf("frames = %d, want 2", p.Frames)
	}
	want := []int16{0x1111, 0x3333}
	if len(p.Buf) != len(want)*2 {
		t.Fatalf("buf len = %d, want %d", len(p.Buf), len(want)*2)
	}
	for i, w := range want {
		if got := int16(binary.LittleEndian.Uint16(p.Buf[i*2:])); got != w {
			t.Errorf("sample %d = %#04x, want %#04x", i, uint16(got), uint16(w))
		}
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Error("Close did not close the underlying S32 stream")
	}
}

func TestOpenCapturePrefersRateErrorAcrossFormats(t *testing.T) {
	// preferRateError must surface a *BadRateError over a raw driver error
	// whichever format produced it, and when BOTH formats reject the rate it must
	// keep the FIRST (S16) error so the reported range is the preferred format's,
	// not the narrower fallback's. Min identifies which format's error survived.
	raw := errors.New("alsa: HW_REFINE: invalid argument")
	s16Rate := &capture.BadRateError{Requested: 384000, Min: 16000, Max: 384000}
	s32Rate := &capture.BadRateError{Requested: 384000, Min: 44100, Max: 96000}
	cases := []struct {
		name     string
		s16, s32 error
		wantRate bool // expect a *BadRateError to surface
		wantMin  int  // Min of the surfaced range (identifies which format's error)
	}{
		{"raw_then_rate", raw, s32Rate, true, 44100},            // S32 rate error beats S16 raw
		{"rate_then_raw", s16Rate, raw, true, 16000},            // S16 rate error kept over S32 raw
		{"both_rate_keep_first", s16Rate, s32Rate, true, 16000}, // S16 (first) range wins
		{"both_raw", raw, errors.New("other"), false, 0},        // no rate error to prefer
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := openStream
			openStream = func(cfg capture.Config) (captureStream, error) {
				if cfg.Format == capture.FormatS16LE {
					return nil, tc.s16
				}
				return nil, tc.s32
			}
			defer func() { openStream = prev }()

			_, err := OpenCapture(&config.Device{Device: testDevID, Rate: 384000, Channels: []int{1}, Format: testFmtS16})
			var bre *capture.BadRateError
			if got := errors.As(err, &bre); got != tc.wantRate {
				t.Fatalf("errors.As BadRateError = %v, want %v (err=%v)", got, tc.wantRate, err)
			}
			if tc.wantRate && bre.Min != tc.wantMin {
				t.Errorf("surfaced range Min = %d, want %d (wrong format's error surfaced)", bre.Min, tc.wantMin)
			}
		})
	}
}

func TestOpenCaptureRejectsRateMismatch(t *testing.T) {
	// Requested 256000 but the driver negotiated 48000: OpenCapture must refuse
	// rather than silently deliver the wrong rate.
	stub, restore := swapOpenStream(capture.Config{Rate: 48000, Channels: 1, PeriodFrames: 960})
	defer restore()
	if _, err := OpenCapture(&config.Device{Device: testDevID, Rate: 256000, Channels: []int{1}, Format: testFmtS16}); err == nil {
		t.Fatal("OpenCapture accepted a rate mismatch, want error")
	}
	if !stub.closed {
		t.Error("OpenCapture did not close the stream on the mismatch")
	}
}

func TestOpenCaptureStartsAndReads(t *testing.T) {
	_, restore := swapOpenStream(capture.Config{Rate: 256000, Channels: 1, PeriodFrames: 5120})
	defer restore()
	src, err := OpenCapture(&config.Device{Device: testDevID, Rate: 256000, Channels: []int{1}, Format: testFmtS16})
	if err != nil {
		t.Fatalf("OpenCapture: %v", err)
	}
	defer func() { _ = src.Close() }()
	rate, ch := src.Negotiated()
	if rate != 256000 || ch != 1 {
		t.Errorf("Negotiated = %d, %d; want 256000, 1", rate, ch)
	}
	p, err := src.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if p.Frames != 5120 { // buffer is PeriodFrames*frameBytes; stub returns len/2 frames
		t.Errorf("Read frames = %d, want 5120", p.Frames)
	}
}

// TestOpenCaptureAtOpensAtGivenCount verifies the caller-resolved open channel
// count is what reaches the hardware open, so the appliance can resolve it once
// (for the rate probe, the busy gate and the open) instead of re-probing here.
func TestOpenCaptureAtOpensAtGivenCount(t *testing.T) {
	var got int
	prev := openStream
	openStream = func(cfg capture.Config) (captureStream, error) {
		got = cfg.Channels
		return &stubStream{neg: capture.Config{Rate: 48000, Channels: cfg.Channels, PeriodFrames: 960}}, nil
	}
	defer func() { openStream = prev }()
	src, err := OpenCaptureAt(&config.Device{Device: testDevID, Rate: 48000, Channels: []int{1}, Format: testFmtS16}, 2)
	if err != nil {
		t.Fatalf("OpenCaptureAt: %v", err)
	}
	defer func() { _ = src.Close() }()
	if got != 2 {
		t.Errorf("hardware opened at %d channels, want the given 2", got)
	}
	if _, ch := src.Negotiated(); ch != 1 {
		t.Errorf("stream delivers %d channels, want the 1 selected", ch)
	}
}
