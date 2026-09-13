package monitor

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// floorDbfs is the reported minimum from internal/levels: silence and anything
// quieter clamps to exactly this value, so a channel peak of exactly it means
// digital zero (the smallest non-zero 16-bit sample is -90.3 dBFS, so the floor
// is unambiguous). It aliases the exported levels constant so the two cannot
// drift and break the exact-equality comparison in allChannelsAtFloor.
const floorDbfs = levels.FloorDbfs

// Clear-side dwell times are constants: only the onset thresholds and onset
// durations are configurable. A condition must stay gone for these before it
// clears, so a brief recovery does not flap the clear.
const (
	zeroClearAfter  = 5 * time.Second
	quietClearAfter = 30 * time.Second
	clipClearAfter  = 30 * time.Second
	// devicePresenceGrace is how many consecutive windows a device may be missing
	// from the levels event before the monitor resolves its conditions and drops
	// it. Two tolerates a one-window blip during a hot-reload card swap.
	devicePresenceGrace = 2
)

// Condition keys pair a device's onset with its clear across windows.
func audioZeroKey(name string) string  { return "audio:" + name + ":zero" }
func audioQuietKey(name string) string { return "audio:" + name + ":quiet" }
func audioClipKey(name string) string  { return "audio:" + name + ":clip" }

// clipSample records whether one measurement window clipped, with its time, so
// the clip detector can age samples out of the sliding window.
type clipSample struct {
	at      time.Time
	clipped bool
}

// deviceState is one device's condition state. It is touched only by the tap
// goroutine (observe and reconcile run there), so it needs no lock.
type deviceState struct {
	zero  *notify.Hysteresis // stuck-at-digital-zero
	quiet *notify.Hysteresis // sustained very-quiet input

	// clip is the sliding window of recent measurement windows (oldest first);
	// clipActive is the clipping condition's raised state; lastClip is the most
	// recent clipped window (drives the constant clear). The ratio is judged only
	// once the retained window spans a full clip window (see evaluateClip).
	clip       []clipSample
	clipActive bool
	lastClip   time.Time

	// missed counts consecutive windows this device was absent; seen marks it
	// present in the current window.
	missed int
	seen   bool
}

func (st *deviceState) resetClip() {
	st.clip = st.clip[:0]
	st.clipActive = false
	st.lastClip = time.Time{}
}

// Signal is the audio-signal condition monitor. It is fed one structured levels
// event per window through a hub tap and raises stuck-at-zero, very-quiet, and
// clipping conditions per device with hysteresis. Settings are swapped atomically
// by Apply; the tap goroutine reads them each window and performs every state
// change itself, so Apply never races the tap.
type Signal struct {
	pub   notify.Publisher
	clock func() time.Time
	set   atomic.Pointer[Settings]

	// states and applied are owned by the tap goroutine (observe/reconcile).
	states  map[string]*deviceState
	applied *Settings
}

// Signal implements the appliance's Monitors handle.
var _ Monitors = (*Signal)(nil)

// SignalOption configures a Signal.
type SignalOption func(*Signal)

// WithClock injects the time source the monitor evaluates hysteresis against, so
// a test can drive a deterministic clock. A nil clock is ignored.
func WithClock(fn func() time.Time) SignalOption {
	return func(s *Signal) {
		if fn != nil {
			s.clock = fn
		}
	}
}

// NewSignal builds a signal monitor publishing to center. It copies the scalar
// initial settings; the caller may reuse s as long as it treats the QuietAlert
// map as immutable (SettingsFrom builds a fresh map each call). A nil center
// becomes a typed-nil *notify.Center (a no-op Publisher), matching newAppliance
// and newStreamEvents, so observe never calls a method on a bare nil interface.
// Register observe as a hub tap (RunSignal does this) to drive it.
func NewSignal(center notify.Publisher, s *Settings, opts ...SignalOption) *Signal {
	if center == nil {
		center = (*notify.Center)(nil)
	}
	sig := &Signal{pub: center, clock: time.Now, states: map[string]*deviceState{}}
	for _, o := range opts {
		o(sig)
	}
	cp := *s
	sig.set.Store(&cp)
	return sig
}

// Apply swaps the settings the monitor evaluates against. It only stores the
// pointer: the tap goroutine notices the change on its next window and resolves
// or re-arms conditions there, so a reconcile on the run loop never touches the
// monitor's per-device state concurrently with the tap.
func (s *Signal) Apply(set *Settings) {
	cp := *set
	s.set.Store(&cp)
}

// RunSignal builds the monitor, registers it as a hub tap, and cancels the tap
// when ctx is done. It returns the monitor so the caller can hand it to the
// appliance as its Monitors (reconcile then re-arms it via Apply).
func RunSignal(ctx context.Context, hub *levels.Hub, center notify.Publisher, s *Settings) *Signal {
	sig := NewSignal(center, s)
	cancel := hub.Tap(sig.observe)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return sig
}

