package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// fakeHost is a HostReader whose readings a test sets directly. An unset
// reading reports ok=false. calls counts reads so a test can assert a disabled
// monitor reads nothing.
type fakeHost struct {
	mu                 sync.Mutex
	memTotal, memAvail int64
	memOK              bool
	temp               float64
	tempOK             bool
	diskTotal, diskUse int64
	diskAvail          int64
	diskOK             bool
	cpu                float64
	cpuOK              bool
	volt, voltOK       bool
	calls              int
}

func (f *fakeHost) Mem() (total, avail int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.memTotal, f.memAvail, f.memOK
}

func (f *fakeHost) Temp() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.temp, f.tempOK
}

func (f *fakeHost) Disk() (total, used, avail int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.diskTotal, f.diskUse, f.diskAvail, f.diskOK
}

func (f *fakeHost) CPU() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.cpu, f.cpuOK
}

func (f *fakeHost) Undervoltage() (now, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.volt, f.voltOK
}

func (f *fakeHost) setCPU(v float64) { f.mu.Lock(); f.cpu, f.cpuOK = v, true; f.mu.Unlock() }

func (f *fakeHost) readCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// hostSettings returns enabled Settings with the default host thresholds.
func hostSettings() Settings {
	return Settings{
		Enabled: true,
		Host: config.HostAlerts{
			CPUPercent: p(90), CPUClearPercent: p(75),
			TempCelsius: p(80), TempClearCelsius: p(75),
			DiskPercent: p(90), DiskClearPercent: p(85),
			MemFreePercent: p(10), MemFreeMiB: p(64),
		},
	}
}

//nolint:gocritic // test helper: taking Settings by value keeps call sites terse.
func newHostT(r HostReader, counters CounterSource, rec *recPub, s Settings, c *clk) *Host {
	return NewHost(r, counters, rec, &s, WithHostClock(c.now))
}

// pollEvery polls h n times, advancing the clock by step before each poll.
func pollEvery(h *Host, c *clk, step time.Duration, n int) {
	for range n {
		c.advance(step)
		h.poll()
	}
}

func TestHostCPUOnsetHysteresisGapAndClear(t *testing.T) {
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)

	r.setCPU(95)
	h.poll() // run starts
	pollEvery(h, c, 10*time.Second, 5)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu onset before 60 s")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu not active after 60 s at 95%")
	}
	if n, ok := rec.lastOnset(hostCPUKey); !ok || n.Severity != notify.SeverityWarning || n.Category != notify.CategorySystem {
		t.Errorf("cpu onset = %+v, want warning/system", n)
	}

	// Between clear (75) and onset (90): the condition holds indefinitely.
	r.setCPU(80)
	pollEvery(h, c, 10*time.Second, 20)
	if !rec.isActive(hostCPUKey) || rec.clearCount(hostCPUKey) != 0 {
		t.Fatal("cpu cleared inside the hysteresis gap")
	}

	// Under the clear threshold for 60 s clears it.
	r.setCPU(70)
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu cleared before the 60 s clear dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostCPUKey) || rec.clearCount(hostCPUKey) != 1 {
		t.Fatal("cpu not cleared after 60 s under 75%")
	}
	// The clear message reads "at or below" the clear threshold, not "under": the
	// active comparison is strictly greater-than, so equality clears too.
	if msg := rec.clearMessage(hostCPUKey); !strings.Contains(msg, "back at or below 75%") {
		t.Errorf("cpu clear message = %q, want it to name being at or below 75%%", msg)
	}

	// Inside the gap while inactive never onsets.
	r.setCPU(85)
	pollEvery(h, c, 10*time.Second, 30)
	if rec.onsetCount(hostCPUKey) != 1 {
		t.Errorf("cpu onsets = %d, want 1 (85%% is below the onset threshold)", rec.onsetCount(hostCPUKey))
	}
}

// TestHostCPUBoundaryEquality pins the onset (>=) and clear (>) comparisons at
// their exact thresholds: 90% onsets, 75% clears.
func TestHostCPUBoundaryEquality(t *testing.T) {
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)

	// Exactly at the onset threshold (90) onsets: the inactive comparison is >=.
	r.setCPU(90)
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu onset before the dwell at exactly 90%")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu did not onset at exactly 90% (>= boundary)")
	}

	// Exactly at the clear threshold (75) clears: the active comparison is >, so
	// equality is not "over" and the clear run runs.
	r.setCPU(75)
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu cleared before the dwell at exactly 75%")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu did not clear at exactly 75% (> boundary means equality clears)")
	}
}

// TestHostOnsetBoundaryEquality pins the onset comparisons at their exact
// thresholds. Temp and disk onset on >= (equality raises); memory onsets on a
// strict avail < limit (equality does NOT raise), the opposite polarity, so both
// sides of that boundary are checked.
func TestHostOnsetBoundaryEquality(t *testing.T) {
	const gib = int64(1) << 30

	// raiseDwell sets a boundary reading and asserts it onsets exactly at its dwell.
	raiseDwell := func(t *testing.T, key string, enterAfter time.Duration, set func(*fakeHost)) {
		t.Helper()
		r := &fakeHost{}
		rec := newRecPub()
		c := newClk()
		h := newHostT(r, nil, rec, hostSettings(), c)
		set(r)
		h.poll()
		c.advance(enterAfter - time.Second)
		h.poll()
		if rec.isActive(key) {
			t.Fatal("onset before the dwell at the exact boundary")
		}
		c.advance(time.Second)
		h.poll()
		if !rec.isActive(key) {
			t.Fatal("did not onset at the exact boundary after the dwell")
		}
	}

	t.Run("temp at exactly the threshold onsets", func(t *testing.T) {
		t.Parallel()
		raiseDwell(t, hostTempKey, tempEnterAfter, func(f *fakeHost) { f.temp, f.tempOK = 80, true })
	})
	t.Run("disk at exactly the threshold onsets", func(t *testing.T) {
		t.Parallel()
		raiseDwell(t, hostDiskKey, diskEnterAfter, func(f *fakeHost) {
			// used/(used+avail) = 90/(90+10) = exactly 90%.
			f.diskTotal, f.diskUse, f.diskAvail, f.diskOK = 100*gib, 90*gib, 10*gib, true
		})
	})
	t.Run("memory one byte below the limit onsets", func(t *testing.T) {
		t.Parallel()
		// 512 MiB host: the 64 MiB floor beats 10% (51 MiB), so the limit is 64 MiB.
		raiseDwell(t, hostMemKey, memEnterAfter, func(f *fakeHost) {
			f.memTotal, f.memAvail, f.memOK = 512<<20, (64<<20)-1, true
		})
	})
	t.Run("memory at exactly the limit does not onset", func(t *testing.T) {
		t.Parallel()
		r := &fakeHost{}
		rec := newRecPub()
		c := newClk()
		h := newHostT(r, nil, rec, hostSettings(), c)
		r.mu.Lock()
		r.memTotal, r.memAvail, r.memOK = 512<<20, 64<<20, true // avail == limit; onset is strict <
		r.mu.Unlock()
		h.poll()
		pollEvery(h, c, 10*time.Second, 12) // well past the 60 s dwell
		if rec.isActive(hostMemKey) || rec.onsetCount(hostMemKey) != 0 {
			t.Fatalf("low-memory onset at avail == limit (onsets=%d), want none", rec.onsetCount(hostMemKey))
		}
	})
}

