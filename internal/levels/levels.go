// Package levels measures per-device audio levels and fans them out to
// subscribers as levels events. A cheap peak/RMS tap runs in each capture pump
// (Meter.Observe), a single central sampler reads and resets the meters at a
// fixed cadence, and a fan-out hub broadcasts the result to every subscriber.
// The hub is an sse.Source; the SSE HTTP transport itself lives in internal/sse.
// The package is platform-neutral: it never touches ALSA, the RTSP path, or
// HTTP, only raw S16LE bytes.
package levels

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/sse"
)

// FloorDbfs is the reported minimum; JSON cannot carry negative infinity, so
// silence and anything quieter clamps here. It is exported so the signal monitor
// can compare a channel peak against the same floor (a peak of exactly this value
// is digital zero) instead of re-typing the literal, which would silently break
// that exact-equality check if the floor ever changed.
const FloorDbfs = -99.0

// dbfsFloor is the internal alias for the exported floor.
const dbfsFloor = FloorDbfs

// fullScale is |math.MinInt16|: the divisor that maps a full-scale sample to
// 0 dBFS. A full-scale negative sample (-32768) has magnitude 32768.
const fullScale = 32768.0

// defaultInterval is the levels sampling and emit cadence (10 Hz).
const defaultInterval = 100 * time.Millisecond

// ChannelLevels is one capture channel's audio levels over the last measurement
// window. The JSON tags match the OpenAPI ChannelLevels schema exactly.
type ChannelLevels struct {
	Channel  int     `json:"channel"`
	PeakDbfs float64 `json:"peakDbfs"`
	RmsDbfs  float64 `json:"rmsDbfs"`
	Clipped  bool    `json:"clipped"`
}

// DeviceLevels is one device's audio levels over the last measurement window,
// one entry per captured channel (a mono device carries a single element). The
// JSON tags match the OpenAPI DeviceLevels schema exactly.
type DeviceLevels struct {
	Name     string          `json:"name"`
	Channels []ChannelLevels `json:"channels"`
}

// LevelsEvent is the payload of one levels SSE event: levels for every device
// with a meter. The JSON tags match the OpenAPI LevelsEvent schema.
type LevelsEvent struct {
	Devices []DeviceLevels `json:"devices"`
}

// Meter accumulates peak and RMS for one device over a measurement window. The
// device's capture goroutine calls Observe; the hub's single sampler calls
// sample. All shared state is atomic so no lock sits on the capture hot path.
// subs is shared with the hub: when no client is subscribed, Observe returns
// immediately, so idle metering costs nothing and the accumulators cannot grow.
type Meter struct {
	subs *atomic.Int32
	ch   []chanAccum // one accumulator per capture channel, fixed at creation
}

// chanAccum holds one channel's atomic accumulators for the current window. It
// must never be copied (it holds atomics); the Meter owns a fixed-length slice
// of them, indexed by capture channel, allocated once at Meter creation.
type chanAccum struct {
	peak    atomic.Uint32 // max |sample| this window
	sumSq   atomic.Uint64 // sum of sample^2 this window
	count   atomic.Uint64 // samples this window
	clipped atomic.Bool   // any full-scale sample this window
}

// Observe folds one interleaved S16LE period into the per-channel accumulators.
// It deinterleaves by the meter's channel count (frame f of channel c sits at
// interleaved index f*nch+c) and accumulates into stack locals per channel,
// touching the atomics only once per channel per period rather than per sample.
// It runs on the capture pump's OS thread, so it stays allocation-free and skips
// all work when no client is watching.
func (m *Meter) Observe(pcm []byte) {
	if m.subs != nil && m.subs.Load() == 0 {
		return
	}
	nch := len(m.ch)
	if nch == 0 {
		return
	}
	frames := (len(pcm) / 2) / nch
	if frames == 0 {
		return
	}
	for c := 0; c < nch; c++ {
		var peak uint32
		var sumSq uint64
		clipped := false
		for f := 0; f < frames; f++ {
			s := int16(binary.LittleEndian.Uint16(pcm[(f*nch+c)*2:]))
			if a := abs16(s); a > peak {
				peak = a
			}
			sumSq += uint64(int64(s) * int64(s))
			if s == math.MaxInt16 || s == math.MinInt16 {
				clipped = true
			}
		}
		acc := &m.ch[c]
		acc.count.Add(uint64(frames))
		acc.sumSq.Add(sumSq)
		for {
			cur := acc.peak.Load()
			if peak <= cur {
				break
			}
			if acc.peak.CompareAndSwap(cur, peak) {
				break
			}
		}
		if clipped {
			acc.clipped.Store(true)
		}
	}
}