// observe evaluates one window. It runs on the hub sampler goroutine.
func (s *Signal) observe(ev levels.LevelsEvent) {
	now := s.clock()
	set := s.set.Load()
	s.reconcile(set)

	for name := range s.states {
		s.states[name].seen = false
	}
	for i := range ev.Devices {
		d := &ev.Devices[i]
		st := s.stateFor(d.Name, set)
		st.seen = true
		if set.Enabled {
			s.evaluate(st, now, d, set)
		}
	}
	s.ageOutAbsent(set.Enabled)
}

// reconcile applies a settings change since the last window on the tap goroutine:
// a monitor disabled resolves everything it owns; a device's quiet opt-out turned
// off resolves only that device's quiet condition.
func (s *Signal) reconcile(set *Settings) {
	prev := s.applied
	s.applied = set
	// Nothing to reconcile on the first window, or when Apply has not swapped the
	// pointer since the last window (the steady state: same pointer every tick).
	if prev == nil || prev == set {
		return
	}
	if prev.Enabled && !set.Enabled {
		s.resolveAll("notifications disabled")
		return
	}
	if !set.Enabled {
		return
	}
	for name, st := range s.states {
		if quietArmed(prev, name) && !quietArmed(set, name) {
			// Resolve the active condition, and always reset the machine so a run
			// left pending (not yet active) at disarm cannot carry a stale start
			// time into a later re-arm and onset prematurely.
			if st.quiet.Active() {
				s.pub.Resolve(audioQuietKey(name), "very-quiet alert turned off")
			}
			st.quiet.Reset()
		}
	}
}

// resolveAll clears every active condition this monitor owns and resets state, so
// a re-enable starts fresh.
func (s *Signal) resolveAll(reason string) {
	for name, st := range s.states {
		if st.zero.Active() {
			s.pub.Resolve(audioZeroKey(name), reason)
		}
		if st.quiet.Active() {
			s.pub.Resolve(audioQuietKey(name), reason)
		}
		if st.clipActive {
			s.pub.Resolve(audioClipKey(name), reason)
		}
		st.zero.Reset()
		st.quiet.Reset()
		st.resetClip()
	}
}

// stateFor returns name's state, creating it with the current onset durations on
// first use.
func (s *Signal) stateFor(name string, set *Settings) *deviceState {
	st := s.states[name]
	if st == nil {
		st = &deviceState{
			zero:  notify.NewHysteresis(secs(set.Audio.ZeroSeconds), zeroClearAfter),
			quiet: notify.NewHysteresis(secs(set.Audio.QuietSeconds), quietClearAfter),
		}
		s.states[name] = st
	}
	return st
}

// evaluate drives one present device's three conditions for this window.
func (s *Signal) evaluate(st *deviceState, now time.Time, d *levels.DeviceLevels, set *Settings) {
	// Keep the onset durations current so a hot-reloaded threshold applies without
	// losing a running timer or an active condition.
	st.zero.SetEnterAfter(secs(set.Audio.ZeroSeconds))
	st.quiet.SetEnterAfter(secs(set.Audio.QuietSeconds))

	switch st.zero.Observe(now, allChannelsAtFloor(d)) {
	case notify.TransitionOnset:
		s.pub.Onset(zeroOnset(d.Name, intVal(set.Audio.ZeroSeconds)))
	case notify.TransitionClear:
		s.pub.Clear(audioZeroKey(d.Name), signalClear("Signal restored", d.Name+" is producing audio again"))
	case notify.TransitionNone:
	}

	if quietArmed(set, d.Name) {
		over := maxPeak(d) < float64(intVal(set.Audio.QuietDbfs))
		switch st.quiet.Observe(now, over) {
		case notify.TransitionOnset:
			s.pub.Onset(quietOnset(d.Name, intVal(set.Audio.QuietDbfs), intVal(set.Audio.QuietSeconds)))
		case notify.TransitionClear:
			s.pub.Clear(audioQuietKey(d.Name), signalClear("Input level recovered", d.Name+" is back above the quiet threshold"))
		case notify.TransitionNone:
		}
	}

	s.evaluateClip(st, now, anyClipped(d), d.Name, set)
}

