package rtspserver

import (
	"context"
	"errors"
	"runtime"
	"sync"
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

// TestActiveFollowsSetActive pins the gate the pipeline stage polls: Active is
// false until a client plays, true while it does, and false again after it
// stops, so a stream with no client skips its encode.
func TestActiveFollowsSetActive(t *testing.T) {
	t.Parallel()
	c := NewChanSource(4)
	if c.Active() {
		t.Fatal("a new source reports Active; the stage would encode for no client")
	}
	c.SetActive(true)
	if !c.Active() {
		t.Fatal("Active = false after SetActive(true)")
	}
	c.SetActive(false)
	if c.Active() {
		t.Fatal("Active = true after SetActive(false)")
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

// TestSessionAdvancesOnEveryActivation pins the play session the pipeline stage
// keys its encoder reset on: each SetActive(true) starts a new session, even
// one that follows a teardown so closely that no stage saw the stream idle,
// and a deactivation keeps the session number, so a stage reading a period
// after a teardown still sees the session it last encoded for.
func TestSessionAdvancesOnEveryActivation(t *testing.T) {
	t.Parallel()
	c := NewChanSource(4)
	if on, s := c.Session(); on || s != 0 {
		t.Fatalf("new source: Session() = (%v, %d), want (false, 0)", on, s)
	}
	c.SetActive(true)
	on, first := c.Session()
	if !on || first == 0 {
		t.Fatalf("after the first PLAY: Session() = (%v, %d), want (true, nonzero)", on, first)
	}
	c.SetActive(false)
	if on, s := c.Session(); on || s != first {
		t.Fatalf("after teardown: Session() = (%v, %d), want (false, %d)", on, s, first)
	}
	c.SetActive(true)
	on, second := c.Session()
	if !on || second == first {
		t.Fatalf("after the second PLAY: Session() = (%v, %d), want (true, not %d)", on, second, first)
	}
	if c.Active() != on {
		t.Errorf("Active() = %v, want it to match Session()'s %v", c.Active(), on)
	}
}

// TestSetActiveConcurrentKeepsEverySession pins that SetActive is safe to call
// from several goroutines: every activation's session bump survives, and a
// deactivation never writes back a stale session or flag. A load-then-store
// update would lose bumps under this contention, so a later client could be
// handed an old session and keep the previous client's encoder state.
func TestSetActiveConcurrentKeepsEverySession(t *testing.T) {
	t.Parallel()
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs two or more procs: on one, the workers almost never preempt each other mid-update, so a lost bump would not show")
	}
	const workers, rounds = 4, 20000
	c := NewChanSource(1)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range rounds {
				c.SetActive(true)
				c.SetActive(false)
			}
		})
	}
	wg.Wait()
	if on, s := c.Session(); on || s != workers*rounds {
		t.Errorf("after %d activations: Session() = (%v, %d), want (false, %d)", workers*rounds, on, s, workers*rounds)
	}
}
