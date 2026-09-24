// Package notify is the appliance's in-memory notification center. It holds a
// bounded ring of discrete history plus a pinned set of active conditions, and
// fans every entry to SSE subscribers as an sse.Event. It is a leaf package: it
// depends only on internal/sse, so both cmd and mgmtserver can import it without
// a cycle. Emitters depend on the Publisher interface, and a nil *Center is a
// no-op so headless wiring and tests need nothing.
package notify

import (
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/sse"
)

// Severity ranks a notification for the UI: error toasts, warning and info are
// badge-only.
type Severity string

// Category groups a notification by the subsystem it concerns.
type Category string

// Kind distinguishes a one-off event from the onset and clear of a condition.
type Kind string

const (
	// SeverityError is a fault that needs attention (a device or config failure).
	SeverityError Severity = "error"
	// SeverityWarning is a degraded state worth surfacing but not urgent.
	SeverityWarning Severity = "warning"
	// SeverityInfo is a routine, informational entry.
	SeverityInfo Severity = "info"
)

const (
	// CategoryDevice covers capture devices and their open/close lifecycle.
	CategoryDevice Category = "device"
	// CategoryAudio covers the audio signal itself (silence, clipping).
	CategoryAudio Category = "audio"
	// CategoryStream covers RTSP streaming clients.
	CategoryStream Category = "stream"
	// CategorySystem covers host health and appliance lifecycle.
	CategorySystem Category = "system"
	// CategoryConfig covers configuration reloads and auth changes.
	CategoryConfig Category = "config"
)

const (
	// KindEvent is a discrete one-off entry with no active condition.
	KindEvent Kind = "event"
	// KindOnset marks a condition becoming active.
	KindOnset Kind = "onset"
	// KindClear marks a condition becoming inactive.
	KindClear Kind = "clear"
)

// Notification is one entry in the center. ID, BootID, Time and UptimeMs are
// always stamped by the Center at publish time. The condition methods also set
// Kind (Onset sets onset; Clear and Resolve set clear) and Clear sets Key from
// its key argument; a caller of Publish fills every remaining field itself. Key
// identifies a condition (empty for discrete events) and Source names the
// subject (device name, track path, remote address) so the UI can render it as
// a chip.
//
// UptimeMs is the Center's age in milliseconds at publish, read from the
// monotonic clock. Time is wall clock, and a Pi without an RTC can step it by
// hours once NTP syncs, so clients measure condition durations and place
// entries in time from UptimeMs, which a clock step does not move.
type Notification struct {
	ID       uint64    `json:"id"`
	BootID   string    `json:"bootId"`
	Time     time.Time `json:"time"`
	UptimeMs int64     `json:"uptimeMs"`
	Severity Severity  `json:"severity"`
	Category Category  `json:"category"`
	Kind     Kind      `json:"kind"`
	Key      string    `json:"key,omitempty"`
	Source   string    `json:"source,omitempty"`
	Title    string    `json:"title"`
	Message  string    `json:"message"`
}

// Snapshot is the full current state a client bootstraps and re-syncs from: the
// boot identity, the server's wall clock and uptime read at one instant (the
// anchor a client maps entry uptimes onto its own clock with), the ring depth,
// the next ID that will be assigned, and every discrete entry still in the ring
// merged with every active onset (even ones the ring has trimmed), in ascending
// ID order.
type Snapshot struct {
	BootID        string         `json:"bootId"`
	ServerTime    time.Time      `json:"serverTime"`
	UptimeMs      int64          `json:"uptimeMs"`
	Capacity      int            `json:"capacity"`
	NextID        uint64         `json:"nextId"`
	Notifications []Notification `json:"notifications"`
}

// Publisher is the seam emitters depend on. *Center implements it; a nil
// *Center is a no-op (Publish drops, the condition methods report false) so a
// headless build or a test can pass a nil Publisher and emit nothing.
type Publisher interface {
	// Publish records a discrete event.
	Publish(n Notification)
	// Onset records the start of a condition keyed by n.Key; it returns false
	// (and publishes nothing) when that key is already active.
	Onset(n Notification) bool
	// Clear records the end of the condition keyed by key; it returns false (and
	// publishes nothing) when that key is not active.
	Clear(key string, n Notification) bool
	// Resolve clears an active condition whose subject has vanished, publishing
	// an info clear with reason as its message; false when key is not active.
	Resolve(key, reason string) bool
}

