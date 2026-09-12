package notify

import (
	"testing"
	"time"
)

func transitionName(tr Transition) string {
	switch tr {
	case TransitionOnset:
		return "onset"
	case TransitionClear:
		return "clear"
	default:
		return "none"
	}
}

func TestHysteresis(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	// One step feeds Observe at t0+at with the given input and asserts the result.
	type step struct {
		at   time.Duration
		over bool
		want Transition
	}
	tests := []struct {
		name       string
		enterAfter time.Duration
		clearAfter time.Duration
		steps      []step
	}{
		{
			name:       "sustained over onsets after enterAfter",
			enterAfter: 30 * time.Second,
			clearAfter: 30 * time.Second,
			steps: []step{
				{0, true, TransitionNone},
				{10 * time.Second, true, TransitionNone},
				{29 * time.Second, true, TransitionNone},
				{30 * time.Second, true, TransitionOnset},
				{40 * time.Second, true, TransitionNone}, // already active
			},
		},
		{
			name:       "blip under resets the onset timer",
			enterAfter: 30 * time.Second,
			clearAfter: 30 * time.Second,
			steps: []step{
				{0, true, TransitionNone},
				{20 * time.Second, false, TransitionNone}, // resets
				{25 * time.Second, true, TransitionNone},  // new run starts here
				{50 * time.Second, true, TransitionNone},  // 25s into the new run
				{55 * time.Second, true, TransitionOnset}, // 30s into the new run
			},
		},
		{
			name:       "sustained under clears, blip over resets clear timer",
			enterAfter: 0,
			clearAfter: 30 * time.Second,
			steps: []step{
				{0, true, TransitionOnset}, // zero enterAfter onsets immediately
				{10 * time.Second, false, TransitionNone},
				{20 * time.Second, true, TransitionNone},  // resets the clear timer
				{25 * time.Second, false, TransitionNone}, // new under run starts
				{54 * time.Second, false, TransitionNone}, // 29s
				{55 * time.Second, false, TransitionClear},
			},
		},
		{
			name:       "zero clearAfter clears on first under",
			enterAfter: 0,
			clearAfter: 0,
			steps: []step{
				{0, true, TransitionOnset},
				{5 * time.Second, false, TransitionClear},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHysteresis(tc.enterAfter, tc.clearAfter)
			for i, s := range tc.steps {
				if got := h.Observe(t0.Add(s.at), s.over); got != s.want {
					t.Errorf("step %d (at %s over=%v): got %s, want %s",
						i, s.at, s.over, transitionName(got), transitionName(s.want))
				}
			}
		})
	}
}

func TestHysteresisReset(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	h := NewHysteresis(0, 0)
	if got := h.Observe(t0, true); got != TransitionOnset {
		t.Fatalf("onset: got %s", transitionName(got))
	}
	h.Reset()
	// After Reset the machine is inactive again, so an under is a no-op (an active
	// machine would have cleared).
	if got := h.Observe(t0.Add(time.Second), false); got != TransitionNone {
		t.Fatalf("after reset, under = %s, want none", transitionName(got))
	}
	// And an over onsets afresh.
	if got := h.Observe(t0.Add(2*time.Second), true); got != TransitionOnset {
		t.Fatalf("after reset, over = %s, want onset", transitionName(got))
	}
}

func TestFlap(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	type step struct {
		at   time.Duration
		want Transition
	}
	tests := []struct {
		name   string
		max    int
		window time.Duration
		quiet  time.Duration
		steps  []step
	}{
		{
			name:   "more than max within window onsets on the crossing event",
			max:    3,
			window: 60 * time.Second,
			quiet:  5 * time.Minute,
			steps: []step{
				{0, TransitionNone},
				{5 * time.Second, TransitionNone},
				{10 * time.Second, TransitionNone},
				{15 * time.Second, TransitionOnset}, // 4th within 60s
				{20 * time.Second, TransitionNone},  // stays active, suppressed
			},
		},
		{
			name:   "quiet gap clears then a fresh burst can onset again",
			max:    3,
			window: 60 * time.Second,
			quiet:  5 * time.Minute,
			steps: []step{
				{0, TransitionNone},
				{5 * time.Second, TransitionNone},
				{10 * time.Second, TransitionNone},
				{15 * time.Second, TransitionOnset},
				{15*time.Second + 5*time.Minute, TransitionClear}, // first event after quiet
				{15*time.Second + 5*time.Minute + 1*time.Second, TransitionNone},
				{15*time.Second + 5*time.Minute + 2*time.Second, TransitionNone},
				{15*time.Second + 5*time.Minute + 3*time.Second, TransitionOnset}, // 4th of new burst
			},
		},
		{
			name:   "events spread beyond the window never onset",
			max:    3,
			window: 60 * time.Second,
			quiet:  5 * time.Minute,
			steps: []step{
				{0, TransitionNone},
				{30 * time.Second, TransitionNone},
				{61 * time.Second, TransitionNone},  // first event aged out
				{91 * time.Second, TransitionNone},  // only two in window
				{121 * time.Second, TransitionNone}, // still within max
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFlap(tc.max, tc.window, tc.quiet)
			for i, s := range tc.steps {
				if got := f.Event(t0.Add(s.at)); got != s.want {
					t.Errorf("step %d (at %s): got %s, want %s",
						i, s.at, transitionName(got), transitionName(s.want))
				}
			}
		})
	}
}

func TestFlapDenseBurstStaysBoundedAndClears(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	f := NewFlap(3, 60*time.Second, 5*time.Minute)
	// Onset on the 4th event within the window.
	for i := 0; i < 3; i++ {
		if got := f.Event(t0.Add(time.Duration(i) * time.Second)); got != TransitionNone {
			t.Fatalf("event %d: got %s, want none", i, transitionName(got))
		}
	}
	if got := f.Event(t0.Add(3 * time.Second)); got != TransitionOnset {
		t.Fatalf("4th event: got %s, want onset", transitionName(got))
	}
	windowAtOnset := len(f.events)
	// A dense burst while active: every event is suppressed and the sliding
	// window does not grow (regression guard for the active-flap window leak).
	last := t0.Add(3 * time.Second)
	for i := 0; i < 100; i++ {
		last = t0.Add(4*time.Second + time.Duration(i)*time.Millisecond)
		if got := f.Event(last); got != TransitionNone {
			t.Fatalf("burst event %d: got %s, want none", i, transitionName(got))
		}
	}
	if len(f.events) > windowAtOnset {
		t.Errorf("window grew to %d during an active flap (was %d at onset); it must stay frozen",
			len(f.events), windowAtOnset)
	}
	// It clears exactly quiet after the last event, not quiet after the onset.
	if got := f.Event(last.Add(5 * time.Minute)); got != TransitionClear {
		t.Fatalf("after quiet since last event: got %s, want clear", transitionName(got))
	}
}

func TestFlapReset(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	f := NewFlap(1, time.Minute, time.Minute)
	f.Event(t0)
	if got := f.Event(t0.Add(time.Second)); got != TransitionOnset {
		t.Fatalf("expected onset on 2nd event, got %s", transitionName(got))
	}
	f.Reset()
	// After Reset the window is empty, so two fresh events are needed to onset
	// again; a single event does not.
	if got := f.Event(t0.Add(2 * time.Second)); got != TransitionNone {
		t.Fatalf("after reset, first event = %s, want none", transitionName(got))
	}
}
