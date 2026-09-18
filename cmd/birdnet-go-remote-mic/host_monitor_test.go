//go:build linux

package main

import (
	"context"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
)

// TestBuildMonitorsWiresSignalAndHost pins that run()'s monitor group carries both
// condition monitors: dropping the host monitor (or the signal monitor) from the
// group would otherwise still compile and pass CI, silently disabling host-health
// notifications. It asserts the group built the way run() builds it holds one of
// each.
func TestBuildMonitorsWiresSignalAndHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cfg config.Config
	cfg.ApplyDefaults()
	s := monitor.SettingsFrom(&cfg)
	g := buildMonitors(ctx, levels.NewHub(), t.TempDir(), func() []monitor.DeviceDrops { return nil }, nil, &s)
	cancel() // stop the goroutines the monitor constructors started
	var hasSignal, hasHost bool
	for _, m := range g {
		switch m.(type) {
		case *monitor.Signal:
			hasSignal = true
		case *monitor.Host:
			hasHost = true
		}
	}
	if !hasSignal {
		t.Error("buildMonitors did not wire the signal monitor into the group")
	}
	if !hasHost {
		t.Error("buildMonitors did not wire the host monitor into the group")
	}
}

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
	r := hostReader{cpu: sysinfo.NewHostCPU(), dataPath: dir}

	// CPU with a nil HostCPU is unavailable, not a panic (the zero-value adapter
	// path); the wired reader below carries the delegation proof.
	if _, ok := (hostReader{}).CPU(); ok {
		t.Error("CPU ok with a nil HostCPU, want false")
	}

	// Each method must delegate to its sysinfo counterpart, so a swapped wiring is
	// caught rather than passing a shape-only check. Compare against the underlying
	// reader called on the same input: totals (disk, mem) are stable across the two
	// back-to-back calls, while availability and temperature can drift, so only the
	// stable figures and the ok flags are pinned.
	adTotal, _, _, adOK := r.Disk()
	siTotal, _, _, siOK := sysinfo.DiskUsageDetail(dir)
	if adOK != siOK || adTotal != siTotal {
		t.Errorf("Disk adapter = (%d, %v), sysinfo.DiskUsageDetail = (%d, %v); adapter must delegate", adTotal, adOK, siTotal, siOK)
	}
	mTotal, _, mOK := r.Mem()
	wTotal, _, wOK := sysinfo.ReadMem()
	if mOK != wOK || mTotal != wTotal {
		t.Errorf("Mem adapter = (%d, %v), sysinfo.ReadMem = (%d, %v); adapter must delegate", mTotal, mOK, wTotal, wOK)
	}
	// Temperature and undervoltage are best-effort sensors with no stable value to
	// compare between two separate samples: a transient cached-read reprobe or an
	// undervoltage transition could legitimately differ the two readings even though
	// the adapter delegates correctly. So assert only that the adapter's availability
	// matches its sysinfo reader (a sensor does not appear or vanish within the test);
	// the stable Disk and Mem totals above carry the swapped-wiring proof.
	_, adTempOK := r.Temp()
	_, siTempOK := sysinfo.ReadTemp()
	if adTempOK != siTempOK {
		t.Errorf("Temp adapter ok=%v, sysinfo.ReadTemp ok=%v; adapter must delegate", adTempOK, siTempOK)
	}
	_, adUVOK := r.Undervoltage()
	_, siUVOK := sysinfo.ReadUndervoltage()
	if adUVOK != siUVOK {
		t.Errorf("Undervoltage adapter ok=%v, sysinfo.ReadUndervoltage ok=%v; adapter must delegate", adUVOK, siUVOK)
	}

	// A zero-value adapter (no data path) reports disk unavailable, not a panic.
	if _, _, _, ok := (hostReader{}).Disk(); ok {
		t.Error("Disk with no data path ok, want false")
	}
}
