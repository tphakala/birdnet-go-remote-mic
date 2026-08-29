//go:build linux

package audio

import (
	"fmt"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// captureStream is the subset of *capture.Stream the appliance uses; the seam
// lets tests substitute a stub with no hardware.
type captureStream interface {
	Negotiated() capture.Config
	Start() error
	Read(buf []byte) (int, error)
	Close() error
}

// openStream is a package var so tests can inject a fake capture layer.
var openStream = func(cfg capture.Config) (captureStream, error) {
	s, err := capture.Open(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

type captureSource struct {
	s          captureStream
	rate       int
	channels   int
	frameBytes int
	buf        []byte
}

// OpenCapture opens and starts a capture stream for cfg. It enforces the
// honest-rate policy: go-audio-capture already fails a rate it cannot deliver
// exactly, and OpenCapture double-checks the negotiated rate matches the
// request. The caller's read goroutine should runtime.LockOSThread so the
// capture loop is not descheduled mid-period.
func OpenCapture(dev config.Device) (Source, error) {
	s, err := openStream(capture.Config{
		Device:   dev.Device,
		Rate:     dev.Rate,
		Channels: dev.Channels,
		Format:   capture.FormatS16LE,
	})
	if err != nil {
		return nil, err
	}
	n := s.Negotiated()
	if n.Rate != dev.Rate {
		_ = s.Close()
		return nil, fmt.Errorf("audio: negotiated rate %d Hz does not match requested %d Hz", n.Rate, dev.Rate)
	}
	if err := s.Start(); err != nil {
		_ = s.Close()
		return nil, err
	}
	frameBytes := n.Channels * 2 // S16LE
	return &captureSource{
		s:          s,
		rate:       n.Rate,
		channels:   n.Channels,
		frameBytes: frameBytes,
		buf:        make([]byte, n.PeriodFrames*frameBytes),
	}, nil
}

func (c *captureSource) Negotiated() (rate, channels int) { return c.rate, c.channels }

func (c *captureSource) Read() (Period, error) {
	n, err := c.s.Read(c.buf)
	if err != nil {
		return Period{}, err
	}
	return Period{Buf: c.buf[:n*c.frameBytes], Frames: n}, nil
}

func (c *captureSource) Close() error { return c.s.Close() }
