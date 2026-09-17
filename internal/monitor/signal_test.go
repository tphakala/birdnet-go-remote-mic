package monitor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// nameGarden is the device name most tests use.
const nameGarden = "garden"

// recPub is a recording notify.Publisher that mimics the Center's active-key
// idempotency so Clear and Resolve only fire for a key that is actually active.
// It is mutex-guarded because the RunSignal integration test drives it from the
// hub sampler goroutine; the table tests call the monitor directly.
type recPub struct {
	mu        sync.Mutex
	onsets    []notify.Notification
	clears    []string
	clearMsgs map[string]string // last clear message per key
	resolves  []resolveCall
	active    map[string]bool
}

type resolveCall struct{ key, reason string }

func newRecPub() *recPub { return &recPub{clearMsgs: map[string]string{}, active: map[string]bool{}} }

func (r *recPub) Publish(notify.Notification) {}

//nolint:gocritic // Notification by value is the Publisher contract; the fake must match it.
func (r *recPub) Onset(n notify.Notification) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[n.Key] {
		return false
	}
	r.active[n.Key] = true
	r.onsets = append(r.onsets, n)
	return true
}

//nolint:gocritic // Notification by value is the Publisher contract; the fake must match it.
func (r *recPub) Clear(key string, n notify.Notification) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active[key] {
		return false
	}
	delete(r.active, key)
	r.clears = append(r.clears, key)
	r.clearMsgs[key] = n.Message
	return true
}

func (r *recPub) Resolve(key, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active[key] {
		return false
	}
	delete(r.active, key)
	r.resolves = append(r.resolves, resolveCall{key, reason})
	return true
}

func (r *recPub) onsetCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for i := range r.onsets {
		if r.onsets[i].Key == key {
			n++
		}
	}
	return n
}

func (r *recPub) isActive(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[key]
}

func (r *recPub) clearCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, k := range r.clears {
		if k == key {
			n++
		}
	}
	return n
}

// clearMessage returns the message of the last Clear recorded for key.
func (r *recPub) clearMessage(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clearMsgs[key]
}

func (r *recPub) resolveCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rc := range r.resolves {
		if rc.key == key {
			n++
		}
	}
	return n
}

// onsetMessage returns the message of the last onset for key.
func (r *recPub) onsetMessage(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := ""
	for i := range r.onsets {
		if r.onsets[i].Key == key {
			msg = r.onsets[i].Message
		}
	}
	return msg
}

// lastOnset returns the most recent onset recorded for key.
func (r *recPub) lastOnset(key string) (notify.Notification, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out notify.Notification
	ok := false
	for i := range r.onsets {
		if r.onsets[i].Key == key {
			out = r.onsets[i]
			ok = true
		}
	}
	return out, ok
}

// clk is a fake monotonic clock the monitor evaluates hysteresis against.
type clk struct{ t time.Time }

func (c *clk) now() time.Time          { return c.t }
func (c *clk) at(d time.Duration)      { c.t = c.base().Add(d) }
func (c *clk) base() time.Time         { return time.Unix(3_000_000, 0) }
func (c *clk) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClk() *clk                     { return &clk{t: time.Unix(3_000_000, 0)} }

// dev builds a device's levels from per-channel peak dBFS (RMS mirrors peak).
func dev(name string, peaks ...float64) levels.DeviceLevels {
	chans := make([]levels.ChannelLevels, len(peaks))
	for i, pk := range peaks {
		chans[i] = levels.ChannelLevels{Channel: i, PeakDbfs: pk, RmsDbfs: pk}
	}
	return levels.DeviceLevels{Name: name, Channels: chans}
}

// devClip builds a mono device that clipped this window, at the given peak.
func devClip(name string, clipped bool, peak float64) levels.DeviceLevels {
	return levels.DeviceLevels{
		Name:     name,
		Channels: []levels.ChannelLevels{{Channel: 0, PeakDbfs: peak, RmsDbfs: peak, Clipped: clipped}},
	}
}

func evt(devs ...levels.DeviceLevels) levels.LevelsEvent {
	return levels.LevelsEvent{Devices: devs}
}

// baseSettings returns an enabled Signal Settings with the default thresholds.
func baseSettings() Settings {
	return Settings{
		Enabled: true,
		Audio: config.AudioAlerts{
			QuietDbfs: p(-60), QuietSeconds: p(600), ZeroSeconds: p(30),
			ClipPercent: p(20), ClipWindowSeconds: p(10),
		},
		QuietAlert: map[string]bool{},
	}
}

//nolint:gocritic // test helper: taking Settings by value keeps call sites terse.
func newSignalT(rec *recPub, s Settings, c *clk) *Signal {
	return NewSignal(rec, &s, WithClock(c.now))
}

