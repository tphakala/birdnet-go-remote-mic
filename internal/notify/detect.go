package notify

import "time"

// Transition is what a detector reports for one observation: nothing changed, a
// condition just onset, or a condition just cleared.
type Transition int

const (
	// TransitionNone means the condition's state did not change.
	TransitionNone Transition = iota
	// TransitionOnset means the condition just became active.
	TransitionOnset
	// TransitionClear means the condition just became inactive.
	TransitionClear
)

// Hysteresis is a two-threshold, two-duration condition state machine: an input
// must stay "over" for enterAfter before the condition onsets, and stay "under"
// for clearAfter before it clears. A brief blip in either direction resets the
// pending timer, so noise near the threshold neither raises nor clears an alert.
// Detectors are pure and fake-clock testable; a monitor drives one from a single
// goroutine, so Hysteresis is not safe for concurrent use.
type Hysteresis struct {
	enterAfter time.Duration
	clearAfter time.Duration

	active bool
	// since marks when the current candidate run began: a sustained "over" while
	// inactive, or a sustained "under" while active. running says since is set.
	since   time.Time
	running bool
}

// NewHysteresis returns a Hysteresis that onsets after over holds for enterAfter
// and clears after under holds for clearAfter. Zero durations onset or clear on
// the first qualifying observation.
func NewHysteresis(enterAfter, clearAfter time.Duration) *Hysteresis {
	return &Hysteresis{enterAfter: enterAfter, clearAfter: clearAfter}
}

// Observe records one reading at now (over reports whether the input is past the
// alerting threshold) and reports the resulting transition.
func (h *Hysteresis) Observe(now time.Time, over bool) Transition {
	if h.active {
		// Active: the candidate run that can clear the condition is a sustained
		// "under". Any "over" resets the clear timer.
		if over {
			h.running = false
			return TransitionNone
		}
		if !h.running {
			h.since, h.running = now, true
		}
		if now.Sub(h.since) >= h.clearAfter {
			h.active, h.running = false, false
			return TransitionClear
		}
		return TransitionNone
	}
	// Inactive: the candidate run that can onset the condition is a sustained
	// "over". Any "under" resets the onset timer.
	if !over {
		h.running = false
		return TransitionNone
	}
	if !h.running {
		h.since, h.running = now, true
	}
	if now.Sub(h.since) >= h.enterAfter {
		h.active, h.running = true, false
		return TransitionOnset
	}
	return TransitionNone
}

// Reset returns the machine to its initial inactive state, discarding any
// pending run. A monitor calls it when the subject it watches disappears.
func (h *Hysteresis) Reset() {
	h.active = false
	h.running = false
	h.since = time.Time{}
}

// Flap detects a rapid burst of repeated events: while inactive, more than max
// events within a sliding window of length window raises an onset; while active,
// the first event after a gap of at least quiet clears it and begins a fresh
// window. Suppressing the individual events during a flap is the caller's job
// (it stops emitting per-event notifications between the onset and the clear).
// Not safe for concurrent use.
type Flap struct {
	maxEvents int
	window    time.Duration
	quiet     time.Duration

	events []time.Time // event times still inside the current window
	last   time.Time   // most recent event time; zero before the first event
	active bool
}

// NewFlap returns a Flap that onsets when more than maxEvents events land within
// window and clears on the first event seen after quiet elapses with none.
func NewFlap(maxEvents int, window, quiet time.Duration) *Flap {
	return &Flap{maxEvents: maxEvents, window: window, quiet: quiet}
}

// Event records an event at now and reports the resulting transition. A clear
// and an onset never occur on the same call: a clearing event resets the window
// to itself, so it cannot also exceed max (unless max is below one, which is not
// a valid flap threshold).
func (f *Flap) Event(now time.Time) Transition {
	if f.active && !f.last.IsZero() && now.Sub(f.last) >= f.quiet {
		// A quiet gap since the previous event: the burst has ended. Clear, and
		// treat this event as the first of a possible new window.
		f.active = false
		f.events = append(f.events[:0], now)
		f.last = now
		return TransitionClear
	}
	f.last = now
	// While the condition is active the sliding window is irrelevant until it
	// clears, so stop maintaining it: appending every event of a dense burst
	// would grow the slice and re-scan it for nothing. f.last still advanced
	// above, so the quiet-gap clear at the top keeps working.
	if f.active {
		return TransitionNone
	}
	// Drop events that have aged out of the sliding window, in place, then add
	// this one. Filtering into a prefix of f.events keeps the append on the same
	// slice, so no aliasing surprise.
	cutoff := now.Add(-f.window)
	kept := 0
	for _, t := range f.events {
		if !t.Before(cutoff) {
			f.events[kept] = t
			kept++
		}
	}
	f.events = append(f.events[:kept], now)
	if len(f.events) > f.maxEvents {
		f.active = true
		return TransitionOnset
	}
	return TransitionNone
}

// Reset returns the detector to its initial inactive state, discarding the
// window. A monitor calls it when the subject it watches disappears.
func (f *Flap) Reset() {
	f.active = false
	f.events = f.events[:0]
	f.last = time.Time{}
}
