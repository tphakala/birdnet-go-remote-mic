package monitor

import (
	"strings"
	"testing"
	"time"
)

// TestHostDiskUsesUsablePercent pins that the disk condition judges used/(used+avail)
// (df's Use%), not used/total: on a filesystem with root-reserved blocks the raw
// used/total can sit below the onset threshold while the space the unprivileged
// service can actually write is already over it.
func TestHostDiskUsesUsablePercent(t *testing.T) {
	const gib = int64(1) << 30
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c) // DiskPercent 90, clear 85

	r.mu.Lock()
	// 88% of raw size (below the 90 onset) but 88/(88+2) = 97.8% usable (over it):
	// 10 GiB of the 100 GiB is root-reserved, invisible to the service.
	r.diskTotal, r.diskUse, r.diskAvail, r.diskOK = 100*gib, 88*gib, 2*gib, true
	r.mu.Unlock()
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if rec.isActive(hostDiskKey) {
		t.Fatal("disk onset before the 60 s dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if !rec.isActive(hostDiskKey) {
		t.Fatal("disk did not onset on usable percentage; it judged used/total (88%) instead of used/(used+avail)")
	}
	if msg := rec.onsetMessage(hostDiskKey); !strings.Contains(msg, "98% full") {
		t.Errorf("disk onset message = %q, want it to report the usable ~98%%", msg)
	}
}

// TestHostDiskAllReservedTreatedUnavailable pins the used+avail>0 guard on the
// active side: a filesystem reporting total>0 but used==0 and avail==0 (every
// block root-reserved, nothing written) has no meaningful usable percentage, so
// it is treated as an unavailable reading, an active alert is held and then
// resolved after the clear dwell, rather than dividing 0/0 into a NaN that
// compares false and silently CLEARS the alert. The discriminator is the final
// assertion: with the guard the dwell ends in a resolve, without it in a clear.
func TestHostDiskAllReservedTreatedUnavailable(t *testing.T) {
	const gib = int64(1) << 30
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)

	// Raise the disk alert at 95% usable.
	r.mu.Lock()
	r.diskTotal, r.diskUse, r.diskAvail, r.diskOK = 100*gib, 95*gib, 5*gib, true
	r.mu.Unlock()
	h.poll()
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostDiskKey) {
		t.Fatal("disk not active after the onset dwell")
	}

	// All-reserved reading (used+avail==0, total>0): unavailable, not a clear.
	r.mu.Lock()
	r.diskUse, r.diskAvail = 0, 0
	r.mu.Unlock()
	pollEvery(h, c, 10*time.Second, 5) // 50 s, short of the 60 s clear/resolve dwell
	if !rec.isActive(hostDiskKey) {
		t.Fatal("an all-reserved reading ended the active disk alert early")
	}
	// Unavailable for the whole clear dwell resolves as sensor-unavailable; without
	// the used+avail>0 guard the NaN usedPct would instead complete a spurious clear.
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostDiskKey) || rec.resolveCount(hostDiskKey) != 1 || rec.clearCount(hostDiskKey) != 0 {
		t.Fatalf("all-reserved disk not resolved as unavailable (resolves=%d clears=%d); the used+avail>0 guard is missing", rec.resolveCount(hostDiskKey), rec.clearCount(hostDiskKey))
	}
}

// TestHostMemFreeMiBClampedToHalf pins that a mem_free_mib set above (or near) the
// host's physical RAM cannot pin a permanent, never-clearing low-memory warning:
// the MiB floor is clamped to half of RAM, so the warning both onsets when memory
// is genuinely low and clears when it recovers.
func TestHostMemFreeMiBClampedToHalf(t *testing.T) {
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	s := hostSettings()
	s.Host.MemFreeMiB = p(4096) // 4 GiB floor on a 512 MiB host: clamps to 256 MiB
	h := newHostT(r, nil, rec, s, c)

	// Genuinely low (16 MiB free on a 512 MiB host) onsets.
	r.mu.Lock()
	r.memTotal, r.memAvail, r.memOK = 512<<20, 16<<20, true
	r.mu.Unlock()
	h.poll()
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostMemKey) {
		t.Fatal("low-memory did not onset with 16 MiB free")
	}

	// Recover to 400 MiB free. Without the clamp the limit would be 4 GiB (> total),
	// so available memory could never reach it and the warning would be permanent;
	// with the clamp (limit 256 MiB, clear level 320 MiB) 400 MiB clears it.
	r.mu.Lock()
	r.memAvail = 400 << 20
	r.mu.Unlock()
	h.poll()
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostMemKey) {
		t.Fatal("cleared before the 60 s clear dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostMemKey) || rec.clearCount(hostMemKey) != 1 {
		t.Fatalf("low-memory did not clear after recovery (clears=%d); the mem_free_mib clamp is missing", rec.clearCount(hostMemKey))
	}
}