func TestSignalZeroOnsetAndClear(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	key := audioZeroKey(nameGarden)

	// At digital zero (all channels at the floor). The onset run starts here.
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.onsetCount(key) != 0 {
		t.Fatalf("premature zero onset")
	}
	// 30 s later, still at zero: onset.
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.onsetCount(key) != 1 {
		t.Fatalf("zero onset after 30 s = %d, want 1", rec.onsetCount(key))
	}
	if msg := rec.onsetMessage(key); msg == "" {
		t.Error("zero onset message empty")
	}
	// Signal returns: the clear run starts.
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, -20)))
	if rec.clearCount(key) != 0 {
		t.Fatalf("premature zero clear")
	}
	// 5 s of non-zero clears it.
	c.advance(5 * time.Second)
	sig.observe(evt(dev(nameGarden, -20)))
	if rec.clearCount(key) != 1 {
		t.Fatalf("zero clear = %d, want 1", rec.clearCount(key))
	}
}

func TestSignalQuietOnsetAndClear(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	key := audioQuietKey(nameGarden)

	// Peak -70 dBFS is below the -60 threshold but not at the floor, so quiet is
	// pending and zero is not.
	sig.observe(evt(dev(nameGarden, -70)))
	c.advance(600 * time.Second)
	sig.observe(evt(dev(nameGarden, -70)))
	if rec.onsetCount(key) != 1 {
		t.Fatalf("quiet onset after 10 min = %d, want 1", rec.onsetCount(key))
	}
	if rec.onsetCount(audioZeroKey(nameGarden)) != 0 {
		t.Error("a -70 dBFS input must not raise stuck-at-zero")
	}
	// Level recovers above the threshold: clear after 30 s.
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, -30)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, -30)))
	if rec.clearCount(key) != 1 {
		t.Fatalf("quiet clear = %d, want 1", rec.clearCount(key))
	}
}

func TestSignalClipOnsetAndClear(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(4) // short window keeps the test to a few steps
	s.Audio.ClipPercent = p(20)
	sig := newSignalT(rec, s, c)
	key := audioClipKey(nameGarden)

	// Clip every window at 1 s spacing. The ratio is only judged once a full 4 s
	// window has been observed, so onset lands on the window at t0+4 s.
	for i := 0; i <= 4; i++ {
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, true, -1)))
	}
	if rec.onsetCount(key) != 1 {
		t.Fatalf("clip onset = %d, want 1", rec.onsetCount(key))
	}
	// Clipping stops; clears after 30 s with no clipped window.
	c.at(5 * time.Second)
	sig.observe(evt(devClip(nameGarden, false, -6)))
	if rec.clearCount(key) != 0 {
		t.Fatal("premature clip clear")
	}
	c.at(4*time.Second + 30*time.Second) // 30 s since the last clipped window (t0+4s)
	sig.observe(evt(devClip(nameGarden, false, -6)))
	if rec.clearCount(key) != 1 {
		t.Fatalf("clip clear = %d, want 1", rec.clearCount(key))
	}
}

func TestSignalNoRepeatsWhileActive(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	key := audioZeroKey(nameGarden)

	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	// Keep feeding zero: the condition stays active and must not re-onset.
	for i := 0; i < 5; i++ {
		c.advance(10 * time.Second)
		sig.observe(evt(dev(nameGarden, floorDbfs)))
	}
	if rec.onsetCount(key) != 1 {
		t.Fatalf("zero onset count while held = %d, want 1", rec.onsetCount(key))
	}
}

func TestSignalDeviceDisappearanceResolves(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	key := audioZeroKey(nameGarden)

	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if !rec.isActive(key) {
		t.Fatal("zero not active before disappearance")
	}
	// Absent for one window: still within the grace period.
	c.advance(time.Second)
	sig.observe(evt())
	if rec.resolveCount(key) != 0 {
		t.Fatal("resolved after only one absent window")
	}
	// Absent for a second window: resolve and drop.
	c.advance(time.Second)
	sig.observe(evt())
	if rec.resolveCount(key) != 1 {
		t.Fatalf("resolve after two absent windows = %d, want 1", rec.resolveCount(key))
	}
}

