//go:build linux

package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// TestDropLogDue pins the drop log cadence: a line at the first drop and then
// one per 50, whether the drops arrive one at a time or several per push.
func TestDropLogDue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		total, n uint64
		want     bool
	}{
		{"first drop", 1, 1, true},
		{"second drop", 2, 1, false},
		{"last of the first fifty", 50, 1, false},
		{"fifty-first drop", 51, 1, true},
		{"one push drops for three clients at the start", 3, 3, true},
		{"one push spans the step", 52, 3, true},
		{"one push ends on the step", 51, 3, true},
		{"one push stays inside a step", 30, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dropLogDue(tt.total, tt.n); got != tt.want {
				t.Errorf("dropLogDue(%d, %d) = %v, want %v", tt.total, tt.n, got, tt.want)
			}
		})
	}
}

// TestNoteDroppedCountsAndLogsOnce pins that a push which drops for several
// clients adds every drop to the stream's counter, and that the log fires at
// the first drop and then once per 50 more.
func TestNoteDroppedCountsAndLogsOnce(t *testing.T) {
	// Not parallel: it swaps the process-wide log output.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	sr := &streamRuntime{}
	sr.stream.Path = "/lossy"
	sr.noteDropped("dev", 0) // a push nobody dropped
	if got := sr.dropped.Load(); got != 0 || buf.Len() != 0 {
		t.Fatalf("no drops: counter %d, log %q, want 0 and empty", got, buf.String())
	}
	sr.noteDropped("dev", 2) // first drop, two clients
	sr.noteDropped("dev", 1)
	if got := sr.dropped.Load(); got != 3 {
		t.Errorf("counter = %d, want 3", got)
	}
	if got := strings.Count(buf.String(), "dropping frames"); got != 1 {
		t.Errorf("logged %d lines for the first three drops, want 1", got)
	}
	if !strings.Contains(buf.String(), "dev (/lossy)") || !strings.Contains(buf.String(), "total drops: 2") {
		t.Errorf("log %q does not name the device, stream and running total", buf.String())
	}
	for range 48 {
		sr.noteDropped("dev", 1) // up to 51 drops: crosses the next step once
	}
	if got := strings.Count(buf.String(), "dropping frames"); got != 2 {
		t.Errorf("logged %d lines by the 51st drop, want 2", got)
	}
}
