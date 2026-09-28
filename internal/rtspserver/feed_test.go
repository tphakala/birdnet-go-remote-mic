package rtspserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// mustSubscribe subscribes to f and fails the test on an error.
func mustSubscribe(t *testing.T, f *Feed) Subscription {
	t.Helper()
	s, err := f.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return s
}

// nextPayload returns the first payload byte of the next queued frame, failing
// if none is ready.
func nextPayload(t *testing.T, s Subscription) byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	fr, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return fr.Payload[0]
}

// noFrame fails the test if s has a frame ready.
func noFrame(t *testing.T, s Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if fr, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next = (%v, %v), want no frame", fr, err)
	}
}

func TestFeedPushWithoutSubscribers(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	if got := f.Push(pipeline.Frame{Payload: []byte{1}, Duration: 1}); got != 0 {
		t.Fatalf("Push with no subscribers = %d drops, want 0", got)
	}
	if on, s := f.Session(); on || s != 0 {
		t.Fatalf("idle Session() = (%v, %d), want (false, 0)", on, s)
	}
	// A frame pushed while idle is not held for the next client.
	s := mustSubscribe(t, f)
	noFrame(t, s)
}

func TestFeedEverySubscriberGetsTheSameSequence(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	a, b, c := mustSubscribe(t, f), mustSubscribe(t, f), mustSubscribe(t, f)
	payload := []byte{0}
	for i := byte(1); i <= 5; i++ {
		payload[0] = i
		if got := f.Push(pipeline.Frame{Payload: payload, Duration: 1}); got != 0 {
			t.Fatalf("Push %d dropped %d", i, got)
		}
	}
	// The producer reuses its buffer: the queued frames are copies.
	payload[0] = 99
	for _, s := range []Subscription{a, b, c} {
		for want := byte(1); want <= 5; want++ {
			if got := nextPayload(t, s); got != want {
				t.Fatalf("got payload %d, want %d", got, want)
			}
		}
	}
}

func TestFeedSlowSubscriberDropsOnlyItsOwn(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	mustSubscribe(t, f) // never read
	mustSubscribe(t, f) // never read
	fast := mustSubscribe(t, f)
	for i := range subQueueDepth + 10 {
		// Each slow queue takes the first subQueueDepth frames, then refuses each
		// one, and only the slow queues' refusals are counted, one per queue.
		want := 0
		if i >= subQueueDepth {
			want = 2
		}
		if got := f.Push(pipeline.Frame{Payload: []byte{byte(i)}, Duration: 1}); got != want {
			t.Errorf("Push %d reported %d drops, want %d", i, got, want)
		}
		// The fast subscriber reads every frame as it is pushed.
		if got := nextPayload(t, fast); got != byte(i) {
			t.Fatalf("fast subscriber: frame %d = %d, want a gap-free sequence", i, got)
		}
	}
}

func TestFeedEpoch(t *testing.T) {
	t.Parallel()
	f := NewFeed()

	a := mustSubscribe(t, f)
	on, first := f.Session()
	if !on || first == 0 {
		t.Fatalf("after the first join: Session() = (%v, %d), want active in a session numbered from 1", on, first)
	}
	b := mustSubscribe(t, f)
	if on, s := f.Session(); !on || s != first {
		t.Errorf("second join: Session() = (%v, %d), want (true, %d): a join keeps the session", on, s, first)
	}
	a.Close()
	if on, s := f.Session(); !on || s != first {
		t.Errorf("non-last leave: Session() = (%v, %d), want (true, %d)", on, s, first)
	}
	b.Close()
	if on, s := f.Session(); on || s != first {
		t.Errorf("last leave: Session() = (%v, %d), want (false, %d): inactive, session kept", on, s, first)
	}
	c := mustSubscribe(t, f)
	defer c.Close()
	if on, s := f.Session(); !on || s != first+1 {
		t.Errorf("join after idle: Session() = (%v, %d), want (true, %d)", on, s, first+1)
	}
}

// TestFeedOverlappingJoinKeepsSession pins that a join landing before the
// previous client's close never passes through idle, so the session (and with
// it the stage's encoder) carries on.
func TestFeedOverlappingJoinKeepsSession(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	a := mustSubscribe(t, f)
	_, first := f.Session()
	b := mustSubscribe(t, f)
	a.Close()
	defer b.Close()
	if on, s := f.Session(); !on || s != first {
		t.Fatalf("Session() = (%v, %d), want (true, %d)", on, s, first)
	}
	if f.Push(pipeline.Frame{Payload: []byte{7}, Duration: 1, Session: first}) != 0 {
		t.Fatal("frame of the running session reported a drop")
	}
	if got := nextPayload(t, b); got != 7 {
		t.Fatalf("got %d, want 7", got)
	}
}