func TestHostTempDiskVoltMem(t *testing.T) {
	const gib = int64(1) << 30
	tests := []struct {
		name       string
		key        string
		sev        notify.Severity
		enterAfter time.Duration
		raise      func(*fakeHost)
		gap        func(*fakeHost) // inside the hysteresis gap: holds an active condition
		recover    func(*fakeHost)
	}{
		{
			"temp", hostTempKey, notify.SeverityWarning, tempEnterAfter,
			func(f *fakeHost) { f.temp, f.tempOK = 82, true },
			func(f *fakeHost) { f.temp = 77 },
			func(f *fakeHost) { f.temp = 70 },
		},
		{
			"disk", hostDiskKey, notify.SeverityWarning, diskEnterAfter,
			// used/(used+avail): 95% raise, 88% gap, 50% recover (no reserved slack here).
			func(f *fakeHost) { f.diskTotal, f.diskUse, f.diskAvail, f.diskOK = 100*gib, 95*gib, 5*gib, true },
			func(f *fakeHost) { f.diskUse, f.diskAvail = 88*gib, 12*gib },
			func(f *fakeHost) { f.diskUse, f.diskAvail = 50*gib, 50*gib },
		},
		{
			"volt", hostVoltKey, notify.SeverityError, voltEnterAfter,
			func(f *fakeHost) { f.volt, f.voltOK = true, true },
			nil, // boolean: no value gap
			func(f *fakeHost) { f.volt = false },
		},
		{
			"mem percent", hostMemKey, notify.SeverityWarning, memEnterAfter,
			// 8 GiB host: the 10% floor (819 MiB) beats 64 MiB; clear needs 1024 MiB.
			func(f *fakeHost) { f.memTotal, f.memAvail, f.memOK = 8*gib, 8*gib*5/100, true },
			func(f *fakeHost) { f.memAvail = 8 * gib * 11 / 100 }, // 901 MiB: above onset, below clear
			func(f *fakeHost) { f.memAvail = 4 * gib },
		},
		{
			"mem mib", hostMemKey, notify.SeverityWarning, memEnterAfter,
			// 512 MiB host with 40 MiB free: 7.8% and under 64 MiB.
			func(f *fakeHost) { f.memTotal, f.memAvail, f.memOK = 512<<20, 40<<20, true },
			// 70 MiB: above the 64 MiB limit (which beats 10% = 51 MiB), below the 80 MiB clear.
			func(f *fakeHost) { f.memAvail = 70 << 20 },
			func(f *fakeHost) { f.memAvail = 200 << 20 },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeHost{}
			rec := newRecPub()
			c := newClk()
			h := newHostT(r, nil, rec, hostSettings(), c)

			tc.raise(r)
			h.poll()
			c.advance(tc.enterAfter - time.Second)
			h.poll()
			if rec.isActive(tc.key) {
				t.Fatal("onset before the dwell")
			}
			c.advance(time.Second)
			h.poll()
			n, ok := rec.lastOnset(tc.key)
			if !ok || !rec.isActive(tc.key) {
				t.Fatal("not active after the dwell")
			}
			if n.Severity != tc.sev || n.Key != tc.key || n.Message == "" {
				t.Errorf("onset = %+v, want severity %s", n, tc.sev)
			}
			if tc.gap != nil {
				tc.gap(r)
				pollEvery(h, c, 10*time.Second, 20)
				if !rec.isActive(tc.key) {
					t.Fatal("cleared inside the hysteresis gap")
				}
			}
			tc.recover(r)
			h.poll()
			pollEvery(h, c, 10*time.Second, 5)
			if !rec.isActive(tc.key) {
				t.Fatal("cleared before the clear dwell")
			}
			pollEvery(h, c, 10*time.Second, 1)
			if rec.isActive(tc.key) || rec.clearCount(tc.key) != 1 {
				t.Fatalf("not cleared at the clear dwell (clears = %d)", rec.clearCount(tc.key))
			}
		})
	}
}

// TestHostMemClearReachableWithHighFreePercent pins H2: with a high MemFreePercent
// the uncapped clear level limit*1.25 exceeds total, so available memory could
// never reach it and the low-memory warning would never clear. The cap (halfway
// between the limit and total) keeps it reachable.
func TestHostMemClearReachableWithHighFreePercent(t *testing.T) {
	const gib = int64(1) << 30
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	s := hostSettings()
	s.Host.MemFreePercent = p(85) // limit = 870 MiB; 1.25x = 1088 MiB > 1 GiB total
	h := newHostT(r, nil, rec, s, c)

	// Raise: 1 GiB host with 50% available (below the 85% limit).
	r.mu.Lock()
	r.memTotal, r.memAvail, r.memOK = gib, gib/2, true
	r.mu.Unlock()
	h.poll()
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostMemKey) {
		t.Fatal("low-memory not active after the dwell")
	}

	// Recover to 95% available: above the capped clear level (halfway to total) but
	// below the uncapped 1088 MiB, so it clears only because of the cap.
	r.mu.Lock()
	r.memAvail = gib * 95 / 100
	r.mu.Unlock()
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostMemKey) {
		t.Fatal("cleared before the 60 s clear dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostMemKey) || rec.clearCount(hostMemKey) != 1 {
		t.Fatalf("low-memory did not clear at 95%% available (clears=%d)", rec.clearCount(hostMemKey))
	}
}

