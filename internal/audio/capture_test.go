//go:build linux

package audio

import (
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
	for _, f := range []string{"s16", ""} {
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
	if _, err := OpenCapture(&config.Device{Device: testDevID, Rate: 48000, Channels: 1, Format: "s32"}); err == nil {
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
	src, err := OpenCapture(&config.Device{Device: testDevID, Rate: 48000, Channels: 1, Format: "s16"})
	if err != nil {
		t.Fatalf("OpenCapture: %v", err)
	}
	defer func() { _ = src.Close() }()
	if got != capture.FormatS16LE {
		t.Errorf("openStream got Format %v, want FormatS16LE", got)
	}
}

func TestOpenCaptureRejectsRateMismatch(t *testing.T) {
	// Requested 256000 but the driver negotiated 48000: OpenCapture must refuse
	// rather than silently deliver the wrong rate.
	stub, restore := swapOpenStream(capture.Config{Rate: 48000, Channels: 1, PeriodFrames: 960})
	defer restore()
	if _, err := OpenCapture(&config.Device{Device: testDevID, Rate: 256000, Channels: 1, Format: "s16"}); err == nil {
		t.Fatal("OpenCapture accepted a rate mismatch, want error")
	}
	if !stub.closed {
		t.Error("OpenCapture did not close the stream on the mismatch")
	}
}

func TestOpenCaptureStartsAndReads(t *testing.T) {
	_, restore := swapOpenStream(capture.Config{Rate: 256000, Channels: 1, PeriodFrames: 5120})
	defer restore()
	src, err := OpenCapture(&config.Device{Device: testDevID, Rate: 256000, Channels: 1, Format: "s16"})
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
