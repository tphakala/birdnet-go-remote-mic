//go:build linux

package sysinfo

import "testing"

// TestHostCPUReadPrimesThenDiffs pins the prime/diff state machine of the host
// monitor's own CPU reader: it must diff /proc/stat over the caller's poll window,
// never report a value before it is primed, and never diff against a zeroed prev
// when the priming read failed (which would report utilization since boot).
func TestHostCPUReadPrimesThenDiffs(t *testing.T) {
	type step struct {
		idle, total uint64
		ok          bool
	}
	// newReader returns a /proc/stat stub yielding steps in order, repeating the
	// last once exhausted so a test can over-poll safely.
	newReader := func(steps ...step) func() (uint64, uint64, bool) {
		i := 0
		return func() (uint64, uint64, bool) {
			s := steps[i]
			if i < len(steps)-1 {
				i++
			}
			return s.idle, s.total, s.ok
		}
	}

	t.Run("prime at construction then diff over the window", func(t *testing.T) {
		t.Parallel()
		c := newHostCPU(newReader(
			step{100, 200, true}, // constructor primes prev=(100,200)
			step{150, 400, true}, // first Read: dTotal=200, dIdle=50 -> 75% busy
		))
		got, ok := c.Read()
		if !ok || got != 75 {
			t.Fatalf("Read = (%v, %v), want (75, true)", got, ok)
		}
	})

	t.Run("a failed construction read primes on the first Read, not against zero", func(t *testing.T) {
		t.Parallel()
		c := newHostCPU(newReader(
			step{0, 0, false},    // constructor read fails: prev must NOT be seeded at zero
			step{100, 200, true}, // first Read primes here and reports ok=false
			step{150, 400, true}, // second Read diffs over the window -> 75%
		))
		if _, ok := c.Read(); ok {
			t.Fatal("first Read after a failed prime reported ok=true; it must prime and report false")
		}
		got, ok := c.Read()
		if !ok || got != 75 {
			t.Fatalf("second Read = (%v, %v), want (75, true) over the poll window, not since boot", got, ok)
		}
	})

	t.Run("a failed poll is unavailable and leaves prev intact", func(t *testing.T) {
		t.Parallel()
		c := newHostCPU(newReader(
			step{100, 200, true}, // prime
			step{0, 0, false},    // a failed poll: ok=false, prev untouched
			step{150, 400, true}, // next good poll diffs against the pre-gap prime -> 75%
		))
		if _, ok := c.Read(); ok {
			t.Fatal("a failed /proc/stat read reported ok=true")
		}
		got, ok := c.Read()
		if !ok || got != 75 {
			t.Fatalf("Read after a failed poll = (%v, %v), want (75, true)", got, ok)
		}
	})

	t.Run("nil receiver is unavailable, not a panic", func(t *testing.T) {
		t.Parallel()
		var c *HostCPU
		if _, ok := c.Read(); ok {
			t.Fatal("nil HostCPU reported ok=true")
		}
	})
}