func TestHostUnavailableReadings(t *testing.T) {
	r := &fakeHost{} // every reading ok=false
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)
	pollEvery(h, c, 10*time.Second, 50)
	if len(rec.onsets) != 0 {
		t.Fatalf("onsets with no readings = %d, want 0", len(rec.onsets))
	}

	// An active condition whose reader starts failing neither clears nor repeats.
	r.setCPU(99)
	h.poll()
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu not active")
	}
	r.mu.Lock()
	r.cpuOK = false
	r.mu.Unlock()
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostCPUKey) || rec.clearCount(hostCPUKey) != 0 || rec.resolveCount(hostCPUKey) != 0 {
		t.Fatal("a briefly failing reader ended the active cpu condition")
	}
	// A sensor gone for the whole clear dwell resolves the stale alert rather
	// than pinning it forever, and raises nothing further while still gone.
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostCPUKey) || rec.resolveCount(hostCPUKey) != 1 {
		t.Fatal("an active cpu condition with its sensor gone for 60 s was not resolved")
	}
	pollEvery(h, c, 10*time.Second, 30)
	if rec.onsetCount(hostCPUKey) != 1 || rec.resolveCount(hostCPUKey) != 1 {
		t.Error("a still-missing sensor raised or resolved again")
	}

	// The sensor-gone resolve reset the hysteresis, so a reading that returns and
	// stays over must serve a fresh full dwell before it re-onsets (pins the Reset:
	// without it the machine would stay active and never publish a second onset).
	r.setCPU(99)
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu re-onset before a fresh 60 s dwell after the sensor returned")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if !rec.isActive(hostCPUKey) || rec.onsetCount(hostCPUKey) != 2 {
		t.Fatalf("cpu did not re-onset after the sensor returned and a full dwell (onsets=%d)", rec.onsetCount(hostCPUKey))
	}

	// A zero total is treated as unavailable rather than dividing by zero. The disk
	// fixture reports used>0 with total 0 so the total>0 guard actually matters:
	// without it the condition would judge used/(used+avail) on a filesystem whose
	// statfs figures are absent and could onset spuriously.
	r.mu.Lock()
	r.memTotal, r.memAvail, r.memOK = 0, 0, true
	r.diskTotal, r.diskUse, r.diskAvail, r.diskOK = 0, 1<<30, 0, true
	r.mu.Unlock()
	pollEvery(h, c, 10*time.Second, 30)
	if rec.isActive(hostMemKey) || rec.isActive(hostDiskKey) {
		t.Error("zero totals raised a mem or disk condition")
	}
}

func TestHostApplyThresholdChange(t *testing.T) {
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)

	r.setCPU(85) // under the default 90
	h.poll()
	pollEvery(h, c, 10*time.Second, 10)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu active at 85% with a 90% threshold")
	}
	s := hostSettings()
	s.Host.CPUPercent = p(80)
	h.Apply(&s)
	pollEvery(h, c, 10*time.Second, 1) // run starts under the new threshold
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("lowered cpu threshold did not apply")
	}
	if !strings.Contains(rec.onsetMessage(hostCPUKey), "80%") {
		t.Errorf("onset message %q does not name the new threshold", rec.onsetMessage(hostCPUKey))
	}
}

func TestHostDisabledResolvesAllAndReadsNothing(t *testing.T) {
	r := &fakeHost{}
	r.setCPU(99)
	drops := uint64(0)
	srcCalls := 0
	var mu sync.Mutex
	src := func() []DeviceCounters {
		mu.Lock()
		defer mu.Unlock()
		srcCalls++
		return []DeviceCounters{{Name: nameGarden, Dropped: drops}}
	}
	dropsKey := streamDropsKey(nameGarden)
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, src, rec, hostSettings(), c)
	h.poll()
	for range 8 {
		c.advance(10 * time.Second)
		mu.Lock()
		drops += 100
		mu.Unlock()
		h.poll()
	}
	if !rec.isActive(hostCPUKey) || !rec.isActive(dropsKey) {
		t.Fatal("cpu and drops not both active before disable")
	}

	s := hostSettings()
	s.Enabled = false
	h.Apply(&s)
	c.advance(10 * time.Second)
	h.poll()
	if rec.resolveCount(hostCPUKey) != 1 || rec.resolveCount(dropsKey) != 1 {
		t.Fatal("disable did not resolve every active condition")
	}
	readsBefore := r.readCount()
	mu.Lock()
	callsBefore := srcCalls
	mu.Unlock()
	pollEvery(h, c, 10*time.Second, 20)
	if r.readCount() != readsBefore {
		t.Errorf("disabled monitor read the host %d times", r.readCount()-readsBefore)
	}
	mu.Lock()
	callsAfter := srcCalls
	mu.Unlock()
	if callsAfter != callsBefore {
		t.Errorf("disabled monitor called the counter source %d times, want 0", callsAfter-callsBefore)
	}
	if rec.onsetCount(hostCPUKey) != 1 {
		t.Error("onset while disabled")
	}

	// Re-enable starts fresh: the full dwell is needed again, and resolveAll cleared
	// the device counter state, so the drops condition must re-baseline and re-onset
	// too rather than staying silently stuck.
	h.Apply(new(hostSettings()))
	c.advance(10 * time.Second)
	mu.Lock()
	drops += 100
	mu.Unlock()
	h.poll() // fresh baseline for the device after clear(h.devs)
	if rec.isActive(hostCPUKey) {
		t.Fatal("re-enable onset immediately, want a fresh dwell")
	}
	for range 6 {
		c.advance(10 * time.Second)
		mu.Lock()
		drops += 100
		mu.Unlock()
		h.poll()
	}
	if !rec.isActive(hostCPUKey) || rec.onsetCount(hostCPUKey) != 2 {
		t.Error("cpu did not re-onset after re-enable and a full dwell")
	}
	if !rec.isActive(dropsKey) || rec.onsetCount(dropsKey) != 2 {
		t.Errorf("drops did not re-onset after re-enable (onsets=%d); clear(h.devs) not pinned", rec.onsetCount(dropsKey))
	}
}

func TestHostStartsDisabled(t *testing.T) {
	r := &fakeHost{}
	r.setCPU(99)
	rec := newRecPub()
	c := newClk()
	s := hostSettings()
	s.Enabled = false
	h := newHostT(r, nil, rec, s, c)
	pollEvery(h, c, 10*time.Second, 20)
	if r.readCount() != 0 || len(rec.onsets) != 0 || len(rec.resolves) != 0 {
		t.Errorf("disabled-at-start monitor: reads=%d onsets=%d resolves=%d, want 0", r.readCount(), len(rec.onsets), len(rec.resolves))
	}
}

// counterFeed is a mutable CounterSource for the dropped-frame and overrun tests.
type counterFeed struct {
	devs []DeviceCounters
}

func (d *counterFeed) source() []DeviceCounters { return append([]DeviceCounters(nil), d.devs...) }

func TestHostDropsOnsetAndClear(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 1000}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline only: the cumulative 1000 is not a rate
	if rec.isActive(key) {
		t.Fatal("baseline poll raised")
	}
	// 5 frames/s: over the 1/s threshold.
	for i := range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 50
		h.poll()
		if i < 3 && rec.isActive(key) {
			t.Fatalf("drops onset after %d polls, before 30 s", i+1)
		}
	}
	if !rec.isActive(key) {
		t.Fatal("drops not active after 30 s at 5 frames/s")
	}
	if n, _ := rec.lastOnset(key); n.Category != notify.CategoryStream || !strings.Contains(n.Message, "5.0 frames/s") {
		t.Errorf("drops onset = %+v", n)
	}

	// While active, a trickle under the onset rate still holds the condition.
	for range 10 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped++
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops cleared while frames were still dropping")
	}
	// 60 s with no drops clears.
	h.poll()
	pollEvery(h, c, 10*time.Second, 7)
	if rec.isActive(key) || rec.clearCount(key) != 1 {
		t.Fatal("drops not cleared after 60 s without drops")
	}
	// Under the rate while inactive never onsets.
	for range 20 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 5 // 0.5/s
		h.poll()
	}
	if rec.onsetCount(key) != 1 {
		t.Errorf("drops onsets = %d, want 1", rec.onsetCount(key))
	}
}

