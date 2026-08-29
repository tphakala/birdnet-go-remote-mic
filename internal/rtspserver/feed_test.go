package rtspserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

func TestPushDiscardedWhileInactive(t *testing.T) {
	c := NewChanSource(4)
	if !c.Push(pipeline.Frame{Payload: []byte{1}, Duration: 1}) {
		t.Fatal("inactive push should report success (discard, not drop)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inactive push must not be delivered; Next err = %v", err)
	}
}

func TestActivateDrainsStaleFrames(t *testing.T) {
	c := NewChanSource(4)
	c.SetActive(true)
	if !c.Push(pipeline.Frame{Payload: []byte{1}, Duration: 1}) {
		t.Fatal("active push should succeed")
	}
	c.SetActive(false)
	c.SetActive(true) // reactivation drains the leftover frame
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale frame survived reactivation; Next err = %v", err)
	}
}

func TestCloseUnblocksNext(t *testing.T) {
	c := NewChanSource(1)
	c.Close()
	c.Close() // idempotent
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Next(ctx); !errors.Is(err, ErrSourceClosed) {
		t.Fatalf("Next after Close = %v, want ErrSourceClosed", err)
	}
}

func TestCloseWinsOverBufferedFrame(t *testing.T) {
	c := NewChanSource(4)
	c.SetActive(true)
	if !c.Push(pipeline.Frame{Payload: []byte{1}, Duration: 1}) {
		t.Fatal("active push should succeed")
	}
	c.Close() // a buffered frame is queued, but the source is now dead
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Next(ctx); !errors.Is(err, ErrSourceClosed) {
		t.Fatalf("Next after Close = %v, want ErrSourceClosed even with a frame queued", err)
	}
}