// sample reads and resets the accumulators and returns the window's per-channel
// levels. The Swaps are not one atomic step, so a sample racing Observe may
// shift a sliver of a channel's energy across its 100 ms window boundary; each
// channel is deinterleaved into its own accumulator, so energy never crosses
// between channels, and there is only ever one sampler, so no window is
// double-counted. That jitter is invisible on a VU meter. It runs on the
// sampler goroutine, not the hot path, so the per-call slice allocation is fine.
func (m *Meter) sample(name string) DeviceLevels {
	chans := make([]ChannelLevels, len(m.ch))
	for c := range m.ch {
		acc := &m.ch[c]
		peak := acc.peak.Swap(0)
		sumSq := acc.sumSq.Swap(0)
		count := acc.count.Swap(0)
		clipped := acc.clipped.Swap(false)
		chans[c] = ChannelLevels{
			Channel:  c,
			PeakDbfs: dbfs(float64(peak) / fullScale),
			RmsDbfs:  rmsDbfs(sumSq, count),
			Clipped:  clipped,
		}
	}
	return DeviceLevels{Name: name, Channels: chans}
}

// reset zeroes every channel's accumulators. It runs under the hub lock,
// mutually excluded with sample, when the first client subscribes so a new
// session does not open on residual left in the meter from a previous one.
func (m *Meter) reset() {
	for c := range m.ch {
		acc := &m.ch[c]
		acc.peak.Store(0)
		acc.sumSq.Store(0)
		acc.count.Store(0)
		acc.clipped.Store(false)
	}
}

// abs16 returns the magnitude of s as a uint32, so that -32768 maps to 32768
// rather than overflowing int16.
func abs16(s int16) uint32 {
	if s < 0 {
		return uint32(-int32(s))
	}
	return uint32(s)
}

// dbfs converts a linear amplitude ratio (0..1) to dBFS, clamped to [-99, 0].
func dbfs(ratio float64) float64 {
	if ratio <= 0 {
		return dbfsFloor
	}
	d := 20 * math.Log10(ratio)
	switch {
	case d < dbfsFloor:
		return dbfsFloor
	case d > 0:
		return 0
	default:
		return d
	}
}

// rmsDbfs converts a sum of squares and a sample count to RMS dBFS.
func rmsDbfs(sumSq, count uint64) float64 {
	if count == 0 {
		return dbfsFloor
	}
	rms := math.Sqrt(float64(sumSq)/float64(count)) / fullScale
	return dbfs(rms)
}

// Event is one SSE event. It aliases sse.Event so the hub satisfies sse.Source
// and existing callers keep using levels.Event unchanged; the wire format and
// JSON contract are untouched.
type Event = sse.Event

// sseBuffer is the per-SSE-subscriber channel depth. A slow client that fills
// it drops levels frames rather than stalling the sampler.
const sseBuffer = 8

// tap is one in-process structured-levels consumer. Unlike a subscriber, a tap
// receives the LevelsEvent value directly on the sampler goroutine (no SSE
// marshal, no channel, no fan-out buffer), so a consumer like the signal monitor
// reads every window's levels allocation-free. A registered tap counts as a
// subscriber, so the meters accumulate and the sampler runs even when no SSE
// client is connected.
type tap struct {
	fn func(LevelsEvent)
}

type namedMeter struct {
	name  string
	meter *Meter
}

// Hub owns the device meters, the SSE fan-out (a shared sse.Broadcaster) and the
// tap set, and the sampler. It is the single reader-resetter of the meters, so
// SSE clients never race each other for a measurement window.
type Hub struct {
	interval time.Duration

	// subs gates the hot path: it counts SSE subscribers plus taps and is read
	// lock-free by the capture path (Meter.Observe) and the sampler.
	subs atomic.Int32
	// bc owns the SSE subscriber channels, drop-on-full delivery, and cancel.
	bc *sse.Broadcaster

	mu     sync.Mutex
	meters []namedMeter
	// sseCount is the SSE subscriber count under mu. It mirrors the broadcaster's
	// size but lives here so the first-consumer decision and meter reset, and the
	// hasSSE sampler gate, stay atomic with meter sampling under the one hub lock.
	sseCount int
	taps     map[*tap]struct{}
}

// Hub is an sse.Source: it fans marshaled levels events to SSE subscribers. The
// heartbeat lives on the SSE connection, not here.
var _ sse.Source = (*Hub)(nil)