// TestHostDropsRateBoundary pins the onset comparison (> dropsPerSecond): a
// steady exactly 1.0 frames/s never onsets.
func TestHostDropsRateBoundary(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 0}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline
	// Exactly 1.0 frames/s (10 dropped per 10 s poll) is not over the threshold.
	for range 10 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 10
		h.poll()
	}
	if rec.isActive(key) || rec.onsetCount(key) != 0 {
		t.Fatalf("drops onset at exactly 1.0 frames/s (onsets=%d), want 0 (> boundary)", rec.onsetCount(key))
	}
}

func TestHostDropsCounterResetAndDeviceRemoval(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 0}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)
	h.poll()
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 100
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops not active")
	}

	// Restart: the counter drops to a small value. No negative rate, no new
	// onset, and the condition clears through the normal no-drop dwell.
	feed.devs[0].Dropped = 3
	c.advance(10 * time.Second)
	h.poll()
	if rec.onsetCount(key) != 1 || !rec.isActive(key) {
		t.Fatal("counter reset changed the condition immediately")
	}
	pollEvery(h, c, 10*time.Second, 1)
	pollEvery(h, c, 10*time.Second, 6)
	if rec.isActive(key) {
		t.Fatal("drops did not clear after the restarted device stopped dropping")
	}

	// Re-raise, then remove the device: resolved after the presence grace.
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 100
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops not re-raised")
	}
	feed.devs = nil
	c.advance(10 * time.Second)
	h.poll()
	if rec.resolveCount(key) != 0 {
		t.Fatal("resolved within the presence grace")
	}
	c.advance(10 * time.Second)
	h.poll()
	if rec.resolveCount(key) != 1 {
		t.Fatal("removed device not resolved after the grace")
	}
	if len(h.devs) != 0 {
		t.Errorf("removed device state retained: %d entries", len(h.devs))
	}
}

// TestHostPendingOnsetRunAbandonedByReadingGap pins H1: a pending onset run must
// not survive a gap in the readings, so a single available reading after the gap
// cannot complete an onset from readings taken minutes apart.
func TestHostPendingOnsetRunAbandonedByReadingGap(t *testing.T) {
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)

	// A pending onset run: 95% for 50 s (short of the 60 s dwell).
	r.setCPU(95)
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu onset before 60 s")
	}
	// The sensor is unavailable for ten minutes.
	r.mu.Lock()
	r.cpuOK = false
	r.mu.Unlock()
	pollEvery(h, c, 10*time.Second, 60)
	// One available 95% reading must NOT complete the onset off the stale run.
	r.setCPU(95)
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostCPUKey) {
		t.Fatal("cpu onset from a single reading after a gap; the pending run was not abandoned")
	}
	// A fresh contiguous 60 s of over readings onsets.
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu did not onset after a fresh 60 s of available readings")
	}
}

// TestHostDropsClearRunSurvivesCounterRestartAtCompletion pins H1: when a device
// restarts with a fresh (smaller) counter on the very poll the clear dwell
// completes, the clear must still fire at 60 s, not be skipped and pushed to 70 s.
func TestHostDropsClearRunSurvivesCounterRestartAtCompletion(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 1000}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline at t=0
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 50 // 5 frames/s, onsets by 30 s
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops not active before the clear run")
	}
	// Drops stop: the clear run begins on the first no-drop poll (t=50 s) and
	// reaches its 60 s dwell at t=110 s. Feed six no-drop polls (t=50..100 s).
	for range 6 {
		c.advance(10 * time.Second)
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops cleared before the clear dwell")
	}
	// At t=110 s the device restarts with a fresh, smaller counter exactly as the
	// clear dwell completes. The clear must still fire on this poll.
	feed.devs[0].Dropped = 5
	c.advance(10 * time.Second)
	h.poll()
	if rec.clearCount(key) != 1 || rec.isActive(key) {
		t.Fatalf("drops did not clear at 60 s when the counter restarted on the clear poll (clears=%d)", rec.clearCount(key))
	}
}

// TestHostDropsPendingRunAbandonedByAbsentBlip pins H1: a device that blips absent
// for one poll (within the presence grace) while a drops onset run is pending must
// have that run abandoned, so it cannot onset off the stale start time when it
// returns.
func TestHostDropsPendingRunAbandonedByAbsentBlip(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 0}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline t=0
	// A pending onset run: 5 frames/s, but only 20 s in (short of the 30 s dwell).
	for range 2 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 50
		h.poll()
	}
	if rec.isActive(key) {
		t.Fatal("drops onset before the 30 s dwell")
	}
	// The device blips absent for a single poll (within the presence grace).
	saved := feed.devs
	feed.devs = nil
	c.advance(10 * time.Second)
	h.poll()
	// It returns, still dropping. The onset must NOT fire off the stale pending run:
	// the absent poll abandoned it, so a fresh 30 s is required.
	feed.devs = saved
	feed.devs[0].Dropped += 50
	c.advance(10 * time.Second)
	h.poll()
	if rec.isActive(key) {
		t.Fatal("drops onset from a stale pending run across an absent blip")
	}
}

// TestHostDropsAbsentPresentAbsentKeepsCondition pins the missed=0 reset on a
// present poll: an active condition survives an absent/present/absent sequence
// without being resolved, because the present poll resets the missed counter.
func TestHostDropsAbsentPresentAbsentKeepsCondition(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 0}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 100
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops not active")
	}
	saved := feed.devs
	feed.devs = nil // absent: missed -> 1
	c.advance(10 * time.Second)
	h.poll()
	feed.devs = saved // present: missed reset to 0
	c.advance(10 * time.Second)
	h.poll()
	feed.devs = nil // absent again: missed -> 1, still under the grace
	c.advance(10 * time.Second)
	h.poll()
	if rec.resolveCount(key) != 0 {
		t.Fatalf("device resolved despite a present poll between absences (resolves=%d)", rec.resolveCount(key))
	}
	if !rec.isActive(key) {
		t.Fatal("condition lost across an absent/present/absent sequence")
	}
}

