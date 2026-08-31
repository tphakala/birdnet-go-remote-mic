//go:build linux

package main

import (
	"context"
	"log"
	"runtime"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/reload"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
)

// pumpBacklog bounds the pumpDone channel. At most one result arrives per live
// pump; the device list is capped at 32. During a reconcile only the stopped
// (superseded) pumps end, but a shutdown that lands mid restart-everything can
// end up to 32 old plus 32 new live pumps at once, so 64 covers the worst case.
// If it were ever exceeded a pump would block on send, not drop, so the bound is
// a smoothing buffer rather than a correctness limit.
const pumpBacklog = 64

// pumpResult reports a device's capture pump goroutine ending, with the error
// that stopped it (nil on a clean EOF or a deliberate stop).
type pumpResult struct {
	rt  *deviceRuntime
	err error
}

// reconcileReq asks the run loop to apply cfg to the running pipeline. The loop
// replies on reply (buffered, so the loop never blocks) once the change is live.
type reconcileReq struct {
	cfg   config.Config
	reply chan error
}

// appliance owns the running capture pipeline and reconciles it to configuration
// changes at runtime, so a settings change starts, stops, or restarts only the
// affected devices and leaves every other device and its RTSP client untouched.
//
// Every method runs on the single run-loop goroutine and is never called
// concurrently, so the maps and counters need no locks. The capture pumps run on
// their own goroutines but touch only their own deviceRuntime (its frame source
// and drop counter), never the appliance's shared state.
type appliance struct {
	ctx  context.Context
	hub  *levels.Hub
	srv  *rtspserver.Server
	prov *provider

	// cfg is the configuration currently applied to the pipeline. devices holds
	// one runtime per configured device keyed by name, in any state (serving,
	// skipped, disabled, or failed). hwNames maps device id to its sound-card
	// label, refreshed on each reconcile.
	cfg     config.Config
	hwNames map[string]string
	devices map[string]*deviceRuntime

	pumpDone chan pumpResult

	alive          int   // number of live capture pumps
	lastPumpErr    error // last spontaneous pump failure, for the exit status
	announceCancel context.CancelFunc

	// open builds and starts one device's runtime. It is a field so tests can
	// inject a fake capture source instead of opening real ALSA hardware; in
	// production it is openDeviceRetry.
	open func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error)
}

func newAppliance(ctx context.Context, hub *levels.Hub, srv *rtspserver.Server, prov *provider) *appliance {
	return &appliance{
		ctx:      ctx,
		hub:      hub,
		srv:      srv,
		prov:     prov,
		hwNames:  map[string]string{},
		devices:  map[string]*deviceRuntime{},
		pumpDone: make(chan pumpResult, pumpBacklog),
		open:     openDeviceRetry,
	}
}

// serving reports how many devices currently have a live pump.
func (a *appliance) serving() int {
	n := 0
	for _, rt := range a.devices {
		if rt.currentState() == mgmtserver.StateServing {
			n++
		}
	}
	return n
}

// allDisabled reports whether every configured device is disabled, distinguishing
// a deliberate all-off config from a genuine open failure when nothing serves.
func (a *appliance) allDisabled() bool {
	if len(a.devices) == 0 {
		return false
	}
	for _, rt := range a.devices {
		if rt.currentState() != mgmtserver.StateDisabled {
			return false
		}
	}
	return true
}

// runningParams snapshots the parameters each serving device is running with, for
// the reconcile planner to diff against the desired config.
func (a *appliance) runningParams() map[string]config.Device {
	m := make(map[string]config.Device)
	for name, rt := range a.devices {
		if rt.currentState() == mgmtserver.StateServing {
			m[name] = rt.dev
		}
	}
	return m
}

// pump runs one device's capture-to-RTP loop until its source ends (a clean stop
// or a failure), then reports the result. Each pump is locked to its OS thread so
// the capture read loop is not descheduled mid-period.
func (a *appliance) pump(rt *deviceRuntime) {
	runtime.LockOSThread()
	perr := rt.stage.Run(rt.src, func(f pipeline.Frame) error {
		if !rt.frames.Push(f) {
			drops := rt.dropped.Add(1)
			if drops%50 == 1 {
				log.Printf("%s: dropping frames: the client is not keeping up (total drops: %d)", rt.dev.Name, drops)
			}
		}
		return a.ctx.Err()
	})
	a.pumpDone <- pumpResult{rt: rt, err: perr}
}