func TestSignalRestartedDeviceStartsFresh(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	key := audioZeroKey(nameGarden)

	// Onset, then let the device disappear so its state is dropped.
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(time.Second)
	sig.observe(evt())
	c.advance(time.Second)
	sig.observe(evt()) // resolved and dropped
	if rec.isActive(key) {
		t.Fatal("condition still active after device dropped")
	}
	// The device reappears at zero: its onset timer starts fresh, so it needs the
	// full 30 s again rather than firing immediately.
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(29 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.isActive(key) {
		t.Error("restarted device onset too early; state was not reset")
	}
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if !rec.isActive(key) {
		t.Error("restarted device did not onset after a fresh 30 s")
	}
}

func TestSignalApplyRaisedQuietThresholdCancelsPendingOnsetKeepsZeroTimer(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)

	// Device q is pending quiet (peak -70 < -60); device z is pending zero (floor).
	sig.observe(evt(dev("q", -70), dev("z", floorDbfs)))

	// Raise the quiet threshold below q's level so quiet no longer holds. Apply
	// only swaps the pointer; the tap applies it on the next window.
	s := baseSettings()
	s.Audio.QuietDbfs = p(-80)
	sig.Apply(&s)

	// At 30 s: z's zero onsets (its timer was untouched by Apply), while q's quiet
	// run was cancelled by the raised threshold and never onsets.
	c.advance(30 * time.Second)
	sig.observe(evt(dev("q", -70), dev("z", floorDbfs)))
	if !rec.isActive(audioZeroKey("z")) {
		t.Error("zero timer was reset by an unrelated Apply; z did not onset at 30 s")
	}
	// Advance well past the original quiet window: still no quiet onset for q.
	c.advance(600 * time.Second)
	sig.observe(evt(dev("q", -70), dev("z", floorDbfs)))
	if rec.onsetCount(audioQuietKey("q")) != 0 {
		t.Errorf("quiet onset for q = %d, want 0 (raised threshold)", rec.onsetCount(audioQuietKey("q")))
	}
}