// TestHostDropsStopServingMidClearRunResolves covers a device that leaves
// Serving while its drops condition is part way through the clear run: it must end
// with the grace resolve, not a clear claiming its client caught up.
func TestHostDropsStopServingMidClearRunResolves(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Dropped: 0}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 100
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops not active")
	}
	// Drops stop: the clear run starts and advances 50 s, just short of the dwell.
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(key) {
		t.Fatal("drops cleared before the device stopped serving")
	}
	feed.devs = nil // stopped serving
	pollEvery(h, c, 10*time.Second, 2)
	if rec.clearCount(key) != 0 {
		t.Errorf("absent device got a clear (clears=%d), want only the grace resolve", rec.clearCount(key))
	}
	if rec.resolveCount(key) != 1 {
		t.Errorf("resolves = %d, want 1 (device stopped)", rec.resolveCount(key))
	}
}

// TestHostDropsRebaselineOnGenerationChange pins the runtime-generation
// rebaseline: when a device restarts with a fresh runtime (a new Gen) whose
// counter has already climbed past the old value between polls, the monitor must
// rebaseline on the Gen change rather than diff two runtimes' counters (which
// would count a cross-runtime delta the counter-went-backwards heuristic cannot
// see). Crucially the tracker must adopt the new Gen, so a genuinely dropping
// fresh runtime can still onset instead of rebaselining on every poll forever.
func TestHostDropsRebaselineOnGenerationChange(t *testing.T) {
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1, Dropped: 0}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)

	h.poll() // baseline: gen 1, prev 0
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 50 // 5 frames/s under gen 1, onsets by 30 s
		h.poll()
	}
	if !rec.isActive(key) {
		t.Fatal("drops not active under gen 1")
	}

	// Restart: a fresh runtime (gen 2) whose counter has already climbed FORWARD
	// past the old prev (200) between polls. The counter-went-backwards heuristic
	// cannot see this restart; only the Gen change can. The poll must rebaseline
	// and observe no drops (starting the clear run), not count the 4800-frame
	// cross-runtime delta as a fresh onset.
	feed.devs[0].Gen = 2
	feed.devs[0].Dropped = 5000
	c.advance(10 * time.Second)
	h.poll()
	if rec.onsetCount(key) != 1 {
		t.Fatalf("generation change raised a fresh onset from a cross-runtime delta (onsets=%d)", rec.onsetCount(key))
	}
	// The rebaseline observed no drops at the restart poll, so the clear run started
	// there and, with a 60 s dwell, clears at exactly the 6th no-drop poll. This
	// budget pins the Gen CONDITION, not just its adoption: a counter-backwards-only
	// implementation would read the 4800-frame cross-runtime delta as "over" at the
	// restart poll, hold the condition, and only start the clear run one poll later,
	// so it would still be active here and fail this assertion.
	for range 6 {
		c.advance(10 * time.Second)
		h.poll()
	}
	if rec.isActive(key) || rec.clearCount(key) != 1 {
		t.Fatalf("drops did not clear 60 s after the generation-change rebaseline (clears=%d, active=%v); the Gen condition is not pinned", rec.clearCount(key), rec.isActive(key))
	}

	// The tracker must now be baselined on gen 2: a genuinely dropping gen-2 runtime
	// onsets again. If st.gen had not been adopted on the rebaseline, every poll
	// would rebaseline and the condition could never fire.
	for range 4 {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 50 // 5 frames/s under gen 2
		h.poll()
	}
	if !rec.isActive(key) || rec.onsetCount(key) != 2 {
		t.Fatalf("gen-2 runtime did not re-onset (onsets=%d); st.gen was not adopted on rebaseline", rec.onsetCount(key))
	}
}

func TestHostNilPublisherAndReadersDoNotPanic(t *testing.T) {
	s := hostSettings()
	h := NewHost(nil, nil, nil, &s)
	h.poll()
	r := &fakeHost{}
	r.setCPU(99)
	h = NewHost(r, nil, nil, &s)
	c := newClk()
	h.clock = c.now
	h.poll()
	pollEvery(h, c, 10*time.Second, 7)
}

// TestRunHostStopsOnCancel drives RunHost's own goroutine under synctest: it must
// poll on its ticker, and it must return promptly when the context is cancelled.
// The bubble fails the test if the goroutine is still blocked when it returns.
func TestRunHostStopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &fakeHost{}
		r.setCPU(50) // an available reading, so a poll actually reads the host
		s := hostSettings()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // ensure the goroutine unblocks even if an assertion fails
		h := RunHost(ctx, r, nil, newRecPub(), &s)
		if h == nil {
			t.Fatal("RunHost returned nil")
		}
		// Let one tick fire, then drain: the goroutine must have polled the reader.
		time.Sleep(hostPollInterval + time.Second)
		synctest.Wait()
		if r.readCount() == 0 {
			t.Fatal("RunHost goroutine did not poll the reader")
		}
		cancel()
		synctest.Wait() // the goroutine must observe ctx.Done and return
	})
}

// overrunPoll advances the clock by step, adds n overruns to the first device,
// and polls.
func overrunPoll(h *Host, c *clk, feed *counterFeed, step time.Duration, n uint64) {
	c.advance(step)
	feed.devs[0].Overruns += n
	h.poll()
}

// overrunOnsetText and overrunClearText are the fragments the overrun onset and
// clear messages must carry, derived from the thresholds so tuning a constant
// does not break the message assertions.
func overrunOnsetText() string {
	return fmt.Sprintf("at least %d capture overruns within %s", overrunOnsetCount, humanDuration(int(overrunWindow/time.Second)))
}

func overrunClearText() string {
	return "no capture overruns for " + humanDuration(int(overrunClearAfter/time.Second))
}

// pollsIn is how many 10 s polls fit in d.
func pollsIn(d time.Duration) int { return int(d / (10 * time.Second)) }

