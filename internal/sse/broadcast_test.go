package sse

import (
	"sync"
	"testing"
)

func testEvent(name string) Event {
	return Event{Name: name, Data: []byte("{}")}
}

func TestBroadcasterDeliversToAllSubscribers(t *testing.T) {
	b := NewBroadcaster(4)
	ch1, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	defer cancel1()
	defer cancel2()

	ev := testEvent("a")
	b.Broadcast(ev)

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Name != ev.Name {
				t.Fatalf("subscriber %d got %q, want %q", i, got.Name, ev.Name)
			}
		default:
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestBroadcasterDropsWhenBufferFull(t *testing.T) {
	// Buffer of one: the first event fills the buffer, the second must drop
	// rather than block, so Broadcast returns and the slow consumer only ever
	// sees the first event.
	b := NewBroadcaster(1)
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Broadcast(testEvent("first"))
	b.Broadcast(testEvent("second")) // must not block; dropped

	select {
	case got := <-ch:
		if got.Name != "first" {
			t.Fatalf("got %q, want first", got.Name)
		}
	default:
		t.Fatal("expected the first event to be buffered")
	}
	select {
	case got := <-ch:
		t.Fatalf("expected the second event to be dropped, got %q", got.Name)
	default:
	}
}

func TestBroadcasterCancelUnsubscribes(t *testing.T) {
	b := NewBroadcaster(4)
	ch, cancel := b.Subscribe()
	if got := b.Len(); got != 1 {
		t.Fatalf("Len after Subscribe = %d, want 1", got)
	}
	cancel()
	if got := b.Len(); got != 0 {
		t.Fatalf("Len after cancel = %d, want 0", got)
	}
	// A broadcast after cancel must not reach the canceled channel.
	b.Broadcast(testEvent("late"))
	select {
	case got := <-ch:
		t.Fatalf("canceled subscriber received %q", got.Name)
	default:
	}
}

func TestBroadcasterCancelIdempotent(t *testing.T) {
	b := NewBroadcaster(4)
	_, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	defer cancel2()

	cancel1()
	cancel1() // second call must be a no-op, not a panic or a double-delete

	// The double-cancel must remove only the first subscriber, leaving the
	// second in the set rather than clearing or corrupting it.
	if got := b.Len(); got != 1 {
		t.Fatalf("Len after double cancel of the first subscriber = %d, want 1", got)
	}

	// The surviving second subscriber still receives a subsequent broadcast,
	// proving the double-cancel did not corrupt the subscriber set.
	b.Broadcast(testEvent("after"))
	select {
	case got := <-ch2:
		if got.Name != "after" {
			t.Fatalf("second subscriber got %q, want after", got.Name)
		}
	default:
		t.Fatal("second subscriber received nothing after the double cancel")
	}
}

func TestBroadcasterChannelNeverClosed(t *testing.T) {
	b := NewBroadcaster(4)
	ch, cancel := b.Subscribe()
	cancel()
	// The channel must stay open after cancel so a late Broadcast (or a
	// concurrent one racing the cancel) can never send on a closed channel. No
	// event was broadcast, so a receive that reports ok==false would mean the
	// channel was closed, which the contract forbids.
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("channel was closed after cancel; the contract forbids closing it")
		}
		t.Fatal("received an unexpected event")
	default:
	}
}

func TestNewBroadcasterDefaultBuffer(t *testing.T) {
	for _, buf := range []int{0, -1, -100} {
		b := NewBroadcaster(buf)
		ch, cancel := b.Subscribe()
		if got := cap(ch); got != defaultBroadcastBuffer {
			t.Fatalf("NewBroadcaster(%d) channel cap = %d, want %d", buf, got, defaultBroadcastBuffer)
		}
		cancel()
	}
}

func TestBroadcasterCustomBuffer(t *testing.T) {
	b := NewBroadcaster(8)
	ch, cancel := b.Subscribe()
	defer cancel()
	if got := cap(ch); got != 8 {
		t.Fatalf("channel cap = %d, want 8", got)
	}
}

func TestBroadcasterBroadcastNoSubscribers(t *testing.T) {
	// Broadcasting to an empty set must be a harmless no-op.
	b := NewBroadcaster(4)
	b.Broadcast(testEvent("nobody"))
	if got := b.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
}

func TestBroadcasterConcurrent(t *testing.T) {
	// Exercise Subscribe, cancel, Broadcast, and Len concurrently so the race
	// detector can flag any unsynchronized access to the subscriber set.
	b := NewBroadcaster(4)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ch, cancel := b.Subscribe()
				_ = ch
				b.Broadcast(testEvent("x"))
				_ = b.Len()
				cancel()
			}
		}()
	}
	wg.Wait()
	if got := b.Len(); got != 0 {
		t.Fatalf("Len after all subscribers canceled = %d, want 0", got)
	}
}