func TestSignalApplyDisabledResolvesAllAndRaisesNothing(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	key := audioZeroKey(nameGarden)

	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if !rec.isActive(key) {
		t.Fatal("zero not active before disable")
	}
	// Disable the monitors: the next window resolves every active condition.
	s := baseSettings()
	s.Enabled = false
	sig.Apply(&s)
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.resolveCount(key) != 1 {
		t.Fatalf("disable resolve = %d, want 1", rec.resolveCount(key))
	}
	// While disabled, further zero windows raise nothing.
	c.advance(60 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.onsetCount(key) != 1 { // only the pre-disable onset
		t.Errorf("onset while disabled = %d, want 1 (no new onset)", rec.onsetCount(key))
	}
	// A device disappearing while disabled is dropped without a second resolve.
	c.advance(time.Second)
	sig.observe(evt())
	c.advance(time.Second)
	sig.observe(evt())
	if rec.resolveCount(key) != 1 {
		t.Errorf("resolve while disabled = %d, want 1 (no double resolve)", rec.resolveCount(key))
	}
}

func TestSignalQuietOptOutMidConditionResolvesQuietOnly(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.QuietAlert = map[string]bool{nameGarden: true}
	sig := newSignalT(rec, s, c)

	// A floor input raises both zero (at 30 s) and quiet (at 600 s).
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(600 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if !rec.isActive(audioZeroKey(nameGarden)) || !rec.isActive(audioQuietKey(nameGarden)) {
		t.Fatalf("expected both zero and quiet active; zero=%v quiet=%v",
			rec.isActive(audioZeroKey(nameGarden)), rec.isActive(audioQuietKey(nameGarden)))
	}
	// Turn off the device's quiet opt-out: only the quiet condition resolves.
	s2 := baseSettings()
	s2.QuietAlert = map[string]bool{nameGarden: false}
	sig.Apply(&s2)
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.resolveCount(audioQuietKey(nameGarden)) != 1 {
		t.Errorf("quiet resolve = %d, want 1", rec.resolveCount(audioQuietKey(nameGarden)))
	}
	if !rec.isActive(audioZeroKey(nameGarden)) {
		t.Error("zero was resolved by a quiet opt-out change; it must stay active")
	}
}

func TestSignalHelpers(t *testing.T) {
	if intVal(nil) != 0 {
		t.Error("intVal(nil) != 0")
	}
	if intVal(p(7)) != 7 {
		t.Error("intVal(p(7)) != 7")
	}
	if allChannelsAtFloor(&levels.DeviceLevels{}) {
		t.Error("a device with no channels must not read as all-at-floor")
	}
	if !allChannelsAtFloor(&levels.DeviceLevels{Channels: []levels.ChannelLevels{{PeakDbfs: floorDbfs}, {PeakDbfs: floorDbfs}}}) {
		t.Error("every channel at the floor must read as all-at-floor")
	}
	if allChannelsAtFloor(&levels.DeviceLevels{Channels: []levels.ChannelLevels{{PeakDbfs: floorDbfs}, {PeakDbfs: -10}}}) {
		t.Error("one non-floor channel means not all-at-floor")
	}
	if got := maxPeak(&levels.DeviceLevels{}); got != floorDbfs {
		t.Errorf("maxPeak(no channels) = %v, want floor", got)
	}
	if got := maxPeak(&levels.DeviceLevels{Channels: []levels.ChannelLevels{{PeakDbfs: -50}, {PeakDbfs: -20}}}); got != -20 {
		t.Errorf("maxPeak = %v, want -20", got)
	}
	if !anyClipped(&levels.DeviceLevels{Channels: []levels.ChannelLevels{{Clipped: false}, {Clipped: true}}}) {
		t.Error("anyClipped should be true when a channel clipped")
	}
	if anyClipped(&levels.DeviceLevels{Channels: []levels.ChannelLevels{{Clipped: false}}}) {
		t.Error("anyClipped should be false when no channel clipped")
	}

	set := &Settings{QuietAlert: map[string]bool{"off": false, "on": true}}
	if !quietArmed(set, "absent") {
		t.Error("a device absent from the map should default armed")
	}
	if quietArmed(set, "off") {
		t.Error("an explicitly disarmed device should not be armed")
	}
	if !quietArmed(set, "on") {
		t.Error("an explicitly armed device should be armed")
	}

	for _, tc := range []struct {
		sec  int
		want string
	}{
		{1, "1 second"}, {45, "45 seconds"}, {60, "1 minute"}, {90, "90 seconds"},
		{600, "10 minutes"}, {3600, "1 hour"}, {7200, "2 hours"},
	} {
		if got := humanDuration(tc.sec); got != tc.want {
			t.Errorf("humanDuration(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}

// raiseAllKinds drives sig so device z holds both zero and quiet, and device c
// holds clip. It expects a Settings with a 4 s clip window.
func raiseAllKinds(t *testing.T, sig *Signal, rec *recPub, c *clk) {
	t.Helper()
	for i := 0; i <= 4; i++ {
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(dev("z", floorDbfs), devClip("c", true, -1)))
	}
	// Jump past the 10 min quiet window: z raises zero (30 s) and quiet (600 s); c
	// keeps clipping so its condition stays raised.
	c.at(600 * time.Second)
	sig.observe(evt(dev("z", floorDbfs), devClip("c", true, -1)))
	if !rec.isActive(audioZeroKey("z")) || !rec.isActive(audioQuietKey("z")) || !rec.isActive(audioClipKey("c")) {
		t.Fatalf("setup: zero=%v quiet=%v clip=%v, want all active",
			rec.isActive(audioZeroKey("z")), rec.isActive(audioQuietKey("z")), rec.isActive(audioClipKey("c")))
	}
}

func TestSignalResolvesEveryConditionKindOnDisable(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(4)
	sig := newSignalT(rec, s, c)
	raiseAllKinds(t, sig, rec, c)

	off := s
	off.Enabled = false
	sig.Apply(&off)
	c.advance(time.Second)
	sig.observe(evt(dev("z", floorDbfs), devClip("c", true, -1)))

	if rec.resolveCount(audioZeroKey("z")) != 1 || rec.resolveCount(audioQuietKey("z")) != 1 || rec.resolveCount(audioClipKey("c")) != 1 {
		t.Fatalf("disable resolves: zero=%d quiet=%d clip=%d, want 1 each",
			rec.resolveCount(audioZeroKey("z")), rec.resolveCount(audioQuietKey("z")), rec.resolveCount(audioClipKey("c")))
	}
}

func TestSignalResolvesEveryConditionKindOnDisappearance(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(4)
	sig := newSignalT(rec, s, c)
	raiseAllKinds(t, sig, rec, c)

	c.advance(time.Second)
	sig.observe(evt())
	c.advance(time.Second)
	sig.observe(evt()) // second absent window: resolve and drop

	if rec.resolveCount(audioZeroKey("z")) != 1 || rec.resolveCount(audioQuietKey("z")) != 1 || rec.resolveCount(audioClipKey("c")) != 1 {
		t.Fatalf("disappearance resolves: zero=%d quiet=%d clip=%d, want 1 each",
			rec.resolveCount(audioZeroKey("z")), rec.resolveCount(audioQuietKey("z")), rec.resolveCount(audioClipKey("c")))
	}
}

func TestSignalReconcileStaysDisabled(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	off := baseSettings()
	off.Enabled = false
	sig := newSignalT(rec, off, c)
	sig.observe(evt(dev(nameGarden, floorDbfs))) // first window records the disabled settings
	// Apply a second, distinct disabled Settings: reconcile sees prev disabled and
	// set disabled with a changed pointer, hitting the still-disabled short-circuit.
	off2 := off
	sig.Apply(&off2)
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.onsetCount(audioZeroKey(nameGarden)) != 0 {
		t.Error("a disabled monitor must not onset")
	}
}

func TestSignalNilPublisherDoesNotPanic(t *testing.T) {
	c := newClk()
	s := baseSettings()
	// A nil Publisher must be normalized to a no-op, so driving a condition to
	// onset on the sampler goroutine cannot panic.
	sig := NewSignal(nil, &s, WithClock(c.now))
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs))) // would Onset; must be a no-op, not a panic
}

func TestSignalOnsetContent(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	c.advance(30 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	on, ok := rec.lastOnset(audioZeroKey(nameGarden))
	if !ok {
		t.Fatal("no zero onset recorded")
	}
	if on.Severity != notify.SeverityWarning {
		t.Errorf("zero onset severity = %q, want warning", on.Severity)
	}
	if on.Category != notify.CategoryAudio {
		t.Errorf("zero onset category = %q, want audio", on.Category)
	}
	if on.Title != "No signal" {
		t.Errorf("zero onset title = %q, want %q", on.Title, "No signal")
	}
	if on.Source != nameGarden {
		t.Errorf("zero onset source = %q, want %q", on.Source, nameGarden)
	}
}

// TestSignalClipGateWaitsForFullWindow pins the "full window observed" gate: a
// 100%-clipped ratio over a few samples must not onset before the window has
// spanned a full clip window.
func TestSignalClipGateWaitsForFullWindow(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(4)
	s.Audio.ClipPercent = p(20)
	sig := newSignalT(rec, s, c)
	key := audioClipKey(nameGarden)
	for i := 0; i <= 3; i++ {
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, true, -1)))
		if rec.onsetCount(key) != 0 {
			t.Fatalf("clip onset at %ds before the window filled = %d, want 0", i, rec.onsetCount(key))
		}
	}
	c.at(4 * time.Second)
	sig.observe(evt(devClip(nameGarden, true, -1)))
	if rec.onsetCount(key) != 1 {
		t.Fatalf("clip onset once the window filled = %d, want 1", rec.onsetCount(key))
	}
}

