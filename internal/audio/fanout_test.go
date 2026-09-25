package audio

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// always builds always-active fan-out streams over the given drop counters.
func always(drops ...*atomic.Uint64) []FanoutStream {
	streams := make([]FanoutStream, len(drops))
	for i, d := range drops {
		streams[i] = FanoutStream{Dropped: d}
	}
	return streams
}

// drainAll reads a consumer to completion, returning the first byte of every
// delivered period in order (the tests tag each period by its first byte).
func drainAll(c Source) []byte {
	var got []byte
	for {
		p, err := c.Read()
		if err != nil {
			return got
		}
		got = append(got, p.Buf[0])
	}
}

func TestFanoutDeliversEveryPeriodToEveryConsumer(t *testing.T) {
	// Three periods, three consumers, all draining: every consumer must see every
	// period in order. Three periods fit the per-consumer buffer, so nothing drops.
	periods := [][]byte{{1, 0}, {2, 0}, {3, 0}}
	src := NewFakeSource(48000, 1, periods)
	drops := []*atomic.Uint64{{}, {}, {}}
	f, cons := NewFanout(src, "dev", always(drops...))
	if len(cons) != 3 {
		t.Fatalf("consumers = %d, want 3", len(cons))
	}

	results := make(chan []byte, len(cons))
	for _, c := range cons {
		go func(c Source) { results <- drainAll(c) }(c)
	}
	if err := f.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := 0; i < len(cons); i++ {
		got := <-results
		if !bytes.Equal(got, []byte{1, 2, 3}) {
			t.Errorf("a consumer got %v, want [1 2 3]", got)
		}
	}
	for i, d := range drops {
		if d.Load() != 0 {
			t.Errorf("consumer %d dropped %d, want 0 (all periods fit the buffer)", i, d.Load())
		}
	}
}

func TestFanoutDropsForAStalledConsumerWithoutBlocking(t *testing.T) {
	// A consumer that never reads fills its bounded queue and then drops the rest.
	// The point is that Run must NOT block on it (a blocking send would deadlock,
	// stalling every sibling and the capture): Run completing within the timeout is
	// the proof that a slow stream cannot stall the shared reader.
	const extra = 20
	n := fanoutBuffer + extra
	periods := make([][]byte, n)
	for i := range periods {
		periods[i] = []byte{byte(i), 0}
	}
	src := NewFakeSource(48000, 1, periods)
	var dropped atomic.Uint64
	f, _ := NewFanout(src, "dev", always(&dropped))

	done := make(chan error, 1)
	go func() { done <- f.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked on a full consumer queue (send was not non-blocking)")
	}
	if got := dropped.Load(); got != extra {
		t.Errorf("dropped = %d, want %d (queue holds %d, the rest drop)", got, extra, fanoutBuffer)
	}
}

// blockingSource blocks in Read until Close is called, then reports io.EOF, to
// exercise Fanout.Close tearing down a reader that is parked on the hardware.
type blockingSource struct {
	rate, ch  int
	release   chan struct{}
	closeOnce sync.Once
}

func (b *blockingSource) Negotiated() (rate, channels int) { return b.rate, b.ch }
func (b *blockingSource) Read() (Period, error) {
	<-b.release
	return Period{}, io.EOF
}
func (b *blockingSource) Close() error {
	b.closeOnce.Do(func() { close(b.release) })
	return nil
}

