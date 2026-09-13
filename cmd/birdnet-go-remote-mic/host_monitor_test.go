//go:build linux

package main

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

func TestProviderDropCounters(t *testing.T) {
	p := &provider{}
	if got := p.dropCounters(); len(got) != 0 {
		t.Fatalf("dropCounters before setDevices = %v, want empty", got)
	}
	a := &deviceRuntime{dev: config.Device{Name: "orchard"}, state: mgmtserver.StateServing}
	a.dropped.Store(42)
	b := &deviceRuntime{dev: config.Device{Name: "bats"}, state: mgmtserver.StateServing}
	// A disabled and a failed device carry (possibly frozen) counters but must be
	// excluded, so the monitor sees them go absent and resolves any active drops
	// condition rather than clearing it with a misleading "client keeping up".
	disabled := &deviceRuntime{dev: config.Device{Name: "attic"}, state: mgmtserver.StateDisabled}
	disabled.dropped.Store(99)
	failed := &deviceRuntime{dev: config.Device{Name: "cellar"}, state: mgmtserver.StateFailed}
	failed.dropped.Store(7)
	p.setDevices([]*deviceRuntime{a, disabled, b, failed})
	got := p.dropCounters()
	if len(got) != 2 || got[0].Name != "orchard" || got[0].Dropped != 42 || got[1].Name != "bats" || got[1].Dropped != 0 {
		t.Errorf("dropCounters = %+v, want only the two serving devices", got)
	}
}

func TestHostReaderAdapter(t *testing.T) {
	r := hostReader{dataPath: t.TempDir()}
	if _, ok := r.CPU(); ok {
		t.Error("CPU ok with a nil sampler, want false")
	}
	if total, _, ok := r.Disk(); !ok || total <= 0 {
		t.Errorf("Disk on a temp dir = (%d, %v), want a positive total", total, ok)
	}
	if total, avail, ok := r.Mem(); !ok || total <= 0 || avail > total {
		t.Errorf("Mem = (%d, %d, %v)", total, avail, ok)
	}
	// Temperature and undervoltage depend on the host's sensors; they must only
	// not panic here.
	r.Temp()
	r.Undervoltage()
	if _, _, ok := (hostReader{}).Disk(); ok {
		t.Error("Disk with no data path ok, want false")
	}
}