// TestSignalClipRatioBoundary pins the >= onset boundary: with ClipPercent=40 and
// a 5-sample window, exactly 2/5 (40%) onsets and 1/5 (20%) does not.
func TestSignalClipRatioBoundary(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(4)
	s.Audio.ClipPercent = p(40)
	sig := newSignalT(rec, s, c)
	key := audioClipKey(nameGarden)
	for i, cl := range []bool{true, false, false, false, false} { // 1/5 = 20%
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, cl, -1)))
	}
	if rec.onsetCount(key) != 0 {
		t.Fatalf("clip onset at 20%% = %d, want 0", rec.onsetCount(key))
	}
	c.at(5 * time.Second) // window t1..t5 = still 1/5
	sig.observe(evt(devClip(nameGarden, true, -1)))
	if rec.onsetCount(key) != 0 {
		t.Fatalf("clip onset still at 20%% = %d, want 0", rec.onsetCount(key))
	}
	c.at(6 * time.Second) // window t2..t6 = 2/5 = 40% exactly
	sig.observe(evt(devClip(nameGarden, true, -1)))
	if rec.onsetCount(key) != 1 {
		t.Fatalf("clip onset at exactly 40%% = %d, want 1 (>= boundary)", rec.onsetCount(key))
	}
}

// TestSignalClipNoReflapWithLongWindow guards the high-severity fix: when the
// clip window is longer than the constant clear dwell, the pre-clear clipped
// samples must be flushed on clear so the condition does not immediately re-onset
// and flap.
func TestSignalClipNoReflapWithLongWindow(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(60) // longer than the 30s clear dwell
	s.Audio.ClipPercent = p(20)
	sig := newSignalT(rec, s, c)
	key := audioClipKey(nameGarden)
	for i := 0; i <= 60; i++ {
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, true, -1)))
	}
	if rec.onsetCount(key) != 1 {
		t.Fatalf("clip onset = %d, want 1", rec.onsetCount(key))
	}
	// Stop clipping: it clears 30s after the last clipped window and must NOT
	// re-onset from the stale clipped samples still inside the 60s window.
	for i := 61; i <= 100; i++ {
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, false, -20)))
	}
	if rec.clearCount(key) != 1 {
		t.Fatalf("clip clear = %d, want 1", rec.clearCount(key))
	}
	if rec.onsetCount(key) != 1 {
		t.Errorf("clip re-onset after clear = %d, want 1 (no flap)", rec.onsetCount(key))
	}
	if rec.isActive(key) {
		t.Error("clip still active after clear + quiet, want cleared")
	}
}

// TestSignalClipWindowLengthenDefersOnset checks that lengthening the clip window
// via Apply mid-run defers onset until the new, longer window has been observed,
// rather than judging the ratio over the small retained sample set.
func TestSignalClipWindowLengthenDefersOnset(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.Audio.ClipWindowSeconds = p(4)
	s.Audio.ClipPercent = p(20)
	sig := newSignalT(rec, s, c)
	key := audioClipKey(nameGarden)
	for i := 0; i <= 4; i++ { // observe a full short window, no clipping
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, false, -20)))
	}
	s2 := s
	s2.Audio.ClipWindowSeconds = p(30)
	sig.Apply(&s2)
	c.at(5 * time.Second)
	sig.observe(evt(devClip(nameGarden, true, -1)))
	if rec.onsetCount(key) != 0 {
		t.Fatalf("clip onset right after lengthening the window = %d, want 0", rec.onsetCount(key))
	}
	for i := 6; i <= 35; i++ {
		c.at(time.Duration(i) * time.Second)
		sig.observe(evt(devClip(nameGarden, true, -1)))
	}
	if rec.onsetCount(key) != 1 {
		t.Fatalf("clip onset after the lengthened window fills = %d, want 1", rec.onsetCount(key))
	}
}