func TestFanoutCloseUnblocksConsumers(t *testing.T) {
	src := &blockingSource{rate: 48000, ch: 1, release: make(chan struct{})}
	var dropped atomic.Uint64
	f, cons := NewFanout(src, "dev", always(&dropped))

	runErr := make(chan error, 1)
	go func() { runErr <- f.Run() }()
	readErr := make(chan error, 1)
	go func() { _, err := cons[0].Read(); readErr <- err }()

	// Nothing has been delivered; closing the fan-out must unblock both the reader
	// (via the source) and the consumer (via the closed feed).
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, io.EOF) {
			t.Errorf("consumer Read after Close = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer Read did not unblock after Close")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run after Close = %v, want nil (clean EOF)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

// mutatingSource reuses one backing buffer across Reads, mutating it in place
// each time, like the real ALSA capture source. It proves the fan-out copies a
// period before queuing it: without the copy every queued period would alias the
// one buffer and read back as the last value.
type mutatingSource struct {
	buf  []byte
	vals []byte
	idx  int
}

func (m *mutatingSource) Negotiated() (rate, channels int) { return 48000, 1 }
func (m *mutatingSource) Read() (Period, error) {
	if m.idx >= len(m.vals) {
		return Period{}, io.EOF
	}
	m.buf[0] = m.vals[m.idx]
	m.buf[1] = 0
	m.idx++
	return Period{Buf: m.buf, Frames: 1}, nil
}
func (m *mutatingSource) Close() error { return nil }

func TestFanoutCopiesReusedBuffer(t *testing.T) {
	src := &mutatingSource{buf: make([]byte, 2), vals: []byte{1, 2, 3, 4, 5}}
	var dropped atomic.Uint64
	f, cons := NewFanout(src, "dev", always(&dropped))
	// Five periods fit the buffer, so Run queues all five (each a fresh copy of the
	// reused backing) and then closes the feed on EOF.
	if err := f.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drainAll(cons[0])
	if !bytes.Equal(got, []byte{1, 2, 3, 4, 5}) {
		t.Errorf("got %v, want [1 2 3 4 5]; the fan-out aliased the reused capture buffer", got)
	}
	if dropped.Load() != 0 {
		t.Errorf("dropped = %d, want 0", dropped.Load())
	}
}

// errSource returns one period, then a non-EOF error, to exercise Run's
// error-propagation path (distinct from a clean io.EOF stop).
type errSource struct {
	rate, ch int
	err      error
	sent     bool
}

func (e *errSource) Negotiated() (rate, channels int) { return e.rate, e.ch }
func (e *errSource) Read() (Period, error) {
	if !e.sent {
		e.sent = true
		return Period{Buf: []byte{1, 0}, Frames: 1}, nil
	}
	return Period{}, e.err
}
func (e *errSource) Close() error { return nil }

func TestFanoutRunPropagatesNonEOFError(t *testing.T) {
	// A real driver error (not io.EOF) must surface from Run: the pump reports it as
	// the device-failed cause, so swallowing it as nil would report a clean stop for
	// a crashed capture.
	wantErr := errors.New("alsa: read failed")
	src := &errSource{rate: 48000, ch: 1, err: wantErr}
	f, _ := NewFanout(src, "dev", always(new(atomic.Uint64)))
	if err := f.Run(); !errors.Is(err, wantErr) {
		t.Fatalf("Run() = %v, want %v", err, wantErr)
	}
}

// countObserver counts how many periods it is handed, to prove metering runs once
// per period on the shared reader rather than once per consumer.
type countObserver struct{ n atomic.Int64 }

func (o *countObserver) Observe([]byte) { o.n.Add(1) }

func TestFanoutMetersEachPeriodOnce(t *testing.T) {
	periods := [][]byte{{1, 0}, {2, 0}, {3, 0}}
	var obs countObserver
	// The fan-out reads a metered source, so the meter sees every period exactly
	// once no matter how many consumers the period fans out to.
	metered := NewMeteredSource(NewFakeSource(48000, 1, periods), &obs)
	f, cons := NewFanout(metered, "dev", always(new(atomic.Uint64), new(atomic.Uint64)))
	results := make(chan []byte, len(cons))
	for _, c := range cons {
		go func(c Source) { results <- drainAll(c) }(c)
	}
	if err := f.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both consumers receive every period. This pins the delivery loop: deleting the
	// per-consumer send in Run leaves the meter count right but this assertion red.
	for i := 0; i < len(cons); i++ {
		if got := <-results; !bytes.Equal(got, []byte{1, 2, 3}) {
			t.Errorf("a consumer got %v, want [1 2 3]", got)
		}
	}
	// The metered base is read exactly once per period regardless of consumer count;
	// metering N times would mean the fan-out read the source per consumer.
	if got := obs.n.Load(); got != int64(len(periods)) {
		t.Errorf("Observe called %d times, want %d (once per period, not per consumer)", got, len(periods))
	}
}

func TestFanoutIdleConsumerGetsNothing(t *testing.T) {
	t.Parallel()
	// One active and one idle consumer: the active one sees every period's audio,
	// the idle one nothing until the feed ends, so its stage goroutine is never
	// woken.
	periods := [][]byte{{1, 0}, {2, 0}, {3, 0}}
	src := NewFakeSource(48000, 1, periods)
	streams := []FanoutStream{
		{Dropped: new(atomic.Uint64), Active: func() bool { return true }},
		{Dropped: new(atomic.Uint64), Active: func() bool { return false }},
	}
	f, cons := NewFanout(src, "dev", streams)
	if err := f.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := drainAll(cons[0]); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("active consumer got %v, want [1 2 3]", got)
	}
	if p, err := cons[1].Read(); err == nil {
		t.Errorf("idle consumer got a period (%d bytes, %d frames), want none before EOF", len(p.Buf), p.Frames)
	}
}