const (
	// notificationEvent is the SSE event name every notification streams under.
	notificationEvent = "notification"
	// defaultCapacity is the ring depth: enough discrete history for the web
	// UI's Events page without unbounded growth. History is bounded by count,
	// never by age, and lives in RAM only (a restart starts empty). At a few
	// hundred bytes per entry the full ring stays well under 1 MB. Active
	// conditions are pinned separately.
	// The snapshot reports it as Capacity so the web UI need not pin it; the
	// OpenAPI /notifications description quotes it.
	defaultCapacity = 500
	// subscriberBuffer is the per-subscriber channel depth. It absorbs a startup
	// burst (one entry per configured device plus "started"); a slow client that
	// fills it drops rather than blocking the publisher.
	subscriberBuffer = 32
	// bootIDBytes is the entropy width of a boot id (64 bits, hex-encoded).
	bootIDBytes = 8
)

// randRead is the entropy source for boot ids, a package variable so a test can
// force the fallback path.
var randRead = rand.Read

// Center is the notification center: a bounded ring of discrete history and a
// pinned map of active conditions under one mutex, plus a shared sse.Broadcaster
// for the SSE fan-out. An ID is assigned and the entry broadcast while c.mu is
// held, so every subscriber sees strictly increasing IDs; a slow subscriber
// sees gaps, never reordering. On the publish path the broadcaster's lock is
// taken while c.mu is held (c.mu -> broadcaster mutex), so publishing stays
// ID-ordered; Subscribe takes only the broadcaster's own lock, not c.mu.
type Center struct {
	clock    func() time.Time
	capacity int
	// start is the clock reading at construction. With time.Now it carries a
	// monotonic reading, so clock().Sub(start) is immune to wall-clock steps.
	start time.Time

	mu     sync.Mutex
	bootID string
	nextID uint64
	ring   *ring
	active map[string]Notification
	bc     *sse.Broadcaster
}

// Option configures a Center.
type Option func(*Center)

// WithCapacity sets the discrete-history ring depth (default 500). Values below
// one are ignored.
func WithCapacity(n int) Option {
	return func(c *Center) {
		if n > 0 {
			c.capacity = n
		}
	}
}

// WithClock injects the time source the Center stamps entries with, so tests can
// drive a deterministic clock. A nil clock is ignored.
func WithClock(fn func() time.Time) Option {
	return func(c *Center) {
		if fn != nil {
			c.clock = fn
		}
	}
}

// NewCenter returns a ready center with a fresh random boot id.
func NewCenter(opts ...Option) *Center {
	c := &Center{
		clock:    time.Now,
		capacity: defaultCapacity,
		active:   make(map[string]Notification),
		bc:       sse.NewBroadcaster(subscriberBuffer),
	}
	for _, opt := range opts {
		opt(c)
	}
	c.ring = newRing(c.capacity)
	c.bootID = newBootID()
	c.start = c.clock()
	return c
}

// newBootID returns a random 64-bit hex string. crypto/rand should never fail;
// the fallback derives a value from the clock so a boot id is always present
// (its only job is to differ across restarts, which the timestamp still gives).
func newBootID() string {
	var b [bootIDBytes]byte
	if _, err := randRead(b[:]); err != nil {
		binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}

var (
	_ sse.Source = (*Center)(nil)
	_ Publisher  = (*Center)(nil)
)

// BootID returns the center's boot identity, stable for the process lifetime.
func (c *Center) BootID() string {
	if c == nil {
		return ""
	}
	return c.bootID
}

// Subscribe registers an SSE consumer and returns its event channel plus a
// cancel func that unregisters it. The shared sse.Broadcaster owns the
// subscriber set: channel buffering, drop-on-full delivery, an idempotent
// cancel, and the never-closed-channel contract. Center adds only the
// nil-receiver guard. It satisfies sse.Source.
func (c *Center) Subscribe() (events <-chan sse.Event, cancel func()) {
	if c == nil {
		// Keep the nil-Center contract: a nil channel blocks forever in the
		// handler's select (it never delivers), and cancel is a no-op.
		return nil, func() {}
	}
	return c.bc.Subscribe()
}

// uptimeMs is the Center's age at now in milliseconds. time.Time.Sub uses the
// monotonic readings when both carry one (always, with the default clock), so
// a wall-clock step between start and now does not skew it.
func (c *Center) uptimeMs(now time.Time) int64 {
	return now.Sub(c.start).Milliseconds()
}

// publishLocked stamps n in place with the next ID, the boot id, the clock time
// and the uptime, appends it to the ring, and broadcasts it to every subscriber without
// blocking. The caller holds c.mu and reads the stamped fields back through n
// for the active-map bookkeeping the condition methods do.
func (c *Center) publishLocked(n *Notification) {
	c.nextID++
	n.ID = c.nextID
	n.BootID = c.bootID
	now := c.clock()
	n.Time = now
	n.UptimeMs = c.uptimeMs(now)
	c.ring.push(n)
	// Marshal once. This fails only for a time whose year is outside [0,9999],
	// which a real clock never produces. The entry is already in the ring so the
	// snapshot still serves it, and the stream event is best effort, so on that
	// impossible-in-practice failure just skip the broadcast rather than emit a
	// broken event.
	data, err := json.Marshal(n)
	if err != nil {
		return
	}
	ev := sse.Event{Name: notificationEvent, Data: data}
	// Broadcast while c.mu is held so the fan-out order matches ID order; the
	// broadcaster takes its own lock underneath (c.mu -> broadcaster mutex).
	c.bc.Broadcast(ev)
}

// Publish records a discrete event. A nil Center drops it.
//
//nolint:gocritic // Notification by value is the Publisher contract; publishing is not a hot path.
func (c *Center) Publish(n Notification) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.publishLocked(&n)
	c.mu.Unlock()
}

