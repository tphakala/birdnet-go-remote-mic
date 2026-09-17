package monitor

import "testing"

// TestOverWithGap pins the hysteresis-gap comparison shared by the CPU, temp, and
// disk conditions: while inactive a reading is "over" at or above the onset
// threshold (>=), while active it stays over until it drops to or below the
// lower clear threshold (>). The asymmetry is what gives the value its gap on top
// of the duration dwell, so a reading sitting between clear and onset holds an
// active condition but never raises a fresh one.
func TestOverWithGap(t *testing.T) {
	const onsetLevel, clearLevel = 90.0, 80.0
	cases := []struct {
		name   string
		active bool
		value  float64
		want   bool
	}{
		{"inactive below onset", false, 89.999, false},
		{"inactive at onset", false, onsetLevel, true},
		{"inactive above onset", false, 95, true},
		{"inactive in gap", false, 85, false},
		{"active above clear", true, 95, true},
		{"active in gap holds", true, 85, true},
		{"active just above clear", true, 80.001, true},
		{"active at clear", true, clearLevel, false},
		{"active below clear", true, 70, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := overWithGap(tc.active, tc.value, onsetLevel, clearLevel); got != tc.want {
				t.Errorf("overWithGap(active=%v, value=%v, onset=%v, clear=%v) = %v, want %v",
					tc.active, tc.value, onsetLevel, clearLevel, got, tc.want)
			}
		})
	}
}
