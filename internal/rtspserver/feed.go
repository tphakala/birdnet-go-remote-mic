package rtspserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// ErrSourceClosed is returned by Next once the source's device is gone.
var ErrSourceClosed = errors.New("rtspserver: frame source closed")

// ChanSource is a bounded FrameSource: the pipeline Pushes frames and the
// playing session's writer Nexts them. Delivery is gated on an active flag so
// no audio is buffered (or copied) while no client is playing; activation
// drains leftovers so a new client never receives stale audio. Push copies the
// payload so the pipeline's buffer reuse is safe.
type ChanSource struct {
	ch        chan pipeline.Frame
	done      chan struct{}
	closeOnce sync.Once
	active    atomic.Bool
}

// NewChanSource returns a ChanSource buffering up to capacity frames.
func NewChanSource(capacity int) *ChanSource {
	return &ChanSource{ch: make(chan pipeline.Frame, capacity), done: make(chan struct{})}
}

// Next returns the next frame, blocking until one is available, ctx is done,
// or the source is closed (dead device). Closure wins over a buffered frame: a
// closed source returns ErrSourceClosed even when the channel still holds a
// frame, so a dead device's writer tears down at once rather than emitting one
// last packet (select would otherwise pick a ready case at random).
func (c *ChanSource) Next(ctx context.Context) (pipeline.Frame, error) {
	select {
	case <-c.done:
		return pipeline.Frame{}, ErrSourceClosed
	default:
	}
	select {
	case <-ctx.Done():
		return pipeline.Frame{}, ctx.Err()
	case <-c.done:
		return pipeline.Frame{}, ErrSourceClosed
	case f := <-c.ch:
		return f, nil
	}
}

// Push copies and enqueues a frame when a client is playing; while inactive it
// discards without copying and reports true (a discard is not a drop). It
// returns false only when the buffer is full (a slow client); the caller
// should keep capturing and let the writer fall behind.
func (c *ChanSource) Push(f pipeline.Frame) bool {
	if !c.active.Load() {
		return true
	}
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

// SetActive toggles delivery. Activation first drains any frames left over
// from a previous session.
func (c *ChanSource) SetActive(active bool) {
	if !active {
		c.active.Store(false)
		return
	}
	for {
		select {
		case <-c.ch:
		default:
			c.active.Store(true)
			return
		}
	}
}

// Close marks the source dead: Next returns ErrSourceClosed so a playing
// writer tears down. Safe to call more than once.
func (c *ChanSource) Close() {
	c.closeOnce.Do(func() { close(c.done) })
}
