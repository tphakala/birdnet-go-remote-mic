//go:build linux

package sysinfo

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
		// A host can register more than one rpi_volt hwmon; a bad first match must
		// not abort the probe. Glob returns the dirs sorted, so the lower-numbered
		// (bad) entry is tried before the higher-numbered (valid) one.
		{"first rpi_volt unparsable, second valid set", []hwmon{
			{"hwmon6", undervoltHwmonName, str("garbage")},
			{"hwmon7", undervoltHwmonName, str("1\n")},
		}, true, true},
		{"first rpi_volt alarm missing, second valid clear", []hwmon{
			{"hwmon8", undervoltHwmonName, nil},
			{"hwmon9", undervoltHwmonName, str("0\n")},
		}, false, true},
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
		root := t.TempDir()
		writeHwmon(t, root, "hwmon5", undervoltHwmonName, str("1\n"))
		alarmPath := filepath.Join(root, "hwmon5", "in0_lcrit_alarm")
		if err := os.Chmod(alarmPath, 0o000); err != nil {
			t.Fatal(err)
		}
		// Probe the actual permission rather than only euid 0: a non-root process
		// with CAP_DAC_OVERRIDE can also read a mode 0000 file and would not
		// exercise the unreadable path, so skip when the file is in fact readable.
		if _, err := os.ReadFile(alarmPath); err == nil { //nolint:gosec // deliberate readability probe of the test fixture
			t.Skip("process can read a mode 0000 file; cannot exercise the unreadable path")
		}
		if now, ok := readUndervoltage(root); now || ok {
			t.Errorf("readUndervoltage(unreadable) = (%v, %v), want (false, false)", now, ok)
		}
	})
}

// TestReadMemSeam exercises readMem against fixtures, in particular the
// reject-when-MemAvailable-absent branch that ReadMem's fixed /proc path cannot
// reach deterministically.
func TestReadMemSeam(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "meminfo")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Run("total and available present", func(t *testing.T) {
		t.Parallel()
		p := write(t, "MemTotal:       16384 kB\nMemFree:         1000 kB\nMemAvailable:    8192 kB\n")
		total, avail, ok := readMem(p)
		if !ok || total != 16384*1024 || avail != 8192*1024 {
			t.Errorf("readMem = (%d, %d, %v), want (%d, %d, true)", total, avail, ok, 16384*1024, 8192*1024)
		}
	})
	t.Run("MemAvailable absent rejects", func(t *testing.T) {
		t.Parallel()
		// A kernel before 3.14 reports no MemAvailable; readMem must reject rather
		// than mistake the missing figure for zero available memory.
		p := write(t, "MemTotal:       16384 kB\nMemFree:         1000 kB\n")
		if _, _, ok := readMem(p); ok {
			t.Error("readMem ok = true without MemAvailable, want false")
		}
	})
	t.Run("MemTotal absent rejects", func(t *testing.T) {
		t.Parallel()
		p := write(t, "MemAvailable:    8192 kB\n")
		if _, _, ok := readMem(p); ok {
			t.Error("readMem ok = true without MemTotal, want false")
		}
	})
	t.Run("missing file rejects", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := readMem(filepath.Join(t.TempDir(), "absent")); ok {
			t.Error("readMem ok = true for a missing file, want false")
		}
	})
}

func TestReadMemAndTempSmoke(t *testing.T) {
	total, avail, ok := ReadMem()
	if !ok {
		// ReadMem documents ok=false as expected when /proc/meminfo lacks
		// MemAvailable (kernels before 3.14), so the smoke test must not fail the
		// build on such a host; the seam test covers the parsing deterministically.
		t.Skip("ReadMem reports no MemAvailable (kernel < 3.14); nothing to assert here")
	}
	if total <= 0 || avail < 0 || avail > total {
		t.Errorf("ReadMem = (%d, %d), want 0 <= avail <= total, total > 0", total, avail)
	}
	if c, ok := ReadTemp(); ok && (c < -40 || c > 150) {
		t.Errorf("ReadTemp = %v, implausible", c)
	}
	// Drive the cached undervoltage sensor too: its reading is host-dependent
	// (unavailable off a Raspberry Pi), so only require that the call is safe.
	ReadUndervoltage()
	if _, _, ok := DiskUsage(""); ok {
		t.Error("DiskUsage(\"\") ok = true, want false")
	}
}