// evaluateClip raises the clipping condition when the fraction of clipped windows
// in the sliding clip window reaches the configured percentage, and clears it
// after a constant quiet period with no clipping.
func (s *Signal) evaluateClip(st *deviceState, now time.Time, clipped bool, name string, set *Settings) {
	winDur := secs(set.Audio.ClipWindowSeconds)
	st.clip = append(st.clip, clipSample{at: now, clipped: clipped})
	cutoff := now.Add(-winDur)
	kept, clippedCount := 0, 0
	for _, cs := range st.clip {
		if cs.at.Before(cutoff) {
			continue
		}
		st.clip[kept] = cs
		kept++
		if cs.clipped {
			clippedCount++
		}
	}
	st.clip = st.clip[:kept]
	if clipped {
		st.lastClip = now
	}

	if st.clipActive {
		if !st.lastClip.IsZero() && now.Sub(st.lastClip) >= clipClearAfter {
			s.pub.Clear(audioClipKey(name), signalClear("Clipping stopped", name+" is no longer clipping"))
			// Flush the window so a re-onset waits for a fresh full window. Without
			// this, a clip window longer than the constant clear dwell still holds
			// the pre-clear clipped samples, which would immediately re-onset and
			// flap the warning until they age out.
			st.resetClip()
		}
		return
	}
	// Judge the ratio only once the retained window spans a full clip window, so a
	// first clipped sample cannot onset at a 100% ratio over one sample, and a
	// hot-reload that lengthens the window waits for it to refill before onset.
	// st.clip always holds at least the sample just appended above, so index 0 is
	// the oldest retained window.
	if now.Sub(st.clip[0].at) < winDur {
		return
	}
	if clippedCount*100 >= intVal(set.Audio.ClipPercent)*len(st.clip) {
		st.clipActive = true
		s.pub.Onset(clipOnset(name, intVal(set.Audio.ClipPercent)))
	}
}

// ageOutAbsent resolves and drops any device missing for devicePresenceGrace
// windows. While the monitor is disabled its conditions were already resolved, so
// the device is dropped without a further resolve.
func (s *Signal) ageOutAbsent(enabled bool) {
	for name, st := range s.states {
		if st.seen {
			st.missed = 0
			continue
		}
		st.missed++
		if st.missed < devicePresenceGrace {
			continue
		}
		if enabled {
			if st.zero.Active() {
				s.pub.Resolve(audioZeroKey(name), "device stopped")
			}
			if st.quiet.Active() {
				s.pub.Resolve(audioQuietKey(name), "device stopped")
			}
			if st.clipActive {
				s.pub.Resolve(audioClipKey(name), "device stopped")
			}
		}
		delete(s.states, name)
	}
}

// quietArmed reports whether name's very-quiet condition is armed: a configured
// device carries its effective flag in the map, and a device absent from the map
// defaults armed.
func quietArmed(set *Settings, name string) bool {
	if v, ok := set.QuietAlert[name]; ok {
		return v
	}
	return true
}

// allChannelsAtFloor reports whether every channel is at the reported floor,
// which means the input is at exact digital zero.
func allChannelsAtFloor(d *levels.DeviceLevels) bool {
	if len(d.Channels) == 0 {
		return false
	}
	for i := range d.Channels {
		if d.Channels[i].PeakDbfs != floorDbfs {
			return false
		}
	}
	return true
}

// maxPeak returns the loudest channel peak, or the floor when there are no
// channels.
func maxPeak(d *levels.DeviceLevels) float64 {
	m := floorDbfs
	for i := range d.Channels {
		if d.Channels[i].PeakDbfs > m {
			m = d.Channels[i].PeakDbfs
		}
	}
	return m
}

// anyClipped reports whether any channel clipped this window.
func anyClipped(d *levels.DeviceLevels) bool {
	for i := range d.Channels {
		if d.Channels[i].Clipped {
			return true
		}
	}
	return false
}

// intVal dereferences a presence-aware threshold. SettingsFrom is fed a defaulted
// config so every pointer is non-nil; the guard keeps a hand-built Settings in a
// test from panicking.
func intVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// secs turns a whole-second threshold into a Duration.
func secs(p *int) time.Duration { return time.Duration(intVal(p)) * time.Second }

func zeroOnset(name string, zeroSeconds int) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategoryAudio,
		Key:      audioZeroKey(name),
		Source:   name,
		Title:    "No signal",
		Message:  fmt.Sprintf("No signal: %s has been at digital zero for %s (check the cable or the mixer capture switch)", name, humanDuration(zeroSeconds)),
	}
}

func quietOnset(name string, quietDbfs, quietSeconds int) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategoryAudio,
		Key:      audioQuietKey(name),
		Source:   name,
		Title:    "Very quiet input",
		Message:  fmt.Sprintf("Very quiet input: %s peak has stayed below %d dBFS for %s", name, quietDbfs, humanDuration(quietSeconds)),
	}
}

func clipOnset(name string, clipPercent int) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategoryAudio,
		Key:      audioClipKey(name),
		Source:   name,
		Title:    "Input clipping",
		Message:  fmt.Sprintf("%s is clipping: at least %d%% of recent windows hit full scale", name, clipPercent),
	}
}

// signalClear builds the info body for a condition clear; Center.Clear fills in
// the key, category, and source from the matching onset.
func signalClear(title, message string) notify.Notification {
	return notify.Notification{Severity: notify.SeverityInfo, Title: title, Message: message}
}

// humanDuration renders a whole-second duration as best-effort prose for a
// message: "45 seconds", "10 minutes", "2 hours". It is not a parseable format.
func humanDuration(sec int) string {
	switch {
	case sec >= 3600 && sec%3600 == 0:
		return plural(sec/3600, "hour")
	case sec >= 60 && sec%60 == 0:
		return plural(sec/60, "minute")
	default:
		return plural(sec, "second")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