// TestSignalApplyShortensZeroSecondsMidRun checks that a hot-reloaded onset
// duration applies to a running timer (via SetEnterAfter), so shortening
// zero_seconds mid-pending-run onsets earlier.
func TestSignalApplyShortensZeroSecondsMidRun(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c) // ZeroSeconds default 30
	key := audioZeroKey(nameGarden)
	sig.observe(evt(dev(nameGarden, floorDbfs))) // start the zero run at t0
	s := baseSettings()
	s.Audio.ZeroSeconds = p(10)
	sig.Apply(&s)
	c.advance(10 * time.Second)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if rec.onsetCount(key) != 1 {
		t.Fatalf("zero onset after a shortened 10s dwell = %d, want 1 (SetEnterAfter not applied via Apply)", rec.onsetCount(key))
	}
}

// TestSignalQuietOptOutResetsPendingRun guards the fix for a stale pending quiet
// run: disarming a device's quiet opt-out while its run is pending (not yet
// active) must reset the run, so a later re-arm starts a fresh dwell instead of
// onsetting immediately off the stale start time.
func TestSignalQuietOptOutResetsPendingRun(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	s := baseSettings()
	s.QuietAlert = map[string]bool{nameGarden: true}
	sig := newSignalT(rec, s, c)
	key := audioQuietKey(nameGarden)

	sig.observe(evt(dev(nameGarden, -70))) // start a pending quiet run at t0
	c.advance(300 * time.Second)           // halfway through the 600s dwell
	sig.observe(evt(dev(nameGarden, -70)))
	if rec.isActive(key) {
		t.Fatal("quiet onset too early")
	}
	off := s
	off.QuietAlert = map[string]bool{nameGarden: false}
	sig.Apply(&off)
	c.advance(time.Second)
	sig.observe(evt(dev(nameGarden, -70))) // disarmed: quiet not evaluated, run reset
	on := s
	on.QuietAlert = map[string]bool{nameGarden: true}
	sig.Apply(&on)
	c.advance(305 * time.Second) // start a fresh run; still short of a full 600s
	sig.observe(evt(dev(nameGarden, -70)))
	if rec.isActive(key) {
		t.Error("quiet onset after only part of a fresh dwell; the pending run was not reset on disarm")
	}
	c.advance(600 * time.Second)
	sig.observe(evt(dev(nameGarden, -70)))
	if !rec.isActive(key) {
		t.Error("quiet did not onset after a full fresh dwell post re-arm")
	}
}