func TestHostOverrunsOnsetAndClear(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1, Overruns: 7}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := deviceOverrunsKey(nameGarden)

	h.poll() // baseline only: the cumulative 7 predates this monitor
	for i := range overrunOnsetCount - 1 {
		overrunPoll(h, c, feed, 30*time.Second, 1)
		if rec.isActive(key) {
			t.Fatalf("overrun onset after %d overruns, want %d", i+1, overrunOnsetCount)
		}
	}
	overrunPoll(h, c, feed, 30*time.Second, 1)
	if !rec.isActive(key) {
		t.Fatalf("no overrun onset after %d overruns within the window", overrunOnsetCount)
	}
	n, _ := rec.lastOnset(key)
	if n.Category != notify.CategoryDevice || n.Severity != notify.SeverityWarning || n.Source != nameGarden || n.Title != "Capture overruns" {
		t.Errorf("overrun onset = %+v, want a device warning titled %q sourced %q", n, "Capture overruns", nameGarden)
	}
	if want := overrunOnsetText(); !strings.Contains(n.Message, want) {
		t.Errorf("overrun onset message = %q, want it to contain %q", n.Message, want)
	}

	// Overruns keep coming, a minute apart: the condition holds.
	for range 10 {
		overrunPoll(h, c, feed, time.Minute, 1)
	}
	if !rec.isActive(key) || rec.onsetCount(key) != 1 {
		t.Fatalf("active=%v onsets=%d while overruns continue, want active with 1 onset", rec.isActive(key), rec.onsetCount(key))
	}

	// Quiet: it clears once overrunClearAfter passes with none, not before.
	pollEvery(h, c, 10*time.Second, pollsIn(overrunClearAfter)-1)
	if !rec.isActive(key) {
		t.Fatal("overrun condition cleared before the quiet dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(key) || rec.clearCount(key) != 1 {
		t.Fatalf("active=%v clears=%d after the quiet dwell, want cleared once", rec.isActive(key), rec.clearCount(key))
	}
	if msg, want := rec.clearMessage(key), overrunClearText(); !strings.Contains(msg, want) {
		t.Errorf("overrun clear message = %q, want it to contain %q", msg, want)
	}

	// A fresh overrun after the clear starts a new window rather than re-raising.
	overrunPoll(h, c, feed, 10*time.Second, 1)
	if rec.isActive(key) {
		t.Fatal("a single overrun after the clear re-raised the condition")
	}
}

// Overruns two minutes apart put at most three in any window, fewer than
// overrunOnsetCount, so they never onset however long they go on.
func TestHostOverrunsSporadicNeverOnset(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	h.poll()
	for range 60 { // two hours, one overrun every two minutes
		overrunPoll(h, c, feed, 2*time.Minute, 1)
	}
	if n := rec.onsetCount(deviceOverrunsKey(nameGarden)); n != 0 {
		t.Errorf("sporadic overruns raised %d onsets, want 0", n)
	}
}

// The window is closed, as notify.Flap keeps it: an overrun exactly
// overrunWindow old still counts, one a poll older does not.
func TestHostOverrunsWindowBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		after time.Duration
		want  bool
	}{
		{"exactly the window", overrunWindow, true},
		{"one poll past the window", overrunWindow + 10*time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
			rec := newRecPub()
			c := newClk()
			h := newHostT(nil, feed.source, rec, hostSettings(), c)
			h.poll()
			overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount-1)
			overrunPoll(h, c, feed, tc.after, 1)
			if got := rec.isActive(deviceOverrunsKey(nameGarden)); got != tc.want {
				t.Errorf("active = %v, want %v", got, tc.want)
			}
		})
	}
}

// A restarted runtime's counter starts from zero after the poll that last saw
// its predecessor, so its whole count is new: a Gen change, or without a Gen a
// counter going backwards, counts the fresh value instead of diffing it. The
// polls after it must diff against the new runtime, or a restart would be
// re-detected and re-counted on every poll and the warning would never clear.
func TestHostOverrunsRestartCountsFreshCounter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		first      DeviceCounters
		next       DeviceCounters
		wantActive bool
	}{
		// The fresh counter already passed the old value: diffing would see only 3.
		{"new gen", DeviceCounters{Name: nameGarden, Gen: 1, Overruns: 2}, DeviceCounters{Name: nameGarden, Gen: 2, Overruns: overrunOnsetCount}, true},
		{"counter went backwards", DeviceCounters{Name: nameGarden, Overruns: 100}, DeviceCounters{Name: nameGarden, Overruns: overrunOnsetCount}, true},
		// Diffing across the reset would underflow into a huge count and onset.
		{"counter went backwards below the onset", DeviceCounters{Name: nameGarden, Overruns: 100}, DeviceCounters{Name: nameGarden, Overruns: overrunOnsetCount - 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			feed := &counterFeed{devs: []DeviceCounters{tc.first}}
			rec := newRecPub()
			c := newClk()
			h := newHostT(nil, feed.source, rec, hostSettings(), c)
			key := deviceOverrunsKey(nameGarden)
			h.poll()
			c.advance(10 * time.Second)
			feed.devs[0] = tc.next
			h.poll()
			if got := rec.isActive(key); got != tc.wantActive {
				t.Fatalf("active after the restart poll = %v, want %v", got, tc.wantActive)
			}
			if !tc.wantActive {
				return
			}
			// Counters held steady from here: still active one poll short of the
			// quiet dwell, cleared exactly once at it.
			pollEvery(h, c, 10*time.Second, pollsIn(overrunClearAfter)-1)
			if !rec.isActive(key) {
				t.Fatal("cleared before the quiet dwell after the restart")
			}
			pollEvery(h, c, 10*time.Second, 1)
			if rec.isActive(key) || rec.clearCount(key) != 1 || rec.onsetCount(key) != 1 {
				t.Errorf("after the quiet dwell: active=%v clears=%d onsets=%d, want cleared once after one onset",
					rec.isActive(key), rec.clearCount(key), rec.onsetCount(key))
			}
		})
	}
}

// A restart while the warning is up neither drops nor re-raises it: the
// condition stays active and clears once, through the normal quiet dwell.
func TestHostOverrunsRestartWhileActive(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := deviceOverrunsKey(nameGarden)
	h.poll()
	overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount)
	if !rec.isActive(key) {
		t.Fatal("overrun condition not raised")
	}

	c.advance(10 * time.Second)
	feed.devs[0] = DeviceCounters{Name: nameGarden, Gen: 2}
	h.poll()
	if !rec.isActive(key) {
		t.Fatal("a restart with no new overruns dropped the active condition")
	}
	// The last overrun was one poll before the restart, so the dwell ends one
	// poll sooner than a full overrunClearAfter from here.
	pollEvery(h, c, 10*time.Second, pollsIn(overrunClearAfter)-2)
	if !rec.isActive(key) {
		t.Fatal("cleared before the quiet dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(key) || rec.clearCount(key) != 1 || rec.resolveCount(key) != 0 {
		t.Errorf("active=%v clears=%d resolves=%d, want cleared once and never resolved", rec.isActive(key), rec.clearCount(key), rec.resolveCount(key))
	}
}

