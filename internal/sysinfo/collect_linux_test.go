//go:build linux

package sysinfo

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestSamplerNilAndBeforeData(t *testing.T) {
	var nilSampler *Sampler
	if v, ok := nilSampler.Percent(); ok || v != 0 {
		t.Errorf("nil sampler Percent = %v, %v; want 0, false", v, ok)
	}
	fresh := &Sampler{}
	if v, ok := fresh.Percent(); ok || v != 0 {
		t.Errorf("fresh sampler Percent = %v, %v; want 0, false", v, ok)
	}
}

func TestSamplerProducesValueAndStopsOnCancel(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s := NewSampler(ctx, 2*time.Millisecond)

	// Poll until the loop has taken its second /proc/stat reading and published
	// a value (hasData). System load is irrelevant: an idle host yields 0%, ok.
	deadline := time.Now().Add(2 * time.Second)
	var (
		v  float64
		ok bool
	)
	for time.Now().Before(deadline) {
		if v, ok = s.Percent(); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("sampler never produced a value after two readings")
	}
	if v < 0 || v > 100 {
		t.Errorf("cpu percent = %v, out of [0,100]", v)
	}

	// Cancelling ctx stops the loop goroutine; a later read stays safe and keeps
	// the last published value.
	cancel()
	if _, ok := s.Percent(); !ok {
		t.Error("Percent after cancel lost its last value")
	}
	// The loop goroutine must actually exit: poll for the count to settle back
	// to the pre-sampler baseline (a small tolerance absorbs runtime churn).
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseGoroutines+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseGoroutines+1 {
		t.Errorf("sampler goroutine did not exit after cancel: %d goroutines, baseline %d", n, baseGoroutines)
	}
}

func TestCollectSmoke(t *testing.T) {
	si := Collect(t.TempDir(), nil)
	if si.Platform == "" || si.CPUCores < 1 {
		t.Errorf("implausible static facts: platform=%q cores=%d", si.Platform, si.CPUCores)
	}
	// dataPath is a real directory, so statfs must report a positive total.
	if si.DiskTotal <= 0 {
		t.Errorf("DiskTotal = %d, want > 0", si.DiskTotal)
	}
	if si.MemTotal <= 0 {
		t.Errorf("MemTotal = %d, want > 0", si.MemTotal)
	}
	// A nil sampler must leave CPUPercent absent, not panic.
	if si.CPUPercent != nil {
		t.Errorf("CPUPercent = %v with nil sampler, want nil", *si.CPUPercent)
	}
}