// openAndStart opens a device, wires its RTSP track and level meter, and starts
// its pump. A device that fails to open is not fatal: it returns a skipped record
// carrying the open error so GET /devices can report it, exactly as at startup.
func (a *appliance) openAndStart(dev *config.Device) *deviceRuntime {
	friendly := a.hwNames[dev.Device]
	// Probe supported rates for the config UI before opening: once we hold the
	// hw device exclusively the probe would see our own process and report busy.
	rates := audio.ProbeRates(dev.Device, dev.Channels, audio.CandidateRates())

	d := *dev
	rt, err := a.open(&d, a.hub)
	if err != nil {
		log.Printf("skipping device %q (%s): %v", dev.Name, dev.Device, err)
		return &deviceRuntime{
			dev:            *dev,
			state:          mgmtserver.StateSkipped,
			err:            err.Error(),
			friendlyName:   friendly,
			supportedRates: rates,
		}
	}
	rt.state = mgmtserver.StateServing
	rt.friendlyName = friendly
	rt.supportedRates = rates
	a.srv.AddTrack(rt.track)
	a.alive++
	go a.pump(rt)
	log.Printf("capture %q: %d Hz, %d ch on %s serving %s", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, rt.dev.Path)
	return rt
}

// stop tears down a serving device the reconcile deliberately removed or is about
// to restart: it retires the RTSP track and level meter and closes the capture
// source, which ends the pump. It marks the runtime superseded so the pump's
// final pumpResult skips the spontaneous-death cleanup. The pump's exit adjusts
// the alive count, so stop does not.
func (a *appliance) stop(rt *deviceRuntime) {
	rt.superseded = true
	a.srv.RemoveTrack(rt.track.Path)
	a.hub.RemoveMeter(rt.dev.Name)
	_ = rt.src.Close()
	rt.frames.Close()
}

// reconcile applies newCfg to the running pipeline: it starts newly enabled or
// added devices, stops removed or disabled ones, and restarts those whose capture
// parameters changed, while leaving unchanged devices serving. It then republishes
// the device records and, if the serving set or discovery flag changed, restarts
// the mDNS advertisement.
func (a *appliance) reconcile(newCfg *config.Config) {
	if names, err := audio.HardwareNames(); err == nil {
		a.hwNames = names
	} else {
		log.Printf("enumerate capture hardware: %v (devices carry no friendly label)", err)
	}

	prevDiscovery := a.prov.discoveryEnabled()

	plan := reload.Reconcile(a.runningParams(), newCfg)

	for _, name := range plan.Stop {
		if rt, ok := a.devices[name]; ok && rt.currentState() == mgmtserver.StateServing {
			a.stop(rt)
		}
	}
	// Restart in two passes: stop every restarting device before starting any,
	// so two devices that swap hardware cards can both reopen. ALSA is
	// single-client, and interleaving stop and start would try to open one
	// device's new card while the other still held it (EBUSY).
	for i := range plan.Restart {
		if rt, ok := a.devices[plan.Restart[i].Name]; ok && rt.currentState() == mgmtserver.StateServing {
			a.stop(rt)
		}
	}
	for i := range plan.Restart {
		a.devices[plan.Restart[i].Name] = a.openAndStart(&plan.Restart[i])
	}
	for i := range plan.Start {
		a.devices[plan.Start[i].Name] = a.openAndStart(&plan.Start[i])
	}

	a.reconcileRecords(newCfg)

	a.cfg = *newCfg
	a.prov.setDiscovery(newCfg.DiscoveryEnabled())
	a.publish(newCfg)

	// Rebuild the mDNS advertisement whenever any device changed or discovery
	// toggled. A param-change restart keeps the serving count identical but
	// alters the advertised path/rate/codec, so gate on the plan being non-empty,
	// not on the count. dnssd cannot retire a single service, so restartAnnounce
	// rebuilds the whole set.
	if !plan.Empty() || newCfg.DiscoveryEnabled() != prevDiscovery {
		a.restartAnnounce()
	}
}

