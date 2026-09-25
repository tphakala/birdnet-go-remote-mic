//go:build linux

package main

import (
	"sync/atomic"
	"testing"
)

// TestNoteEncodedWakesOnce pins the hot-path contract of noteEncoded: the first
// frame sets the flag and wakes a waiting retry, and later frames do neither
// (one atomic load each); with no retry waiting, nothing wakes.
func TestNoteEncodedWakesOnce(t *testing.T) {
	t.Parallel()
	var sr streamRuntime
	var await atomic.Bool
	wakes := 0
	wake := func() { wakes++ }

	sr.noteEncoded(&await, wake)
	if !sr.encoded.Load() || wakes != 0 {
		t.Fatalf("first frame with no waiter: encoded=%v wakes=%d, want true and 0", sr.encoded.Load(), wakes)
	}

	var waited streamRuntime
	await.Store(true)
	for range 3 {
		waited.noteEncoded(&await, wake)
	}
	if wakes != 1 {
		t.Errorf("got %d wakes over three frames with a waiter, want 1", wakes)
	}
}
