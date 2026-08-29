package rtspserver

import (
	"context"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// ChanSource is a bounded FrameSource: the pipeline Pushes frames and the
// playing session's writer Nexts them. Push copies the payload so the
// pipeline's buffer reuse is safe.
type ChanSource struct {
	ch chan pipeline.Frame
}

// NewChanSource returns a ChanSource buffering up to capacity frames.
func NewChanSource(capacity int) *ChanSource {
	return &ChanSource{ch: make(chan pipeline.Frame, capacity)}
}

// Next returns the next frame, blocking until one is available or ctx is done.
func (c *ChanSource) Next(ctx context.Context) (pipeline.Frame, error) {
	select {
	case <-ctx.Done():
		return pipeline.Frame{}, ctx.Err()
	case f := <-c.ch:
		return f, nil
	}
}

// Push copies and enqueues a frame. It returns false when the buffer is full (a
// slow client); the caller should tear the session down rather than block the
// capture loop.
func (c *ChanSource) Push(f pipeline.Frame) bool {
	cp := pipeline.Frame{
		Payload:  append([]byte(nil), f.Payload...),
		Duration: f.Duration,
		Captured: f.Captured,
	}
	select {
	case c.ch <- cp:
		return true
	default:
		return false
	}
}
