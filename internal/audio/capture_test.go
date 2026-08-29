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

func TestOpenCaptureRejectsRateMismatch(t *testing.T) {
	// Requested 256000 but the driver negotiated 48000: OpenCapture must refuse
	// rather than silently deliver the wrong rate.
	stub, restore := swapOpenStream(capture.Config{Rate: 48000, Channels: 1, PeriodFrames: 960})
	defer restore()
	if _, err := OpenCapture(config.Device{Device: "hw:1,0", Rate: 256000, Channels: 1, Format: "s16"}); err == nil {
		t.Fatal("OpenCapture accepted a rate mismatch, want error")
	}
	if !stub.closed {
		t.Error("OpenCapture did not close the stream on the mismatch")
	}
}

func TestOpenCaptureStartsAndReads(t *testing.T) {
	_, restore := swapOpenStream(capture.Config{Rate: 256000, Channels: 1, PeriodFrames: 5120})
	defer restore()
	src, err := OpenCapture(config.Device{Device: "hw:1,0", Rate: 256000, Channels: 1, Format: "s16"})
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