// Onset records the start of a condition keyed by n.Key. It is idempotent: a
// duplicate onset for an already-active key publishes nothing and returns false,
// so a monitor restart or a re-run reconcile cannot double-enter a condition. An
// empty key, or a nil Center, publishes nothing and returns false.
//
//nolint:gocritic // Notification by value is the Publisher contract; publishing is not a hot path.
func (c *Center) Onset(n Notification) bool {
	if c == nil {
		return false
	}
	// A condition must be keyed: an empty key would collide in the active map and
	// cannot pair with a later Clear. Reject it rather than register active[""].
	if n.Key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, active := c.active[n.Key]; active {
		return false
	}
	n.Kind = KindOnset
	c.publishLocked(&n)
	c.active[n.Key] = n
	return true
}

// Clear records the end of the condition keyed by key. It is idempotent: a clear
// for an inactive key publishes nothing and returns false. A nil Center returns
// false.
//
//nolint:gocritic // Notification by value is the Publisher contract; publishing is not a hot path.
func (c *Center) Clear(key string, n Notification) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	onset, active := c.active[key]
	if !active {
		return false
	}
	delete(c.active, key)
	n.Key = key
	n.Kind = KindClear
	// Category and source are the condition's identity, fixed by its key across
	// onset and clear, so always take them from the onset. This keeps the clear
	// event and snapshot entry consistent with the onset for the same key and
	// never lets a clear carry an empty required category.
	n.Category = onset.Category
	n.Source = onset.Source
	c.publishLocked(&n)
	return true
}

// Resolve clears an active condition whose subject has vanished (a device
// removed by a hot reload), publishing an info clear with reason as its message
// so clients drop the active condition. It reuses the onset's category and
// source. Returns false when key is not active; a nil Center returns false.
func (c *Center) Resolve(key, reason string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	onset, active := c.active[key]
	if !active {
		return false
	}
	delete(c.active, key)
	n := Notification{
		Severity: SeverityInfo,
		Category: onset.Category,
		Kind:     KindClear,
		Key:      key,
		Source:   onset.Source,
		Title:    "Cleared",
		Message:  reason,
	}
	c.publishLocked(&n)
	return true
}

// Snapshot returns the full current state: every ring entry merged with any
// active onset the ring has already trimmed, in ascending ID order, plus the
// boot id, the server clock and uptime, the ring depth, and the next ID to be
// assigned.
func (c *Center) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.ring.all()
	present := make(map[uint64]struct{}, len(entries))
	for i := range entries {
		present[entries[i].ID] = struct{}{}
	}
	// Pin active onsets the ring has trimmed so the panel's current-issues view
	// survives a chatty history. An onset still in the ring is not duplicated.
	for k := range c.active {
		onset := c.active[k]
		if _, ok := present[onset.ID]; !ok {
			entries = append(entries, onset)
		}
	}
	slices.SortFunc(entries, func(a, b Notification) int { return cmp.Compare(a.ID, b.ID) })
	// One clock read, so the wall time and the uptime describe the same instant.
	now := c.clock()
	return Snapshot{
		BootID:        c.bootID,
		ServerTime:    now,
		UptimeMs:      c.uptimeMs(now),
		Capacity:      c.capacity,
		NextID:        c.nextID + 1,
		Notifications: entries,
	}
}

// Active returns the onset entry of every currently active condition, in
// ascending ID order. A nil Center returns nil.
func (c *Center) Active() []Notification {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Notification, 0, len(c.active))
	for k := range c.active {
		out = append(out, c.active[k])
	}
	slices.SortFunc(out, func(a, b Notification) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Started builds the first ring entry cmd publishes right after constructing the
// Center: an info system event naming the running version. Publish stamps its
// ID, boot id, time and uptime.
func Started(version string) Notification {
	return Notification{
		Severity: SeverityInfo,
		Category: CategorySystem,
		Kind:     KindEvent,
		Title:    "Appliance started",
		Message:  "Appliance started, version " + version,
	}
}