func TestFanoutIdleConsumerNeverDrops(t *testing.T) {
	t.Parallel()
	// An idle stream's stage reads nothing while it waits, so were the fan-out
	// to queue anything for it, the queue would fill and count drops that feed
	// the host monitor's drop alert for a stream nobody plays.
	dropped := new(atomic.Uint64)
	f, cons := NewFanout(NewFakeSource(48000, 1, nil), "dev",
		[]FanoutStream{{Dropped: dropped, Active: func() bool { return false }}})
	p := Period{Buf: []byte{7, 0}, Frames: 1}
	for range 3 * fanoutBuffer {
		f.distribute(p)
	}
	if got := len(cons[0].(*fanoutConsumer).ch); got != 0 {
		t.Errorf("idle consumer has %d queued periods, want 0", got)
	}
	if got := dropped.Load(); got != 0 {
		t.Errorf("idle consumer dropped %d periods, want 0", got)
	}
}

func TestFanoutFollowsActiveFlag(t *testing.T) {
	t.Parallel()
	// The active flag is consulted per period, so a client that starts playing
	// gets audio from the next period on, and a stream nobody plays is sent
	// nothing.
	var active atomic.Bool
	f, cons := NewFanout(NewFakeSource(48000, 1, nil), "dev",
		[]FanoutStream{{Dropped: new(atomic.Uint64), Active: active.Load}})
	p := Period{Buf: []byte{7, 0}, Frames: 1}
	f.distribute(p)
	active.Store(true)
	f.distribute(p)
	// Check the queue first, so a missing period fails here instead of blocking
	// the Read on a feed that is never closed.
	if got := len(cons[0].(*fanoutConsumer).ch); got != 1 {
		t.Fatalf("got %d queued periods, want 1 (the period after activation)", got)
	}
	if got, _ := cons[0].Read(); got.Frames != 1 || !bytes.Equal(got.Buf, []byte{7, 0}) {
		t.Errorf("period after activation: got %v (%d frames), want [7 0] (1 frame)", got.Buf, got.Frames)
	}
}

func TestFanoutDistributeAllocs(t *testing.T) {
	// Not parallel: AllocsPerRun counts process-wide allocations.
	tests := []struct {
		name   string
		active []bool
		want   float64
	}{
		{"all idle", []bool{false, false}, 0},
		{"one active", []bool{true, false}, 1},
		{"second active", []bool{false, true}, 1},
		{"all active", []bool{true, true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			streams := make([]FanoutStream, len(tt.active))
			for i, a := range tt.active {
				streams[i] = FanoutStream{Dropped: new(atomic.Uint64), Active: func() bool { return a }}
			}
			f, cons := NewFanout(NewFakeSource(48000, 2, nil), "dev", streams)
			p := Period{Buf: make([]byte, 4096), Frames: 1024}
			got := testing.AllocsPerRun(100, func() {
				f.distribute(p)
				// Only an active consumer is sent a period; reading an idle one
				// would block.
				for i, c := range cons {
					if tt.active[i] {
						_, _ = c.Read()
					}
				}
			})
			if got != tt.want {
				t.Errorf("allocs per period = %v, want %v", got, tt.want)
			}
		})
	}
}

// BenchmarkFanoutDistribute measures the per-period fan-out cost for two
// streams of a 384 kHz stereo device (10 ms periods), idle and playing.
func BenchmarkFanoutDistribute(b *testing.B) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "active"
		}
		b.Run(name, func(b *testing.B) {
			on := func() bool { return active }
			f, cons := NewFanout(NewFakeSource(384000, 2, nil), "dev", []FanoutStream{
				{Dropped: new(atomic.Uint64), Active: on},
				{Dropped: new(atomic.Uint64), Active: on},
			})
			p := Period{Buf: make([]byte, 3840*4), Frames: 3840}
			b.ReportAllocs()
			for b.Loop() {
				f.distribute(p)
				if !active {
					continue // an idle consumer is sent nothing to read
				}
				for _, c := range cons {
					_, _ = c.Read()
				}
			}
		})
	}
}
