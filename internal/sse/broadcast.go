package sse

import "sync"

// defaultBroadcastBuffer is the per-subscriber channel depth a Broadcaster uses
// when the caller passes a non-positive buffer. It matches the handler's merge
// buffer so a producer that does not care about depth gets a sensible default.
const defaultBroadcastBuffer = 32

// subscriber is one consumer's delivery channel. It is unexported: a consumer
// only ever holds the receive end and the cancel func Subscribe hands back.
type subscriber struct {
	ch chan Event
}

// Broadcaster is a concurrency-safe SSE fan-out: a set of subscriber channels
// with drop-on-full delivery. It owns the subscriber set and its own mutex, so
// a producer that used to hand-roll the set (a "subscriber{ ch }" plus a
// "map[*subscriber]struct{}" under a lock, a non-blocking broadcast, and a
// sync.Once cancel that never closes the channel) embeds one instead. The
// legitimate per-producer state stays at the producer: the levels hub keeps its
// atomic subscriber gate and first-subscriber meter reset, the notification
// center keeps its nil-receiver guard.
//
// A Broadcaster satisfies Source, so it can also be mounted on a Handler
// directly, but producers hold it as a field and wrap Subscribe with their own
// bookkeeping rather than embedding it, to keep that state off their public API.
type Broadcaster struct {
	buffer int

	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

var _ Source = (*Broadcaster)(nil)

// NewBroadcaster returns a ready Broadcaster whose subscriber channels are
// buffered to buffer entries. A non-positive buffer falls back to the default;
// the buffer absorbs a burst, and a slow consumer that fills it drops events
// rather than stalling Broadcast.
func NewBroadcaster(buffer int) *Broadcaster {
	if buffer <= 0 {
		buffer = defaultBroadcastBuffer
	}
	return &Broadcaster{
		buffer: buffer,
		subs:   make(map[*subscriber]struct{}),
	}
}

// Subscribe registers a consumer and returns its event channel plus a cancel
// func that unregisters it. The channel is never closed; a consumer stops by
// calling cancel, which just removes the subscriber under the mutex so a late
// Broadcast cannot send on a closed channel. cancel is idempotent (a second
// call is a no-op). It satisfies Source.
func (b *Broadcaster) Subscribe() (events <-chan Event, cancel func()) {
	s := &subscriber{ch: make(chan Event, b.buffer)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
		})
	}
}

// Broadcast sends ev to every subscriber without blocking: a subscriber whose
// buffer is full drops this event rather than stalling the producer. It holds
// the mutex only for the fan-out, so Subscribe and cancel serialize against it
// and the subscriber set cannot change mid-broadcast.
func (b *Broadcaster) Broadcast(ev Event) {
	b.mu.Lock()
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
}

// Len reports the current number of subscribers. A producer uses it to gate
// work when no client is connected; a test uses it to assert subscribe and
// cancel bookkeeping.
func (b *Broadcaster) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
