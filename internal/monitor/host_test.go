package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
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

	// Inside the gap while inactive never onsets.
	r.setCPU(85)
	pollEvery(h, c, 10*time.Second, 30)
	if rec.onsetCount(hostCPUKey) != 1 {
		t.Errorf("cpu onsets = %d, want 1 (85%% is below the onset threshold)", rec.onsetCount(hostCPUKey))
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
			pollEvery(h, c, 10*time.Second, 6)
			if rec.isActive(tc.key) || rec.clearCount(tc.key) != 1 {
				t.Fatalf("not cleared after recovery (clears = %d)", rec.clearCount(tc.key))
			}
		})
	}
}

func TestHostUnavailableReadingsNeverRaiseOrClear(t *testing.T) {
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

	// A zero total is treated as unavailable rather than dividing by zero.
	r.mu.Lock()
	r.memTotal, r.memAvail, r.memOK = 0, 0, true
	r.diskTotal, r.diskUse, r.diskOK = 0, 0, true
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
	var mu sync.Mutex
	src := func() []DeviceDrops {
		mu.Lock()
		defer mu.Unlock()
		return []DeviceDrops{{Name: nameGarden, Dropped: drops}}
	}
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
	if !rec.isActive(hostCPUKey) || !rec.isActive(streamDropsKey(nameGarden)) {
		t.Fatal("cpu and drops not both active before disable")
	}

	s := hostSettings()
	s.Enabled = false
	h.Apply(&s)
	c.advance(10 * time.Second)
	h.poll()
	if rec.resolveCount(hostCPUKey) != 1 || rec.resolveCount(streamDropsKey(nameGarden)) != 1 {
		t.Fatal("disable did not resolve every active condition")
	}
	before := r.readCount()
	pollEvery(h, c, 10*time.Second, 20)
	if r.readCount() != before {
		t.Errorf("disabled monitor read the host %d times", r.readCount()-before)
	}
	if rec.onsetCount(hostCPUKey) != 1 {
		t.Error("onset while disabled")
	}

	// Re-enable starts fresh: the full dwell is needed again.
	h.Apply(new(hostSettings()))
	c.advance(10 * time.Second)
	h.poll()
	if rec.isActive(hostCPUKey) {
		t.Fatal("re-enable onset immediately, want a fresh dwell")
	}
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostCPUKey) || rec.onsetCount(hostCPUKey) != 2 {
		t.Error("cpu did not re-onset after re-enable and a full dwell")
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

func TestRunHostStopsOnCancel(t *testing.T) {
	s := hostSettings()
	ctx, cancel := context.WithCancel(context.Background())
	h := RunHost(ctx, &fakeHost{}, nil, newRecPub(), &s)
	if h == nil {
		t.Fatal("RunHost returned nil")
	}
	cancel()
}

func TestGroupApplyFansOut(t *testing.T) {
	a, b := &recMonitors{}, &recMonitors{}
	g := Group{a, nil, b}
	s := hostSettings()
	g.Apply(&s)
	if a.n != 1 || b.n != 1 {
		t.Errorf("Group.Apply calls = %d, %d; want 1, 1", a.n, b.n)
	}
}

type recMonitors struct{ n int }

func (r *recMonitors) Apply(*Settings) { r.n++ }