// NewHub returns a hub with the default 10 Hz sampling cadence.
func NewHub() *Hub {
	return &Hub{
		interval: defaultInterval,
		bc:       sse.NewBroadcaster(sseBuffer),
		taps:     make(map[*tap]struct{}),
	}
}

// Meter registers and returns a meter for the named device with one accumulator
// per capture channel. Call it once per device during setup, before Run,
// passing the device's negotiated capture channel count. A count below 1 is
// treated as mono so a caller that has not negotiated yet still gets a usable
// single-channel meter.
func (h *Hub) Meter(name string, channels int) *Meter {
	if channels < 1 {
		channels = 1
	}
	m := &Meter{subs: &h.subs, ch: make([]chanAccum, channels)}
	h.mu.Lock()
	h.meters = append(h.meters, namedMeter{name: name, meter: m})
	h.mu.Unlock()
	return m
}

// RemoveMeter drops the named device's meter so a hot-reloaded device that was
// stopped no longer appears in the levels stream. Removing a name that is not
// registered is a no-op. It is safe to call concurrently with Run and Subscribe.
func (h *Hub) RemoveMeter(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.meters {
		if h.meters[i].name == name {
			h.meters = append(h.meters[:i], h.meters[i+1:]...)
			return
		}
	}
}

// Run drives the sampler until ctx is cancelled. Sampling is skipped while no
// client is subscribed, so an idle appliance does no work. Heartbeats are the
// SSE connection's job (sse.Handler), not the hub's.
func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	// taps is reused across ticks and touched only by this goroutine, so a steady
	// state with a fixed tap set does not allocate the tap slice here.
	var taps []func(LevelsEvent)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.subs.Load() == 0 {
				// Drop references to any now-canceled tap so its closure can be
				// garbage-collected rather than lingering in the reused backing
				// array until the next sampled tick or until Run exits.
				clear(taps)
				taps = taps[:0]
				continue
			}
			ev, refreshed, hasSSE := h.sample(taps)
			taps = refreshed
			// Deliver the structured event to in-process taps first (on this
			// goroutine, outside the hub lock, so a slow tap cannot block
			// RemoveMeter/Subscribe/Tap; each call is panic-isolated so one bad tap
			// does not crash the sampler). Then marshal and fan out to SSE only when
			// a client is actually connected: a registered tap keeps subs>0, so
			// without this gate an appliance with no browser open would marshal a
			// levels event every tick and discard it against an empty subscriber set.
			for _, fn := range taps {
				h.deliverTap(fn, ev)
			}
			if hasSSE {
				h.bc.Broadcast(marshalLevels(ev))
			}
		}
	}
}

// sample builds this window's structured levels event and refreshes dst with a
// snapshot of the registered tap functions, both under one lock so the meter set
// and the tap set are consistent for the window. It appends into dst (reused by
// the sampler goroutine) to avoid a per-tick allocation for the tap slice; the
// returned slice aliases dst's backing array. The devices slice is freshly
// allocated, so it is safe to hand to taps and to marshal after the lock. hasSSE
// reports whether any SSE subscriber is connected, so the caller can skip the
// marshal and broadcast when only an in-process tap is registered.
func (h *Hub) sample(dst []func(LevelsEvent)) (ev LevelsEvent, taps []func(LevelsEvent), hasSSE bool) {
	h.mu.Lock()
	devs := h.sampleDevicesLocked()
	// Zero the reused backing before rebuilding so a shrunk tap set does not retain
	// a canceled callback beyond the new length.
	clear(dst)
	taps = dst[:0]
	for t := range h.taps {
		taps = append(taps, t.fn)
	}
	hasSSE = h.sseCount > 0
	h.mu.Unlock()
	return LevelsEvent{Devices: devs}, taps, hasSSE
}

// deliverTap invokes one tap, recovering from a panic so a misbehaving in-process
// consumer degrades to a dropped window rather than crashing the sampler
// goroutine, which would take the whole appliance (RTSP and management) down with
// it. It runs outside the hub lock.
func (h *Hub) deliverTap(fn func(LevelsEvent), ev LevelsEvent) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("levels: tap panicked, dropping window: %v", r)
		}
	}()
	fn(ev)
}

// sampleDevicesLocked snapshots every meter into a fresh per-device slice. The
// caller holds h.mu.
func (h *Hub) sampleDevicesLocked() []DeviceLevels {
	devs := make([]DeviceLevels, 0, len(h.meters))
	for i := range h.meters {
		devs = append(devs, h.meters[i].meter.sample(h.meters[i].name))
	}
	return devs
}