// The quiet dwell is judged per poll: the poll that completes it clears the
// condition before counting its own overruns, which start a fresh window. A
// full burst there clears and re-raises it at once; a smaller one leaves it
// cleared with those overruns pending toward the next onset. Either way the
// clear is published, never skipped.
func TestHostOverrunsBurstOnQuietBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		burst      uint64
		wantActive bool
	}{
		{"full burst re-raises", overrunOnsetCount, true},
		{"small burst stays cleared", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
			rec := newRecPub()
			c := newClk()
			h := newHostT(nil, feed.source, rec, hostSettings(), c)
			key := deviceOverrunsKey(nameGarden)
			h.poll()
			overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount)
			pollEvery(h, c, 10*time.Second, pollsIn(overrunClearAfter)-1)
			if !rec.isActive(key) || rec.clearCount(key) != 0 || rec.onsetCount(key) != 1 {
				t.Fatalf("before the boundary: active=%v clears=%d onsets=%d, want true/0/1",
					rec.isActive(key), rec.clearCount(key), rec.onsetCount(key))
			}
			overrunPoll(h, c, feed, 10*time.Second, tc.burst)
			wantOnsets := 1
			if tc.wantActive {
				wantOnsets = 2
			}
			if rec.clearCount(key) != 1 || rec.onsetCount(key) != wantOnsets || rec.isActive(key) != tc.wantActive {
				t.Fatalf("boundary poll: clears=%d onsets=%d active=%v, want 1/%d/%v",
					rec.clearCount(key), rec.onsetCount(key), rec.isActive(key), wantOnsets, tc.wantActive)
			}
			if tc.wantActive {
				return
			}
			// The boundary poll's overruns count toward the next onset: the rest of
			// a full burst one poll later raises it.
			overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount-tc.burst)
			if !rec.isActive(key) || rec.onsetCount(key) != 2 {
				t.Errorf("after the rest of the burst: active=%v onsets=%d, want true/2", rec.isActive(key), rec.onsetCount(key))
			}
		})
	}
}

// A pending window survives an absence inside the presence grace: it counts
// overruns by wall-clock time, so the gap cannot stretch it.
func TestHostOverrunsPendingWindowSpansAbsentBlip(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	h.poll()
	overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount-1)
	saved := feed.devs[0]
	feed.devs = nil
	pollEvery(h, c, 10*time.Second, devicePresenceGrace-1)
	feed.devs = []DeviceCounters{saved}
	overrunPoll(h, c, feed, 10*time.Second, 1)
	if !rec.isActive(deviceOverrunsKey(nameGarden)) {
		t.Error("the overruns before the blip no longer counted toward the onset")
	}
}

func TestHostOverrunsResolvedOnRemovalAndDisable(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := deviceOverrunsKey(nameGarden)
	h.poll()
	overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount)
	if !rec.isActive(key) {
		t.Fatal("overrun condition not raised")
	}

	// Removed: kept through the presence grace, then resolved, never cleared as
	// if recovered.
	saved := feed.devs[0]
	feed.devs = nil
	pollEvery(h, c, 10*time.Second, devicePresenceGrace-1)
	if !rec.isActive(key) || rec.resolveCount(key) != 0 {
		t.Fatal("the condition did not survive an absence inside the presence grace")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.resolveCount(key) != 1 || rec.clearCount(key) != 0 {
		t.Fatalf("removed device: resolves=%d clears=%d, want 1 resolve and no clear", rec.resolveCount(key), rec.clearCount(key))
	}
	if got, want := rec.resolveReason(key), "device stopped"; got != want {
		t.Errorf("resolve reason = %q, want %q", got, want)
	}

	// Back, raised again, then notifications disabled: resolved.
	feed.devs = []DeviceCounters{saved}
	pollEvery(h, c, 10*time.Second, 1) // first sighting again: baseline
	overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount)
	if !rec.isActive(key) {
		t.Fatal("overrun condition not re-raised")
	}
	s := hostSettings()
	s.Enabled = false
	h.Apply(&s)
	pollEvery(h, c, 10*time.Second, 1)
	if rec.resolveCount(key) != 2 || rec.clearCount(key) != 0 {
		t.Fatalf("disable: resolves=%d clears=%d, want 2 resolves and no clear", rec.resolveCount(key), rec.clearCount(key))
	}
	if got, want := rec.resolveReason(key), "notifications disabled"; got != want {
		t.Errorf("disable resolve reason = %q, want %q", got, want)
	}
}

// A Gen-less runtime whose overrun counter goes backwards has restarted even
// when its drop counter kept climbing, so the drops condition rebaselines
// rather than carrying a pending onset run across the restart.
func TestHostDropsRebaselineOnOverrunCounterReset(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Overruns: 10}}}
	rec := newRecPub()
	c := newClk()
	h := newHostT(nil, feed.source, rec, hostSettings(), c)
	key := streamDropsKey(nameGarden)
	h.poll() // t=0: baseline
	step := func(overruns uint64) {
		c.advance(10 * time.Second)
		feed.devs[0].Dropped += 100 // 10 frames/s, over the onset rate
		feed.devs[0].Overruns = overruns
		h.poll()
	}
	step(10) // t=10: the drops onset run starts
	step(2)  // t=20: the overrun counter went backwards, a restart
	step(2)  // t=30
	step(2)  // t=40: a run kept from t=10 would onset here
	if rec.isActive(key) {
		t.Fatal("the drops onset run survived a restart signalled by the overrun counter")
	}
	step(2) // t=50
	step(2) // t=60: the run restarted at t=30 completes its 30 s
	if !rec.isActive(key) {
		t.Error("drops did not onset after the restart's fresh 30 s run")
	}
}

// gardenOneOverrunLine is the line for a single overrun logged at once.
const gardenOneOverrunLine = `device "garden": 1 capture overrun(s), audio lost`

// logRec captures the host monitor's log lines.
type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRec) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// overrunLogCount parses the overrun count out of a sub-threshold line, or
// reports ok=false for any other line.
func overrunLogCount(line string) (n uint64, ok bool) {
	var name string
	if _, err := fmt.Sscanf(line, "device %q: %d capture overrun(s)", &name, &n); err != nil {
		return 0, false
	}
	return n, true
}

// newHostLogT is newHostT with the monitor's log lines captured.
//
//nolint:gocritic // test helper: taking Settings by value keeps call sites terse.
func newHostLogT(counters CounterSource, rec *recPub, s Settings, c *clk) (*Host, *logRec) {
	lr := &logRec{}
	return NewHost(nil, counters, rec, &s, WithHostClock(c.now), WithHostLogf(lr.logf)), lr
}