func TestFeedNeverDeliversAnEarlierSession(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	a := mustSubscribe(t, f)
	_, old := f.Session()
	a.Close()
	b := mustSubscribe(t, f)
	defer b.Close()
	_, cur := f.Session()
	if cur == old {
		t.Fatalf("session did not advance across a leave and a join: %d", cur)
	}
	// An earlier session's frame, overtaken by the leave and the join while it
	// was being encoded.
	f.Push(pipeline.Frame{Payload: []byte{1}, Duration: 1, Session: old})
	noFrame(t, b)
	f.Push(pipeline.Frame{Payload: []byte{2}, Duration: 1, Session: cur})
	f.Push(pipeline.Frame{Payload: []byte{3}, Duration: 1}) // untagged goes to whoever plays
	if got := nextPayload(t, b); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
	if got := nextPayload(t, b); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestFeedClose(t *testing.T) {
	t.Parallel()
	// Closure wins over a frame that is still queued. Without the pre-check in
	// Next, a select over two ready cases picks one at random, so a single
	// attempt would miss that regression half the time: repeat on fresh feeds.
	for range 64 {
		f := NewFeed()
		b := mustSubscribe(t, f)
		f.Push(pipeline.Frame{Payload: []byte{1}, Duration: 1})
		f.Close()
		if _, err := b.Next(t.Context()); !errors.Is(err, ErrSourceClosed) {
			t.Fatalf("Next with a queued frame after Close: err = %v, want ErrSourceClosed", err)
		}
	}
	f := NewFeed()
	f.Close()
	f.Close() // idempotent
	if _, err := f.Subscribe(); !errors.Is(err, ErrSourceClosed) {
		t.Errorf("Subscribe after Close: err = %v, want ErrSourceClosed", err)
	}
}

// TestFeedCloseWaitsForSubscribe pins the ordering between Close and a
// Subscribe that is midway through publishing: Close must not return, and so
// must not let the feed count as closed, while Subscribe holds the lock, or a
// subscriber could be published after Close returned.
func TestFeedCloseWaitsForSubscribe(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	f.mu.Lock() // stands in for a Subscribe that has passed its closure check
	returned := make(chan struct{})
	go func() {
		f.Close()
		close(returned)
	}()
	select {
	case <-returned:
		f.mu.Unlock()
		t.Fatal("Close returned while a Subscribe held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	f.mu.Unlock()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return once the lock was released")
	}
	if _, err := f.Subscribe(); !errors.Is(err, ErrSourceClosed) {
		t.Errorf("Subscribe after Close: err = %v, want ErrSourceClosed", err)
	}
}

// TestFeedCloseWakesParkedNext pins that Close ends a Next that is already
// blocked on an empty queue.
func TestFeedCloseWakesParkedNext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := NewFeed()
		a := mustSubscribe(t, f)
		errc := make(chan error, 1)
		go func() {
			_, err := a.Next(t.Context())
			errc <- err
		}()
		synctest.Wait() // a's Next is parked on its empty queue
		f.Close()
		synctest.Wait()
		select {
		case err := <-errc:
			if !errors.Is(err, ErrSourceClosed) {
				t.Errorf("parked Next err = %v, want ErrSourceClosed", err)
			}
		default:
			t.Fatal("Close did not wake the parked Next")
		}
	})
}

func TestFeedClientCap(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	subs := make([]Subscription, 0, maxClients)
	for range maxClients {
		subs = append(subs, mustSubscribe(t, f))
	}
	if _, err := f.Subscribe(); !errors.Is(err, ErrTooManyClients) {
		t.Fatalf("subscribe %d: err = %v, want ErrTooManyClients", maxClients+1, err)
	}
	if got := f.Clients(); got != maxClients {
		t.Fatalf("Clients() = %d after a refused subscribe, want %d", got, maxClients)
	}
	subs[3].Close()
	subs[3].Close() // idempotent: must not free a second slot
	if got := f.Clients(); got != maxClients-1 {
		t.Fatalf("Clients() = %d after one Close, want %d", got, maxClients-1)
	}
	mustSubscribe(t, f)
	if _, err := f.Subscribe(); !errors.Is(err, ErrTooManyClients) {
		t.Fatalf("err = %v, want ErrTooManyClients once full again", err)
	}
}

func TestFeedConcurrent(t *testing.T) {
	t.Parallel()
	f := NewFeed()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup

	wg.Go(func() {
		for ctx.Err() == nil {
			f.Push(pipeline.Frame{Payload: []byte{1, 2, 3}, Duration: 1})
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			_, _ = f.Session()
			_ = f.Clients()
		}
	})
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				s, err := f.Subscribe()
				if err != nil {
					continue // at the cap
				}
				rctx, rcancel := context.WithTimeout(ctx, time.Millisecond)
				_, _ = s.Next(rctx)
				rcancel()
				s.Close()
			}
		})
	}
	wg.Wait()
	if got := f.Clients(); got != 0 {
		t.Errorf("Clients() = %d after every subscriber closed, want 0", got)
	}
}

// TestFeedPushAllocs pins the allocation contract: none while idle or for a
// frame of an earlier session, exactly the one payload copy per frame with one
// or more clients.
func TestFeedPushAllocs(t *testing.T) {
	// Not parallel: AllocsPerRun counts process-wide allocations.
	for _, tt := range []struct {
		name     string
		clients  int
		staleTag bool // tag the frame with the session before the current one
		want     float64
	}{
		{"no clients", 0, false, 0},
		{"one client", 1, false, 1},
		{"three clients", 3, false, 1},
		{"stale tag", 3, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFeed()
			if tt.staleTag {
				// One earlier stretch of playing, so the clients below join a
				// session numbered at least 2 and the tag before it is a real
				// session, not the untagged zero.
				mustSubscribe(t, f).Close()
			}
			subs := make([]Subscription, tt.clients)
			for i := range subs {
				subs[i] = mustSubscribe(t, f)
			}
			_, tag := f.Session()
			if tt.staleTag {
				tag--
			}
			fr := pipeline.Frame{Payload: make([]byte, 320), Duration: 960, Session: tag}
			// A delivery that never arrives fails the test after the deadline
			// instead of hanging it inside the measured loop.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var lost error
			got := testing.AllocsPerRun(100, func() {
				f.Push(fr)
				if tt.staleTag {
					return // nothing is queued for a stale frame
				}
				for _, s := range subs {
					if _, err := s.Next(ctx); err != nil {
						lost = err
					}
				}
			})
			if lost != nil {
				t.Fatalf("a subscriber was not delivered the frame: %v", lost)
			}
			if got != tt.want {
				t.Errorf("allocs per Push = %v, want %v", got, tt.want)
			}
		})
	}
}