// marshalLevels renders a structured levels event as the SSE wire event. The
// marshal cannot fail: every field is a clamped finite scalar (dbfs/rmsDbfs keep
// the floats finite, no NaN or Inf) plus a string and a bool.
func marshalLevels(le LevelsEvent) Event {
	data, _ := json.Marshal(le)
	return Event{Name: "levels", Data: data}
}

// Subscribe registers a new SSE client and returns its event channel plus a
// cancel func that unregisters it. The shared sse.Broadcaster owns the channel
// and its never-closed, drop-on-full, idempotent-cancel contract; the hub layers
// on the subscriber gate and the first-consumer meter reset. Registering the
// first consumer of either kind (SSE or tap) resets residual meters so a new
// session does not open on a previous session's accumulation.
//
// The broadcaster registration and the matching sseCount update both happen
// under h.mu (as do the removal and decrement on cancel), so a sampler tick can
// never see the channel registered but uncounted, or counted but unregistered:
// the hasSSE gate and the broadcaster's subscriber set stay consistent. The
// atomic subs hot-path gate is bumped just outside the lock, as before.
func (h *Hub) Subscribe() (events <-chan Event, cancel func()) {
	h.mu.Lock()
	ch, cancelSub := h.bc.Subscribe()
	first := h.sseCount == 0 && len(h.taps) == 0
	h.sseCount++
	if first {
		// The sampler stops draining the meters while no client is subscribed, so
		// clear any residual before this first session starts reading. The reset
		// runs under h.mu (mutually excluded from sample) and completes before the
		// hot-path gate opens on h.subs.Add(1) below, so the sampler cannot read a
		// residual window into the fresh session.
		for i := range h.meters {
			h.meters[i].meter.reset()
		}
	}
	h.mu.Unlock()
	h.subs.Add(1)
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			cancelSub()
			h.sseCount--
			h.mu.Unlock()
			h.subs.Add(-1)
		})
	}
}

// Tap registers an in-process consumer that receives every window's structured
// LevelsEvent directly on the sampler goroutine, and returns an idempotent
// cancel that unregisters it. A tap counts as a subscriber: it increments the
// subscriber gate so the meters accumulate and the sampler runs even with no SSE
// client connected, and registering the first consumer of either kind resets
// residual meters the same way the first SSE subscriber does. The fn runs on the
// sampler goroutine outside the hub lock and must not block (the signal monitor
// reads the event and publishes to the notification center). Because fn runs
// outside h.mu, a tap may call its own cancel from inside fn without deadlocking.
// The event's devices slice is freshly allocated each window; a tap must treat it
// as read-only.
func (h *Hub) Tap(fn func(LevelsEvent)) (cancel func()) {
	t := &tap{fn: fn}
	h.mu.Lock()
	first := h.sseCount == 0 && len(h.taps) == 0
	h.taps[t] = struct{}{}
	if first {
		for i := range h.meters {
			h.meters[i].meter.reset()
		}
	}
	h.mu.Unlock()
	h.subs.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.taps, t)
			h.mu.Unlock()
			h.subs.Add(-1)
		})
	}
}

// The SSE HTTP handler lives in internal/sse; cmd wires sse.Handler(hub, center) onto
// GET /events. The hub is only the levels Source.

// Accumulator measures per-channel RMS over a bounded one-shot capture, such as
// the short probe that picks a new device's default channel. Unlike Meter it
// is single-goroutine and never reset: feed it periods with Add, then read the
// result once with RMSDbfs.
type Accumulator struct {
	sumSq []uint64
	count uint64
}

// NewAccumulator returns an Accumulator for interleaved S16LE audio with the
// given channel count (at least one).
func NewAccumulator(channels int) *Accumulator {
	return &Accumulator{sumSq: make([]uint64, max(1, channels))}
}

// Add folds one interleaved S16LE period into the per-channel sums. A trailing
// partial frame is ignored.
func (a *Accumulator) Add(pcm []byte) {
	nch := len(a.sumSq)
	frames := (len(pcm) / 2) / nch
	for f := 0; f < frames; f++ {
		for c := 0; c < nch; c++ {
			s := int64(int16(binary.LittleEndian.Uint16(pcm[(f*nch+c)*2:])))
			a.sumSq[c] += uint64(s * s)
		}
	}
	a.count += uint64(frames)
}

// RMSDbfs returns each channel's RMS level in dBFS, floored at FloorDbfs (and
// FloorDbfs for every channel when nothing was added).
func (a *Accumulator) RMSDbfs() []float64 {
	out := make([]float64, len(a.sumSq))
	for c, s := range a.sumSq {
		out[c] = rmsDbfs(s, a.count)
	}
	return out
}