// Sub-threshold overruns log at once when no line was written in the last
// window, then at most one line per overrunWindow carrying what accumulated,
// flushed even by a quiet poll; a device with no overruns logs nothing.
func TestHostOverrunLogRateLimited(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
	c := newClk()
	h, lr := newHostLogT(feed.source, newRecPub(), hostSettings(), c)
	h.poll()
	overrunPoll(h, c, feed, 10*time.Second, 1) // t=10
	if got := lr.all(); len(got) != 1 || got[0] != gardenOneOverrunLine {
		t.Fatalf("after the first overrun: lines = %q, want one immediate line", got)
	}
	overrunPoll(h, c, feed, 10*time.Second, 2) // t=20
	overrunPoll(h, c, feed, 10*time.Second, 1) // t=30
	pollEvery(h, c, 10*time.Second, pollsIn(overrunWindow)-3)
	if got := lr.all(); len(got) != 1 {
		t.Fatalf("inside the window: lines = %q, want still one", got)
	}
	pollEvery(h, c, 10*time.Second, 1) // t=310: the window since t=10 has passed
	if got := lr.all(); len(got) != 2 || got[1] != `device "garden": 3 capture overrun(s) since the last overrun report, audio lost` {
		t.Fatalf("after the window: lines = %q, want the 3 held back reported by the quiet poll", got)
	}
	pollEvery(h, c, 10*time.Second, 2*pollsIn(overrunWindow))
	if got := lr.all(); len(got) != 2 {
		t.Fatalf("quiet device: lines = %q, want no new line", got)
	}

	// A steady stream just under the threshold (two every 160 s, at most four in
	// any window) for two hours writes about one line per window and loses no
	// count.
	before := len(lr.all())
	const (
		gap    = 160 * time.Second
		bursts = 45
	)
	for range bursts {
		overrunPoll(h, c, feed, gap, 2)
	}
	pollEvery(h, c, 10*time.Second, pollsIn(overrunWindow)) // flush the tail
	lines := lr.all()[before:]
	var sum uint64
	for _, l := range lines {
		n, ok := overrunLogCount(l)
		if !ok {
			t.Fatalf("unexpected line %q in the steady stream", l)
		}
		sum += n
	}
	if maxLines := bursts*int(gap/time.Second)/int(overrunWindow/time.Second) + 2; len(lines) > maxLines {
		t.Errorf("steady stream wrote %d lines, want at most %d", len(lines), maxLines)
	}
	if sum != 2*bursts {
		t.Errorf("steady stream lines report %d overruns, want %d", sum, 2*bursts)
	}
}

// The onset line speaks for overruns not yet reported, overruns while raised
// write no line but are counted into the clear line, and after the clear the
// next overrun is logged at once rather than held for the window.
func TestHostOverrunLogOnsetAndClear(t *testing.T) {
	t.Parallel()
	feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
	c := newClk()
	h, lr := newHostLogT(feed.source, newRecPub(), hostSettings(), c)
	h.poll()
	overrunPoll(h, c, feed, 10*time.Second, 1)                   // t=10: logged at once
	overrunPoll(h, c, feed, 10*time.Second, 2)                   // t=20: held back
	overrunPoll(h, c, feed, 10*time.Second, overrunOnsetCount-3) // t=30: onset
	overrunPoll(h, c, feed, 10*time.Second, 2)                   // t=40: while raised
	pollEvery(h, c, 10*time.Second, pollsIn(overrunClearAfter))  // t=340: clear
	if got := lr.all(); len(got) != 3 {
		t.Fatalf("after the clear: lines = %q, want onset and clear after the first line only", got)
	}
	overrunPoll(h, c, feed, 10*time.Second, 1) // t=350
	episode := uint64(overrunOnsetCount-3) + 2
	want := []string{
		gardenOneOverrunLine,
		`device "garden": ` + overrunOnsetText() + `, audio lost; raising an overrun warning`,
		fmt.Sprintf(`device "garden": no capture overruns for %s, overrun warning cleared (%d overrun(s) since the check that raised it)`,
			humanDuration(int(overrunClearAfter/time.Second)), episode),
		gardenOneOverrunLine,
	}
	if got := lr.all(); !slices.Equal(got, want) {
		t.Errorf("lines =\n%q\nwant\n%q", got, want)
	}
}

// A device first seen with overruns already counted logs its running total
// once, and that line starts the rate-limit window; one first seen with none
// logs nothing and its first overrun is logged at once.
func TestHostOverrunLogFirstSighting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		count uint64
		want  []string
	}{
		{"with overruns", 7, []string{`device "garden": capture has recovered from 7 overrun(s) since it opened`}},
		{"with one overrun", 1, []string{`device "garden": capture has recovered from 1 overrun(s) since it opened`}},
		{"without overruns", 0, []string{gardenOneOverrunLine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1, Overruns: tc.count}}}
			c := newClk()
			h, lr := newHostLogT(feed.source, newRecPub(), hostSettings(), c)
			h.poll()
			overrunPoll(h, c, feed, 10*time.Second, 1)
			if got := lr.all(); !slices.Equal(got, tc.want) {
				t.Errorf("lines = %q, want %q", got, tc.want)
			}
		})
	}
}

// When a device stops serving or notifications are turned off, the overrun
// state owes a line before it goes: the raised warning's count, or the
// overruns the rate limit still held back. A device that owes nothing writes
// no further line, and the owed line is written exactly once.
func TestHostOverrunLogFlushOnStop(t *testing.T) {
	t.Parallel()
	// The raised cases start with more than the onset count in one poll, so an
	// episode that capped the onset poll's overruns would under-report.
	raised := []uint64{overrunOnsetCount + 3, 2}
	raisedTotal := overrunOnsetCount + 5
	for _, tc := range []struct {
		name    string
		bursts  []uint64
		disable bool
		last    string
	}{
		{"held back, device stops", []uint64{1, 2}, false,
			`device "garden": device stopped; 2 capture overrun(s) since the last overrun report, audio lost`},
		{"held back, notifications disabled", []uint64{1, 2}, true,
			`device "garden": notifications disabled; 2 capture overrun(s) since the last overrun report, audio lost`},
		{"raised, device stops", raised, false,
			fmt.Sprintf(`device "garden": device stopped; overrun warning resolved after %d overrun(s) since the check that raised it`, raisedTotal)},
		{"raised, notifications disabled", raised, true,
			fmt.Sprintf(`device "garden": notifications disabled; overrun warning resolved after %d overrun(s) since the check that raised it`, raisedTotal)},
		{"nothing owed, device stops", []uint64{1}, false, gardenOneOverrunLine},
		{"nothing owed, notifications disabled", []uint64{1}, true, gardenOneOverrunLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			feed := &counterFeed{devs: []DeviceCounters{{Name: nameGarden, Gen: 1}}}
			c := newClk()
			h, lr := newHostLogT(feed.source, newRecPub(), hostSettings(), c)
			h.poll()
			for _, n := range tc.bursts {
				overrunPoll(h, c, feed, 10*time.Second, n)
			}
			before := len(lr.all())
			if tc.disable {
				s := hostSettings()
				s.Enabled = false
				h.Apply(&s)
				pollEvery(h, c, 10*time.Second, 1)
			} else {
				feed.devs = nil
				pollEvery(h, c, 10*time.Second, devicePresenceGrace)
			}
			got := lr.all()
			wantNew := 1
			if tc.last == gardenOneOverrunLine {
				wantNew = 0
			}
			if len(got)-before != wantNew || len(got) == 0 || got[len(got)-1] != tc.last {
				t.Errorf("lines = %q, want %d new line(s) ending with %q", got, wantNew, tc.last)
			}
		})
	}
}