// TestCachedSensor drives the cache/probe/backoff state machine with an
// instrumented probe and readAt and a manual clock, so every transition is
// asserted without touching sysfs.
func TestCachedSensor(t *testing.T) {
	now := time.Unix(0, 0)
	var (
		probes, reads      int
		probeFound, readOK bool
		probePath          string
		probeVal, readVal  int
	)
	cs := &cachedSensor[int]{
		clock:   func() time.Time { return now },
		backoff: time.Minute,
		readAt:  func(string) (int, bool) { reads++; return readVal, readOK },
		probe:   func() (string, int, bool) { probes++; return probePath, probeVal, probeFound },
	}

	// 1) Nothing cached, probe finds nothing: not ok, and the backoff is armed.
	if v, ok := cs.read(); ok || v != 0 || probes != 1 || reads != 0 {
		t.Fatalf("miss = (%d,%v) probes=%d reads=%d, want (0,false) probes=1 reads=0", v, ok, probes, reads)
	}
	// 2) Within the backoff window: no re-probe.
	now = now.Add(30 * time.Second)
	if v, ok := cs.read(); ok || v != 0 || probes != 1 {
		t.Fatalf("within backoff re-probed: (%d,%v) probes=%d, want (0,false) probes=1", v, ok, probes)
	}
	// 3) Past the backoff, the probe finds the sensor: cache the path, no readAt.
	now = now.Add(31 * time.Second)
	probeFound, probePath, probeVal = true, "sensorA", 42
	if v, ok := cs.read(); !ok || v != 42 || probes != 2 || reads != 0 {
		t.Fatalf("post-backoff probe = (%d,%v) probes=%d reads=%d, want (42,true) probes=2 reads=0", v, ok, probes, reads)
	}
	// 4) Cache hit: readAt only, no probe.
	readOK, readVal = true, 43
	if v, ok := cs.read(); !ok || v != 43 || probes != 2 || reads != 1 {
		t.Fatalf("cache hit = (%d,%v) probes=%d reads=%d, want (43,true) probes=2 reads=1", v, ok, probes, reads)
	}
	// 5) The cached read fails (path went stale): invalidate and re-probe now, not
	//    throttled by the backoff.
	readOK, probeVal = false, 44
	if v, ok := cs.read(); !ok || v != 44 || probes != 3 || reads != 2 {
		t.Fatalf("stale re-probe = (%d,%v) probes=%d reads=%d, want (44,true) probes=3 reads=2", v, ok, probes, reads)
	}
}

// TestCachedSensorFallbackNotCached pins the uncached-fallback path: a probe that
// returns ok with an empty path yields the reading but is never cached, so a later
// preferred sensor is not masked and readAt is never consulted.
func TestCachedSensorFallbackNotCached(t *testing.T) {
	now := time.Unix(0, 0)
	probes := 0
	cs := &cachedSensor[int]{
		clock:   func() time.Time { return now },
		backoff: time.Minute,
		readAt:  func(string) (int, bool) { t.Fatal("readAt called for an uncached fallback reading"); return 0, false },
		probe:   func() (string, int, bool) { probes++; return "", 5, true },
	}
	for i := range 3 {
		now = now.Add(10 * time.Second)
		if v, ok := cs.read(); !ok || v != 5 {
			t.Fatalf("read %d = (%d,%v), want (5,true)", i, v, ok)
		}
	}
	if probes != 3 {
		t.Fatalf("fallback probed %d times over 3 reads, want 3 (never cached)", probes)
	}
}

