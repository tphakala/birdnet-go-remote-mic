//go:build linux

package sysinfo

import (
	"context"
	"os"
	"path/filepath"
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

// writeHwmon creates root/<dir>/name with name and, when alarm is non-nil,
// in0_lcrit_alarm with that content.
func writeHwmon(t *testing.T, root, dir, name string, alarm *string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "name"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if alarm != nil {
		if err := os.WriteFile(filepath.Join(d, "in0_lcrit_alarm"), []byte(*alarm), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadUndervoltage(t *testing.T) {
	str := func(s string) *string { return &s }
	// hwmon is one fake /sys/class/hwmon entry; a nil alarm writes no
	// in0_lcrit_alarm file.
	type hwmon struct {
		dir, name string
		alarm     *string
	}
	tests := []struct {
		name    string
		entries []hwmon
		wantNow bool
		wantOK  bool
	}{
		{"rpi_volt among several, clear", []hwmon{
			{"hwmon0", "cpu_thermal", str("1\n")},
			{"hwmon1", "nvme", nil},
			{"hwmon4", undervoltHwmonName, str("0\n")},
		}, false, true},
		{"rpi_volt alarm set", []hwmon{{"hwmon2", undervoltHwmonName, str("1\n")}}, true, true},
		{"no rpi_volt device", []hwmon{{"hwmon0", "coretemp", str("0\n")}}, false, false},
		{"empty hwmon root", nil, false, false},
		{"alarm file missing", []hwmon{{"hwmon3", undervoltHwmonName, nil}}, false, false},
		{"alarm file unparsable", []hwmon{{"hwmon3", undervoltHwmonName, str("garbage")}}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for _, e := range tc.entries {
				writeHwmon(t, root, e.dir, e.name, e.alarm)
			}
			now, ok := readUndervoltage(root)
			if now != tc.wantNow || ok != tc.wantOK {
				t.Errorf("readUndervoltage = (%v, %v), want (%v, %v)", now, ok, tc.wantNow, tc.wantOK)
			}
		})
	}
	t.Run("missing root", func(t *testing.T) {
		t.Parallel()
		if now, ok := readUndervoltage(filepath.Join(t.TempDir(), "absent")); now || ok {
			t.Errorf("readUndervoltage(absent) = (%v, %v), want (false, false)", now, ok)
		}
	})
	t.Run("unreadable alarm file", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode 0000 file")
		}
		root := t.TempDir()
		writeHwmon(t, root, "hwmon5", undervoltHwmonName, str("1\n"))
		if err := os.Chmod(filepath.Join(root, "hwmon5", "in0_lcrit_alarm"), 0o000); err != nil {
			t.Fatal(err)
		}
		if now, ok := readUndervoltage(root); now || ok {
			t.Errorf("readUndervoltage(unreadable) = (%v, %v), want (false, false)", now, ok)
		}
	})
}

func TestReadMemAndTempSmoke(t *testing.T) {
	total, avail, ok := ReadMem()
	if !ok {
		t.Fatal("ReadMem ok = false on a Linux host")
	}
	if total <= 0 || avail < 0 || avail > total {
		t.Errorf("ReadMem = (%d, %d), want 0 <= avail <= total, total > 0", total, avail)
	}
	if c, ok := ReadTemp(); ok && (c < -40 || c > 150) {
		t.Errorf("ReadTemp = %v, implausible", c)
	}
	if _, _, ok := DiskUsage(""); ok {
		t.Error("DiskUsage(\"\") ok = true, want false")
	}
}
