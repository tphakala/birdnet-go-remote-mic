//go:build linux

package main

import (
	"log"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
)

// hostReader adapts the sysinfo readers to the host monitor's HostReader. The
// interface lives in the monitor package so monitor does not depend on sysinfo,
// which pulls in the management server; that is a deliberate split to keep the
// dependency out of monitor, not a compiler-forced cycle. This adapter is the only
// place the two meet.
type hostReader struct {
	sampler  *sysinfo.Sampler
	dataPath string
}

var _ monitor.HostReader = hostReader{}

func (r hostReader) Mem() (total, avail int64, ok bool) { return sysinfo.ReadMem() }
func (r hostReader) Temp() (float64, bool)              { return sysinfo.ReadTemp() }
func (r hostReader) CPU() (float64, bool)               { return r.sampler.Percent() }
func (r hostReader) Undervoltage() (now, ok bool)       { return sysinfo.ReadUndervoltage() }

func (r hostReader) Disk() (total, used int64, ok bool) {
	return sysinfo.DiskUsage(r.dataPath)
}

// dropCounters returns the cumulative dropped-frame count of every SERVING device
// for the host monitor, tagged with the runtime's Gen so the monitor can tell a
// restarted runtime from its predecessor. A device that is disabled, skipped, or
// failed is omitted, so the monitor sees it go absent and resolves any active drops
// condition after the presence grace ("device stopped") rather than clearing it
// with a misleading "client keeping up" message. A restarted device gets a fresh
// runtime with a new Gen and a counter that starts at zero; the monitor rebaselines
// when the Gen changes, falling back to a counter that goes backwards.
func (p *provider) dropCounters() []monitor.DeviceDrops {
	recs := p.deviceList()
	out := make([]monitor.DeviceDrops, 0, len(recs))
	for _, rt := range recs {
		if rt.currentState() != mgmtserver.StateServing {
			continue
		}
		out = append(out, monitor.DeviceDrops{Name: rt.dev.Name, Gen: rt.gen, Dropped: rt.dropped.Load()})
	}
	return out
}

// logUndervoltageSupport logs once at startup when the host exposes no readable
// rpi_volt hwmon, so an operator knows undervoltage alerts are unavailable on this
// host rather than silently absent. It never raises a notification.
func logUndervoltageSupport() {
	if _, ok := sysinfo.ReadUndervoltage(); !ok {
		log.Print("undervoltage alerts unavailable: no readable rpi_volt hwmon detected at startup")
	}
}