// writeZone creates root/<zone>/temp with milli milli-Celsius and, when typ is
// non-empty, root/<zone>/type.
func writeZone(t *testing.T, root, zone, typ string, milli int) {
	t.Helper()
	d := filepath.Join(root, zone)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "temp"), []byte(strconv.Itoa(milli)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if typ != "" {
		if err := os.WriteFile(filepath.Join(d, "type"), []byte(typ+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeTemp(t *testing.T) {
	t.Run("prefers a cpu/soc zone and returns its path", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeZone(t, root, "thermal_zone0", "acpitz", 40000)
		writeZone(t, root, "thermal_zone1", "cpu-thermal", 55000)
		path, c, ok := probeTemp(root)
		if !ok || c != 55 || path != filepath.Join(root, "thermal_zone1", "temp") {
			t.Fatalf("probeTemp = (%q, %v, %v), want the cpu zone at 55 C", path, c, ok)
		}
	})
	t.Run("returns a fallback reading uncached (empty path)", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeZone(t, root, "thermal_zone0", "battery", 25000)
		path, c, ok := probeTemp(root)
		if !ok || c != 25 || path != "" {
			t.Fatalf("probeTemp fallback = (%q, %v, %v), want (\"\", 25, true)", path, c, ok)
		}
	})
	t.Run("no readable zone", func(t *testing.T) {
		t.Parallel()
		if path, _, ok := probeTemp(filepath.Join(t.TempDir(), "absent")); ok || path != "" {
			t.Fatalf("probeTemp(absent) = (%q, _, %v), want not ok", path, ok)
		}
	})
	t.Run("skips unreadable and unparseable zones", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		// zone0: temp is a directory, so ReadFile fails (unreadable entry).
		if err := os.MkdirAll(filepath.Join(root, "thermal_zone0", "temp"), 0o755); err != nil {
			t.Fatal(err)
		}
		// zone1: a non-numeric temp fails to parse.
		writeZone(t, root, "thermal_zone1", "acpitz", 0)
		if err := os.WriteFile(filepath.Join(root, "thermal_zone1", "temp"), []byte("garbage\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// zone2: the one good CPU zone, the expected winner.
		writeZone(t, root, "thermal_zone2", "cpu-thermal", 50000)
		path, c, ok := probeTemp(root)
		if !ok || c != 50 || path != filepath.Join(root, "thermal_zone2", "temp") {
			t.Fatalf("probeTemp = (%q, %v, %v), want the cpu zone at 50 C after skipping the bad zones", path, c, ok)
		}
	})
}

func TestReadTempAt(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, "thermal_zone0", "cpu-thermal", 48000)
	tempPath := filepath.Join(root, "thermal_zone0", "temp")

	t.Run("valid cpu zone", func(t *testing.T) {
		if c, ok := readTempAt(tempPath); !ok || c != 48 {
			t.Fatalf("readTempAt = (%v, %v), want (48, true)", c, ok)
		}
	})
	t.Run("rejects a zone whose type is no longer cpu/soc", func(t *testing.T) {
		// A driver reload renumbered zone0 onto a different device.
		if err := os.WriteFile(filepath.Join(root, "thermal_zone0", "type"), []byte("battery\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := readTempAt(tempPath); ok {
			t.Error("readTempAt trusted a zone whose type is no longer cpu/soc")
		}
	})
	t.Run("rejects a missing type", func(t *testing.T) {
		if _, ok := readTempAt(filepath.Join(t.TempDir(), "thermal_zone0", "temp")); ok {
			t.Error("readTempAt ok for a missing zone")
		}
	})
	t.Run("rejects an unreadable temp", func(t *testing.T) {
		d := filepath.Join(t.TempDir(), "thermal_zone0")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "type"), []byte("cpu\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := readTempAt(filepath.Join(d, "temp")); ok {
			t.Error("readTempAt ok with a missing temp file")
		}
	})
}

func TestReadAlarmAt(t *testing.T) {
	str := func(s string) *string { return &s }
	root := t.TempDir()
	writeHwmon(t, root, "hwmon0", undervoltHwmonName, str("1\n"))
	alarmPath := filepath.Join(root, "hwmon0", "in0_lcrit_alarm")

	t.Run("valid rpi_volt alarm", func(t *testing.T) {
		if now, ok := readAlarmAt(alarmPath); !ok || !now {
			t.Fatalf("readAlarmAt = (%v, %v), want (true, true)", now, ok)
		}
	})
	t.Run("rejects a device whose name is no longer rpi_volt", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "hwmon0", "name"), []byte("coretemp\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := readAlarmAt(alarmPath); ok {
			t.Error("readAlarmAt trusted a hwmon whose name is no longer rpi_volt")
		}
	})
	t.Run("rejects a missing device", func(t *testing.T) {
		if _, ok := readAlarmAt(filepath.Join(t.TempDir(), "hwmon0", "in0_lcrit_alarm")); ok {
			t.Error("readAlarmAt ok for a missing device")
		}
	})
	t.Run("rejects an unreadable alarm", func(t *testing.T) {
		d := filepath.Join(t.TempDir(), "hwmon0")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "name"), []byte(undervoltHwmonName+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := readAlarmAt(filepath.Join(d, "in0_lcrit_alarm")); ok {
			t.Error("readAlarmAt ok with a missing alarm file")
		}
	})
}