// TestRunSignalDrivesMonitorFromHub wires the monitor to a real hub through a tap
// and drives it end to end, then cancels the context so the tap deregisters.
func TestRunSignalDrivesMonitorFromHub(t *testing.T) {
	hub := levels.NewHub()
	hub.Meter("x", 1) // a registered but silent meter reports the floor each window
	rec := newRecPub()
	s := baseSettings()
	s.Audio.ZeroSeconds = p(0) // onset on the first floor window, no waiting

	ctx, cancel := context.WithCancel(context.Background())
	sig := RunSignal(ctx, hub, rec, &s)
	if sig == nil {
		t.Fatal("RunSignal returned nil")
	}

	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()

	waitFor(t, "zero condition raised through the hub tap", func() bool {
		return rec.isActive(audioZeroKey("x"))
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hub.Run did not stop after cancel")
	}
}

// attached reports whether the monitor currently holds a hub tap.
func (s *Signal) attached() bool {
	s.tapMu.Lock()
	defer s.tapMu.Unlock()
	return s.cancel != nil
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunSignalTapFollowsEnabled drives the tap lifecycle against a real hub:
// disabled at start attaches nothing, an enable attaches and evaluates, a disable
// resolves on the next window and then detaches, a re-enable re-attaches, and
// shutdown detaches for good so a later Apply cannot re-attach.
func TestRunSignalTapFollowsEnabled(t *testing.T) {
	hub := levels.NewHub()
	hub.Meter("x", 1)
	rec := newRecPub()
	s := baseSettings()
	s.Audio.ZeroSeconds = p(0)
	s.Enabled = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := RunSignal(ctx, hub, rec, &s)
	// Count the windows the tap actually processes. Wrapping the clock is race-free
	// here: the monitor starts disabled, so no tap is attached and the tap goroutine
	// is not yet reading sig.clock.
	var windows atomic.Int64
	sig.clock = func() time.Time { windows.Add(1); return time.Now() }

	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()

	if sig.attached() {
		t.Fatal("disabled monitor attached a tap at start")
	}

	on := s
	on.Enabled = true
	sig.Apply(&on)
	if !sig.attached() {
		t.Fatal("enable did not attach the tap")
	}
	// A second Apply while already attached must not attach a second tap: attach is
	// idempotent. A leaked second tap would double the window count per hub tick.
	sig.Apply(&on)
	if !sig.attached() {
		t.Fatal("second enable dropped the tap")
	}
	waitFor(t, "zero onset after enable", func() bool { return rec.isActive(audioZeroKey("x")) })

	off := on
	off.Enabled = false
	sig.Apply(&off)
	waitFor(t, "detach after disable", func() bool { return !sig.attached() })
	if rec.resolveCount(audioZeroKey("x")) != 1 {
		t.Fatalf("disable resolves = %d, want 1 before detaching", rec.resolveCount(audioZeroKey("x")))
	}
	// Once detached the tap must stop processing windows entirely, with no leaked
	// second tap from the repeated Apply: sample, wait five hub windows (~500 ms),
	// and expect no further increments.
	settled := windows.Load()
	time.Sleep(500 * time.Millisecond)
	if got := windows.Load(); got != settled {
		t.Errorf("tap processed %d windows after detach, want 0 (leaked tap?)", got-settled)
	}

	sig.Apply(&on)
	if !sig.attached() {
		t.Fatal("re-enable did not re-attach the tap")
	}
	waitFor(t, "zero re-onset after re-enable", func() bool { return rec.onsetCount(audioZeroKey("x")) == 2 })

	cancel()
	waitFor(t, "detach on shutdown", func() bool { return !sig.attached() })
	sig.Apply(&on)
	if sig.attached() {
		t.Error("Apply after shutdown re-attached the tap")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hub.Run did not stop after cancel")
	}
}

// TestSignalApplyWithoutHubNeverAttaches covers a directly driven monitor: Apply
// and a disabled window must not try to attach or detach a nil hub.
func TestSignalApplyWithoutHubNeverAttaches(t *testing.T) {
	rec := newRecPub()
	c := newClk()
	sig := newSignalT(rec, baseSettings(), c)
	s := baseSettings()
	sig.Apply(&s)
	s.Enabled = false
	sig.Apply(&s)
	sig.observe(evt(dev(nameGarden, floorDbfs)))
	// The real guard here is that the Apply and observe calls above did not panic on
	// the nil hub; this assertion cannot fail on its own, because a hubless attach
	// and detach are always no-ops.
	if sig.attached() {
		t.Error("hubless monitor attached a tap")
	}
}

// TestRunSignalDetachRechecksEnabled pins the re-check inside detach(): a detach
// must not remove the tap while the settings are still enabled, since an Apply may
// have re-enabled the monitor since the window that decided to detach. The hub is
// not run, so no tap fires and the re-check is exercised deterministically.
func TestRunSignalDetachRechecksEnabled(t *testing.T) {
	hub := levels.NewHub()
	rec := newRecPub()
	s := baseSettings() // enabled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := RunSignal(ctx, hub, rec, &s)
	if !sig.attached() {
		t.Fatal("enabled monitor did not attach at start")
	}
	sig.detach() // settings still enabled: must be a no-op
	if !sig.attached() {
		t.Error("detach removed the tap while the monitor was still enabled")
	}
}

// panicPub is a recPub that panics once on the first Resolve of a chosen key, to
// simulate a publisher fault mid-resolve (the hub recovers a tap panic in
// production).
type panicPub struct {
	*recPub
	panicKey string
	panicked bool
}

func (p *panicPub) Resolve(key, reason string) bool {
	if key == p.panicKey && !p.panicked {
		p.panicked = true
		panic("simulated publisher panic during resolve")
	}
	return p.recPub.Resolve(key, reason)
}

// TestSignalReconcileRetriesResolveAfterPublisherPanic pins the reconcile ordering
// fix: reconcile must advance s.applied only after the resolve work completes, so a
// recovered publisher panic mid-resolve leaves s.applied unadvanced and the next
// window re-runs the reconcile and finishes the (idempotent) resolves. Before the
// fix s.applied advanced first, so the retry saw prev==set and skipped the
// remaining resolves, pinning a condition active forever.
func TestSignalReconcileRetriesResolveAfterPublisherPanic(t *testing.T) {
	rec := &panicPub{recPub: newRecPub(), panicKey: audioQuietKey("b")}
	c := newClk()
	s := baseSettings()
	s.Audio.ZeroSeconds = p(0)  // zero onsets on the first floor window
	s.Audio.QuietSeconds = p(0) // quiet onsets on the first quiet window
	sig := NewSignal(rec, &s, WithClock(c.now))

	// Onset zero on device a and quiet on device b under the enabled settings.
	sig.observe(evt(dev("a", floorDbfs), dev("b", -70)))
	if !rec.isActive(audioZeroKey("a")) || !rec.isActive(audioQuietKey("b")) {
		t.Fatalf("setup: want zero(a) and quiet(b) active, got zero=%v quiet=%v",
			rec.isActive(audioZeroKey("a")), rec.isActive(audioQuietKey("b")))
	}
	enabled := sig.applied // the currently applied (enabled) settings pointer

	// Disable: the reconcile resolves everything this monitor owns. The publisher
	// panics once on b's quiet Resolve; recover it here the way the hub's tap
	// delivery does in production.
	off := s
	off.Enabled = false
	func() {
		defer func() { _ = recover() }()
		sig.reconcile(&off)
	}()

	// The panic must have left s.applied unadvanced, or the next window would treat
	// the disable as already reconciled and skip the outstanding resolves.
	if sig.applied != enabled {
		t.Fatal("reconcile advanced s.applied past a mid-resolve panic; the retry will skip the remaining resolves")
	}

	// The next window retries: the one-shot panic is spent, so every condition
	// resolves and none is left pinned active.
	sig.reconcile(&off)
	if rec.isActive(audioQuietKey("b")) {
		t.Error("b's quiet condition stayed active after a publisher panic aborted the first resolve run")
	}
	if rec.isActive(audioZeroKey("a")) {
		t.Error("a's zero condition stayed active after the retry")
	}
	// Idempotency: each key resolves exactly once across the aborted run and the
	// retry. resolveCount counts only was-active resolves, so a retry that
	// re-resolves an already-cleared key is a no-op and does not double-publish.
	if got := rec.resolveCount(audioZeroKey("a")); got != 1 {
		t.Errorf("zero(a) resolveCount = %d, want 1 (retry must not double-resolve an already-cleared key)", got)
	}
	if got := rec.resolveCount(audioQuietKey("b")); got != 1 {
		t.Errorf("quiet(b) resolveCount = %d, want 1", got)
	}
}

// reenablePub is a recPub that re-enables the monitor from inside its Resolve, once,
// to drive the reentrant "Apply during the disabled window" path.
type reenablePub struct {
	*recPub
	sig          *Signal
	reenableWith *Settings
}

func (r *reenablePub) Resolve(key, reason string) bool {
	ok := r.recPub.Resolve(key, reason)
	if r.reenableWith != nil {
		set := r.reenableWith
		r.reenableWith = nil
		r.sig.Apply(set) // reentrant: store-before-attach, then detach's re-check keeps the tap
	}
	return ok
}

// TestRunSignalApplyReentrantEnableKeepsTap covers a publisher whose Resolve
// re-enables the monitor from inside the disabled window (reconcile -> resolveAll
// -> Resolve -> Apply(enable)). It pins two properties. First, that the reentrant
// Apply does not deadlock: the tap mutex is not held during Resolve, so Apply can
// take it to re-attach. Second, that the reentrant enable survives the disabled
// window: observe's trailing detach re-checks the stored settings, finds them
// enabled, keeps the tap, and the enable is not lost.
//
// This does NOT cover the concurrent store-vs-attach ordering inside Apply (the
// run-loop-Apply-versus-tap-detach race is a separate concurrency invariant). This
// test is single-goroutine and reentrant, so the Apply here finds the tap already
// attached and its attach is a no-op regardless of the store/attach order. The hub
// is not Run, so the tap never fires and the windows are driven by hand.
func TestRunSignalApplyReentrantEnableKeepsTap(t *testing.T) {
	hub := levels.NewHub()
	s := baseSettings()
	s.Audio.ZeroSeconds = p(0) // zero onsets on the first floor window
	rec := &reenablePub{recPub: newRecPub()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := RunSignal(ctx, hub, rec, &s)
	rec.sig = sig
	if !sig.attached() {
		t.Fatal("enabled monitor did not attach at start")
	}

	sig.observe(evt(dev(nameGarden, floorDbfs)))
	if !rec.isActive(audioZeroKey(nameGarden)) {
		t.Fatal("setup: zero condition did not onset")
	}

	off := s
	off.Enabled = false
	sig.Apply(&off)
	on := s // enabled
	rec.reenableWith = &on
	sig.observe(evt(dev(nameGarden, floorDbfs)))

	if !sig.attached() {
		t.Error("a reentrant Apply(enable) during the disabled window left the tap detached")
	}
	if !sig.set.Load().Enabled {
		t.Error("the reentrant enable was lost")
	}
}
