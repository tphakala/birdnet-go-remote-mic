//go:build linux

package main

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
)

func TestProviderDropCounters(t *testing.T) {
	p := &provider{}
	if got := p.dropCounters(); len(got) != 0 {
		t.Fatalf("dropCounters before setDevices = %v, want empty", got)
	}
	// Distinct gens on the serving runtimes so the test pins that dropCounters
	// carries rt.gen into DeviceDrops.Gen (a regression hardcoding Gen 0 would fail).
	a := &deviceRuntime{dev: config.Device{Name: "orchard"}, gen: 5, state: mgmtserver.StateServing}
	a.dropped.Store(42)
	b := &deviceRuntime{dev: config.Device{Name: "bats"}, gen: 8, state: mgmtserver.StateServing}
	// A disabled and a failed device carry (possibly frozen) counters but must be
	// excluded, so the monitor sees them go absent and resolves any active drops
	// condition rather than clearing it with a misleading "client keeping up".
	disabled := &deviceRuntime{dev: config.Device{Name: "attic"}, state: mgmtserver.StateDisabled}
	disabled.dropped.Store(99)
	failed := &deviceRuntime{dev: config.Device{Name: "cellar"}, state: mgmtserver.StateFailed}
	failed.dropped.Store(7)
	p.setDevices([]*deviceRuntime{a, disabled, b, failed})
	got := p.dropCounters()
	if len(got) != 2 ||
		got[0].Name != "orchard" || got[0].Gen != 5 || got[0].Dropped != 42 ||
		got[1].Name != "bats" || got[1].Gen != 8 || got[1].Dropped != 0 {
		t.Errorf("dropCounters = %+v, want only the two serving devices with their gens", got)
	}
}

func TestHostReaderAdapter(t *testing.T) {
	dir := t.TempDir()
	r := hostReader{dataPath: dir}

	// CPU with a nil sampler is unavailable, not a panic.
	if _, ok := r.CPU(); ok {
		t.Error("CPU ok with a nil sampler, want false")
	}

	// Each method must delegate to its sysinfo counterpart, so a swapped wiring is
	// caught rather than passing a shape-only check. Compare against the underlying
	// reader called on the same input: totals (disk, mem) are stable across the two
	// back-to-back calls, while availability and temperature can drift, so only the
	// stable figures and the ok flags are pinned.
	adTotal, _, adOK := r.Disk()
	siTotal, _, siOK := sysinfo.DiskUsage(dir)
	if adOK != siOK || adTotal != siTotal {
		t.Errorf("Disk adapter = (%d, %v), sysinfo.DiskUsage = (%d, %v); adapter must delegate", adTotal, adOK, siTotal, siOK)
	}
	mTotal, _, mOK := r.Mem()
	wTotal, _, wOK := sysinfo.ReadMem()
	if mOK != wOK || mTotal != wTotal {
		t.Errorf("Mem adapter = (%d, %v), sysinfo.ReadMem = (%d, %v); adapter must delegate", mTotal, mOK, wTotal, wOK)
	}
	if _, adTempOK := r.Temp(); adTempOK != secondOK(sysinfo.ReadTemp()) {
		t.Errorf("Temp adapter ok=%v, sysinfo.ReadTemp ok=%v; adapter must delegate", adTempOK, secondOK(sysinfo.ReadTemp()))
	}
	adUV, adUVOK := r.Undervoltage()
	siUV, siUVOK := sysinfo.ReadUndervoltage()
	if adUV != siUV || adUVOK != siUVOK {
		t.Errorf("Undervoltage adapter = (%v, %v), sysinfo.ReadUndervoltage = (%v, %v); adapter must delegate", adUV, adUVOK, siUV, siUVOK)
	}

	// A zero-value adapter (no data path) reports disk unavailable, not a panic.
	if _, _, ok := (hostReader{}).Disk(); ok {
		t.Error("Disk with no data path ok, want false")
	}
}

// secondOK returns the ok flag of a (value, ok) reader result, so the adapter's
// temperature availability can be compared without pinning the drifting value.
func secondOK(_ float64, ok bool) bool { return ok }