// reconcileRecords makes the device records match newCfg after the plan ran:
// disabled devices keep a visible (unopened) record, and devices removed from the
// config are dropped. Enabled devices are already handled by start/restart, and
// unchanged ones are left in place.
func (a *appliance) reconcileRecords(newCfg *config.Config) {
	want := make(map[string]bool, len(newCfg.Devices))
	for i := range newCfg.Devices {
		d := newCfg.Devices[i]
		want[d.Name] = true
		if d.IsEnabled() {
			continue
		}
		// A disabled device is visible but never opened. Always publish a FRESH
		// record rather than mutating an existing one in place: an existing record
		// may already be published to the provider, whose HTTP handlers read
		// deviceRuntime.dev and friendlyName without a lock, so mutating those
		// fields here would race a concurrent GET /devices.
		a.devices[d.Name] = &deviceRuntime{dev: d, state: mgmtserver.StateDisabled, friendlyName: a.hwNames[d.Device]}
	}
	for name, rt := range a.devices {
		if want[name] {
			continue
		}
		// A serving record the plan did not stop is a plan bug; stop it as a
		// safety net. A device the plan already stopped is marked superseded, so
		// skip it here to avoid a redundant second teardown.
		if rt.currentState() == mgmtserver.StateServing && !rt.superseded {
			a.stop(rt)
		}
		delete(a.devices, name)
	}
}

// publish snapshots the device records in config order and hands them to the
// provider for the management API.
func (a *appliance) publish(cfg *config.Config) {
	recs := make([]*deviceRuntime, 0, len(cfg.Devices))
	for i := range cfg.Devices {
		if rt, ok := a.devices[cfg.Devices[i].Name]; ok {
			recs = append(recs, rt)
		}
	}
	a.prov.setDevices(recs)
}

// onPumpDone handles a pump ending. A pump the reconcile stopped (superseded) only
// adjusts the alive count; its teardown already happened. A pump that stopped on
// its own is a device that died after startup: its track and meter are retired,
// its record marked failed, and it keeps returning 404 until a reload restarts it.
func (a *appliance) onPumpDone(res pumpResult) {
	a.alive--
	if res.rt.superseded {
		return
	}
	a.srv.RemoveTrack(res.rt.track.Path)
	res.rt.frames.Close()
	_ = res.rt.src.Close()
	a.hub.RemoveMeter(res.rt.dev.Name)
	if res.err != nil && a.ctx.Err() == nil {
		a.lastPumpErr = res.err
		res.rt.markFailed(res.err)
		log.Printf("device %q failed: %v; %s returns 404 until reload", res.rt.dev.Name, res.err, res.rt.dev.Path)
	}
	a.publish(&a.cfg)
}

// restartAnnounce cancels the current mDNS advertisement and starts a fresh one
// for the serving set. dnssd cannot retire a single service, so the whole
// advertisement is rebuilt whenever the serving set or discovery flag changes.
func (a *appliance) restartAnnounce() {
	if a.announceCancel != nil {
		a.announceCancel()
		a.announceCancel = nil
	}
	if !a.prov.discoveryEnabled() {
		return
	}
	serving := make([]*deviceRuntime, 0, len(a.devices))
	for i := range a.cfg.Devices {
		if rt, ok := a.devices[a.cfg.Devices[i].Name]; ok && rt.currentState() == mgmtserver.StateServing {
			serving = append(serving, rt)
		}
	}
	if len(serving) == 0 {
		return
	}
	actx, cancel := context.WithCancel(a.ctx)
	a.announceCancel = cancel
	startAnnounce(actx, a.cfg.Listen, serving)
}

// closeAll releases every serving device's capture source at shutdown so the
// ALSA hardware is freed promptly rather than at process exit.
func (a *appliance) closeAll() {
	for _, rt := range a.devices {
		if rt.currentState() == mgmtserver.StateServing && rt.src != nil {
			_ = rt.src.Close()
		}
	}
}
