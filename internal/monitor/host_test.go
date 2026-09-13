package monitor

import (
	"context"
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

func (f *fakeHost) Disk() (total, used int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.diskTotal, f.diskUse, f.diskOK
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
func newHostT(r HostReader, drops DropSource, rec *recPub, s Settings, c *clk) *Host {
	return NewHost(r, drops, rec, &s, WithHostClock(c.now))
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
			func(f *fakeHost) { f.diskTotal, f.diskUse, f.diskOK = 100*gib, 95*gib, true },
			func(f *fakeHost) { f.diskUse = 88 * gib },
			func(f *fakeHost) { f.diskUse = 50 * gib },
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
	// without it usedPct would be +Inf, which is >= the threshold and would onset.
	r.mu.Lock()
	r.memTotal, r.memAvail, r.memOK = 0, 0, true
	r.diskTotal, r.diskUse, r.diskOK = 0, 1<<30, true
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
	src := func() []DeviceDrops {
		mu.Lock()
		defer mu.Unlock()
		srcCalls++
		return []DeviceDrops{{Name: nameGarden, Dropped: drops}}
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
		t.Errorf("disabled monitor called the drop source %d times", callsAfter-callsBefore)
	}
	if rec.onsetCount(hostCPUKey) != 1 {
		t.Error("onset while disabled")
	}

	// Re-enable starts fresh: the full dwell is needed again, and resolveAll cleared
	// the device drop state, so the drops condition must re-baseline and re-onset
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

// dropFeed is a mutable DropSource for the dropped-frame tests.
type dropFeed struct {
	devs []DeviceDrops
}

func (d *dropFeed) source() []DeviceDrops { return append([]DeviceDrops(nil), d.devs...) }

func TestHostDropsOnsetAndClear(t *testing.T) {
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 1000}}}
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
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 0}}}
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
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 0}}}
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
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 1000}}}
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
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 0}}}
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
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 0}}}
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
	feed := &dropFeed{devs: []DeviceDrops{{Name: nameGarden, Dropped: 0}}}
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
