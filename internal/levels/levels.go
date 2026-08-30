// Package levels measures per-device audio levels and streams them to clients
// over Server-Sent Events. A cheap peak/RMS tap runs in each capture pump
// (Meter.Observe), a single central sampler reads and resets the meters at a
// fixed cadence, and a fan-out hub broadcasts the result to every connected SSE
// client. The package is platform-neutral: it never touches ALSA or the RTSP
// path, only raw S16LE bytes and HTTP.
package levels

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// dbfsFloor is the reported minimum; JSON cannot carry negative infinity, so
// silence and anything quieter clamps here.
const dbfsFloor = -99.0

// fullScale is |math.MinInt16|: the divisor that maps a full-scale sample to
// 0 dBFS. A full-scale negative sample (-32768) has magnitude 32768.
const fullScale = 32768.0

// defaultInterval is the levels sampling and emit cadence (10 Hz).
const defaultInterval = 100 * time.Millisecond

// defaultHeartbeat is how often an idle stream emits a heartbeat so clients can
// detect a dead server and proxies keep the connection open.
const defaultHeartbeat = 15 * time.Second

// DeviceLevels is one device's audio levels over the last measurement window.
// The JSON tags match the OpenAPI DeviceLevels schema exactly.
type DeviceLevels struct {
	Name     string  `json:"name"`
	PeakDbfs float64 `json:"peakDbfs"`
	RmsDbfs  float64 `json:"rmsDbfs"`
	Clipped  bool    `json:"clipped"`
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
	subs    *atomic.Int32
	peak    atomic.Uint32 // max |sample| this window
	sumSq   atomic.Uint64 // sum of sample^2 this window
	count   atomic.Uint64 // samples this window
	clipped atomic.Bool   // any full-scale sample this window
}

// Observe folds one S16LE period into the accumulators. It is a single pass and
// runs on the capture pump's OS thread, so it stays allocation-free and skips
// all work when no client is watching.
func (m *Meter) Observe(pcm []byte) {
	if m.subs != nil && m.subs.Load() == 0 {
		return
	}
	n := len(pcm) / 2
	if n == 0 {
		return
	}
	var peak uint32
	var sumSq uint64
	clipped := false
	for i := 0; i < n; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		if a := abs16(s); a > peak {
			peak = a
		}
		sumSq += uint64(int64(s) * int64(s))
		if s == math.MaxInt16 || s == math.MinInt16 {
			clipped = true
		}
	}
	m.count.Add(uint64(n))
	m.sumSq.Add(sumSq)
	for {
		cur := m.peak.Load()
		if peak <= cur {
			break
		}
		if m.peak.CompareAndSwap(cur, peak) {
			break
		}
	}
	if clipped {
		m.clipped.Store(true)
	}
}

// sample reads and resets the accumulators and returns the window's levels. The
// four Swaps are not one atomic step, so a sample racing Observe may shift a
// sliver of energy across the 100 ms boundary; that jitter is invisible on a VU
// meter and there is only ever one sampler, so no window is double-counted.
func (m *Meter) sample(name string) DeviceLevels {
	peak := m.peak.Swap(0)
	sumSq := m.sumSq.Swap(0)
	count := m.count.Swap(0)
	clipped := m.clipped.Swap(false)
	return DeviceLevels{
		Name:     name,
		PeakDbfs: dbfs(float64(peak) / fullScale),
		RmsDbfs:  rmsDbfs(sumSq, count),
		Clipped:  clipped,
	}
}

