//go:build linux

package main

import (
	"log"

	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
)

// hostReader adapts the sysinfo readers to the host monitor's HostReader. The
// interface lives in the monitor package because sysinfo imports the management
// server; this adapter is the only place the two meet.
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

// dropCounters returns every published device's cumulative dropped-frame count
// for the host monitor. A restarted device gets a fresh runtime whose counter
// starts at zero; the monitor rebaselines on a counter that goes backwards.
func (p *provider) dropCounters() []monitor.DeviceDrops {
	recs := p.deviceList()
	out := make([]monitor.DeviceDrops, 0, len(recs))
	for _, rt := range recs {
		out = append(out, monitor.DeviceDrops{Name: rt.dev.Name, Dropped: rt.dropped.Load()})
	}
	return out
}

// logUndervoltageSupport logs once at startup when the host exposes no readable
// rpi_volt hwmon, so an operator knows undervoltage alerts are unavailable on this
// host rather than silently absent. It never raises a notification.
func logUndervoltageSupport() {
	if _, ok := sysinfo.ReadUndervoltage(); !ok {
		log.Print("undervoltage monitoring unavailable: no readable rpi_volt hwmon on this host")
	}
}
