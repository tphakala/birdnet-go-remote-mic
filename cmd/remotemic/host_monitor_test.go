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
	g := buildMonitors(ctx, levels.NewHub(), t.TempDir(), func() []monitor.DeviceCounters { return nil }, nil, &s)
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

func TestProviderDeviceCounters(t *testing.T) {
	p := &provider{}
	if got := p.deviceCounters(); len(got) != 0 {
		t.Fatalf("deviceCounters before setDevices = %v, want empty", got)
	}
	// Distinct gens on the serving runtimes so the test pins that deviceCounters
	// carries rt.gen into DeviceCounters.Gen (a regression hardcoding Gen 0 would
	// fail). The device-level dropped figure is the sum of its streams' counters
	// (droppedTotal), so each runtime carries one stream holding the drops; the
	// overrun figure comes from the device's capture source.
	a := &deviceRuntime{dev: config.Device{Name: "orchard"}, gen: 5, state: mgmtserver.StateServing, streams: []*streamRuntime{{}}, src: newOverrunCapture(3)}
	a.streams[0].dropped.Store(42)
	b := &deviceRuntime{dev: config.Device{Name: "bats"}, gen: 8, state: mgmtserver.StateServing, streams: []*streamRuntime{{}}}
	// A disabled and a failed device carry (possibly frozen) counters but must be
	// excluded, so the monitor sees them go absent and resolves any active drops
	// condition rather than clearing it with a misleading "client keeping up".
	disabled := &deviceRuntime{dev: config.Device{Name: "attic"}, state: mgmtserver.StateDisabled, streams: []*streamRuntime{{}}}
	disabled.streams[0].dropped.Store(99)
	failed := &deviceRuntime{dev: config.Device{Name: "cellar"}, state: mgmtserver.StateFailed, streams: []*streamRuntime{{}}}
	failed.streams[0].dropped.Store(7)
	p.setDevices([]*deviceRuntime{a, disabled, b, failed})
	got := p.deviceCounters()
	if len(got) != 2 ||
		got[0].Name != "orchard" || got[0].Gen != 5 || got[0].Dropped != 42 || got[0].Overruns != 3 ||
		got[1].Name != "bats" || got[1].Gen != 8 || got[1].Dropped != 0 || got[1].Overruns != 0 {
		t.Errorf("deviceCounters = %+v, want only the two serving devices with their gens and counters", got)
	}
}

func TestDeviceRuntimeAggregatesStreamDrops(t *testing.T) {
	// A device's droppedTotal sums its streams' counters, and status() reports that
	// sum plus each stream's own figure. Exercises the 2+ stream path that the
	// single-stream fixtures elsewhere do not.
	rt := &deviceRuntime{
		dev:      config.Device{Name: "iface", Streams: []config.Stream{{Path: "/a"}, {Path: "/b"}}},
		state:    mgmtserver.StateServing,
		rate:     48000,
		channels: 2,
		streams: []*streamRuntime{
			{stream: config.Stream{Path: "/a"}},
			{stream: config.Stream{Path: "/b"}},
		},
	}
	rt.streams[0].dropped.Store(5)
	rt.streams[1].dropped.Store(2)

	if got := rt.droppedTotal(); got != 7 {
		t.Errorf("droppedTotal() = %d, want 7 (5+2)", got)
	}
	ds := rt.status()
	if ds.DroppedFrames != 7 {
		t.Errorf("status DroppedFrames = %d, want 7", ds.DroppedFrames)
	}
	if len(ds.Streams) != 2 {
		t.Fatalf("status Streams = %d, want 2", len(ds.Streams))
	}
	if ds.Streams[0].Path != "/a" || ds.Streams[0].DroppedFrames != 5 {
		t.Errorf("status stream[0] = %+v, want /a drops=5", ds.Streams[0])
	}
	if ds.Streams[1].Path != "/b" || ds.Streams[1].DroppedFrames != 2 {
		t.Errorf("status stream[1] = %+v, want /b drops=2", ds.Streams[1])
	}
}

func TestDeviceRuntimeStatusReportsOverruns(t *testing.T) {
	t.Parallel()
	// status() reports the device capture's overrun count, and zero for a record
	// that holds no capture.
	rt := &deviceRuntime{dev: config.Device{Name: "iface"}, state: mgmtserver.StateServing}
	if got := rt.status().Overruns; got != 0 {
		t.Errorf("status Overruns with no capture source = %d, want 0", got)
	}
	rt.src = newOverrunCapture(6)
	if got := rt.status().Overruns; got != 6 {
		t.Errorf("status Overruns = %d, want the capture's 6", got)
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