// reset zeroes the accumulators. It runs under the hub lock, mutually excluded
// with sample, when the first client subscribes so a new session does not open
// on residual left in the meter from a previous one.
func (m *Meter) reset() {
	m.peak.Store(0)
	m.sumSq.Store(0)
	m.count.Store(0)
	m.clipped.Store(false)
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

// Event is one SSE event: a type name and its already-marshaled JSON data.
type Event struct {
	Name string
	Data []byte
}

type subscriber struct {
	ch chan Event
}

type namedMeter struct {
	name  string
	meter *Meter
}

// Hub owns the device meters, the subscriber set, and the sampler. It is the
// single reader-resetter of the meters, so SSE clients never race each other
// for a measurement window.
type Hub struct {
	interval  time.Duration
	heartbeat time.Duration

	subs atomic.Int32

	mu      sync.Mutex
	meters  []namedMeter
	subList map[*subscriber]struct{}
}

// NewHub returns a hub with the default 10 Hz cadence and 15 s heartbeat.
func NewHub() *Hub {
	return &Hub{
		interval:  defaultInterval,
		heartbeat: defaultHeartbeat,
		subList:   make(map[*subscriber]struct{}),
	}
}

// Meter registers and returns a meter for the named device. Call it once per
// device during setup, before Run.
func (h *Hub) Meter(name string) *Meter {
	m := &Meter{subs: &h.subs}
	h.mu.Lock()
	h.meters = append(h.meters, namedMeter{name: name, meter: m})
	h.mu.Unlock()
	return m
}

// Run drives the sampler until ctx is cancelled. Sampling and heartbeats are
// skipped while no client is subscribed, so an idle appliance does no work.
func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	hb := time.NewTicker(h.heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.subs.Load() == 0 {
				continue
			}
			h.broadcast(h.levelsEvent())
		case <-hb.C:
			if h.subs.Load() == 0 {
				continue
			}
			h.broadcast(Event{Name: "heartbeat", Data: []byte("{}")})
		}
	}
}

// levelsEvent snapshots every meter into a marshaled levels event.
func (h *Hub) levelsEvent() Event {
	h.mu.Lock()
	devs := make([]DeviceLevels, 0, len(h.meters))
	for i := range h.meters {
		devs = append(devs, h.meters[i].meter.sample(h.meters[i].name))
	}
	h.mu.Unlock()
	// Cannot fail: every field is a clamped scalar (dbfs/rmsDbfs keep the floats
	// finite, no NaN or Inf) plus a string and a bool.
	data, _ := json.Marshal(LevelsEvent{Devices: devs})
	return Event{Name: "levels", Data: data}
}

// broadcast sends ev to every subscriber, dropping it for any whose buffer is
// full (a slow client falls behind rather than stalling the sampler).
func (h *Hub) broadcast(ev Event) {
	h.mu.Lock()
	for s := range h.subList {
		select {
		case s.ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
}

// Subscribe registers a new SSE client and returns its event channel plus a
// cancel func that unregisters it. The channel is never closed; cancel just
// removes the subscriber so a late broadcast cannot send on a closed channel.
func (h *Hub) Subscribe() (events <-chan Event, cancel func()) {
	s := &subscriber{ch: make(chan Event, 8)}
	h.mu.Lock()
	first := len(h.subList) == 0
	h.subList[s] = struct{}{}
	if first {
		// The sampler stops draining the meters while no client is subscribed,
		// so clear any residual before this first session starts reading.
		for i := range h.meters {
			h.meters[i].meter.reset()
		}
	}
	h.mu.Unlock()
	h.subs.Add(1)
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subList, s)
			h.mu.Unlock()
			h.subs.Add(-1)
		})
	}
}

// EventsHandler returns the SSE HTTP handler for GET /events. It is mounted
// beside the generated management API handler.
func (h *Hub) EventsHandler() http.Handler {
	return http.HandlerFunc(h.serveEvents)
}

func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	filter := parseEventFilter(r.URL.Query().Get("events"))

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	rc := http.NewResponseController(w)
	ch, cancel := h.Subscribe()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !filter.allows(ev.Name) {
				continue
			}
			// A bounded per-write deadline keeps a stuck client from parking the
			// writer forever without killing the long-lived stream the way the
			// server's WriteTimeout would.
			_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// eventFilter decides which event types a client receives. Heartbeats are
// always delivered so an idle connection stays alive regardless of the filter.
type eventFilter struct {
	all   bool
	names map[string]bool
}

func parseEventFilter(q string) eventFilter {
	if strings.TrimSpace(q) == "" {
		return eventFilter{all: true}
	}
	names := make(map[string]bool)
	for _, p := range strings.Split(q, ",") {
		if p = strings.TrimSpace(p); p != "" {
			names[p] = true
		}
	}
	if len(names) == 0 {
		return eventFilter{all: true}
	}
	return eventFilter{names: names}
}

func (f eventFilter) allows(name string) bool {
	if name == "heartbeat" {
		return true
	}
	return f.all || f.names[name]
}