// TestHostClearRunRestartsAfterReadingGap pins the clear-run restart: while a
// condition is active, a gap in the readings (sensor briefly unavailable) abandons
// the pending clear run, so the clear needs a contiguous stretch of under readings
// after the gap rather than completing from two under readings a whole clear dwell
// apart with the sensor dark between them.
func TestHostClearRunRestartsAfterReadingGap(t *testing.T) {
	r := &fakeHost{}
	rec := newRecPub()
	c := newClk()
	h := newHostT(r, nil, rec, hostSettings(), c)

	// Raise CPU.
	r.setCPU(99)
	h.poll()
	pollEvery(h, c, 10*time.Second, 6)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu not active")
	}

	// One under-clear reading begins the clear run.
	r.setCPU(70)
	h.poll()
	// The sensor is unavailable for 50 s (5 polls, short of the 60 s clear dwell).
	r.mu.Lock()
	r.cpuOK = false
	r.mu.Unlock()
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostCPUKey) || rec.clearCount(hostCPUKey) != 0 || rec.resolveCount(hostCPUKey) != 0 {
		t.Fatal("the reading gap cleared or resolved the condition early")
	}
	// One more under reading right after the gap. Without the clear-run restart this
	// reading sits 60 s after the first and would complete the clear; with it, the
	// run restarted, so this is only the first reading of a fresh clear run.
	r.setCPU(70)
	pollEvery(h, c, 10*time.Second, 1)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu cleared from two under readings spanning a reading gap; the clear run did not restart")
	}
	// A fresh contiguous 60 s of under readings clears.
	pollEvery(h, c, 10*time.Second, 5)
	if !rec.isActive(hostCPUKey) {
		t.Fatal("cpu cleared before a fresh 60 s clear dwell")
	}
	pollEvery(h, c, 10*time.Second, 1)
	if rec.isActive(hostCPUKey) || rec.clearCount(hostCPUKey) != 1 {
		t.Fatalf("cpu did not clear after a fresh 60 s under-run (clears=%d)", rec.clearCount(hostCPUKey))
	}
}

// TestHostMessagesAreAsserted pins the human-readable onset/clear text the round-2
// review flagged as unasserted: the CPU onset names its threshold and dwell, and
// the undervoltage clear makes no exact-duration claim (the 10 s poll of a ~2 s
// sticky alarm cannot honestly promise a continuous undervoltage-free minute).
func TestHostMessagesAreAsserted(t *testing.T) {
	t.Run("cpu onset names the threshold and dwell", func(t *testing.T) {
		t.Parallel()
		r := &fakeHost{}
		rec := newRecPub()
		c := newClk()
		h := newHostT(r, nil, rec, hostSettings(), c)
		r.setCPU(95)
		h.poll()
		pollEvery(h, c, 10*time.Second, 6)
		if msg := rec.onsetMessage(hostCPUKey); !strings.Contains(msg, "90%") || !strings.Contains(msg, "1 minute") {
			t.Errorf("cpu onset message = %q, want it to name the 90%% threshold and the 1 minute dwell", msg)
		}
	})
	t.Run("undervoltage clear makes no exact-duration claim", func(t *testing.T) {
		t.Parallel()
		r := &fakeHost{}
		rec := newRecPub()
		c := newClk()
		h := newHostT(r, nil, rec, hostSettings(), c)
		r.mu.Lock()
		r.volt, r.voltOK = true, true
		r.mu.Unlock()
		h.poll()
		pollEvery(h, c, 10*time.Second, 2)
		if !rec.isActive(hostVoltKey) {
			t.Fatal("undervoltage not active")
		}
		r.mu.Lock()
		r.volt = false
		r.mu.Unlock()
		h.poll()
		pollEvery(h, c, 10*time.Second, 6)
		if rec.clearCount(hostVoltKey) != 1 {
			t.Fatalf("undervoltage did not clear (clears=%d)", rec.clearCount(hostVoltKey))
		}
		msg := rec.clearMessage(hostVoltKey)
		if strings.Contains(msg, "1 minute") || strings.Contains(msg, "for 1") {
			t.Errorf("undervoltage clear message = %q still overclaims an exact duration", msg)
		}
		if !strings.Contains(msg, "No undervoltage detected") {
			t.Errorf("undervoltage clear message = %q, want it to say no undervoltage detected", msg)
		}
	})
}
