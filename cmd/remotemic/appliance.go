//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"slices"
	"strings"
	"sync"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
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
	// guard is the shared-token guard the RTSP and management servers consult;
	// reconcile swaps its token so a config change applies without a restart.
	guard *auth.Guard
	// notifier records device and lifecycle transitions in the notification
	// center. Onset/Clear/Resolve are idempotent and a nil *Center is a no-op, so
	// every emission site is safe to reach on every reconcile.
	notifier notify.Publisher

	// cfg is the configuration currently applied to the pipeline. devices holds
	// one runtime per configured device keyed by name, in any state (serving,
	// skipped, disabled, or failed). hw maps each configured device id to the
	// hardware it resolved to (or the resolve error), refreshed on every
	// reconcile and on every host hardware change.
	cfg     config.Config
	hw      map[string]hwResult
	devices map[string]*deviceRuntime
	// downReason holds the cause class of each device's active down condition,
	// keyed by device name, so a change of cause (not connected becoming
	// ambiguous) re-raises the condition instead of being swallowed by the
	// idempotent Onset. An entry exists exactly while the down key is active.
	downReason map[string]string
	// capsCache holds the last non-empty probed capabilities per device id. A
	// re-probe during a hot reload can transiently report nothing (the card is
	// briefly busy or gone mid card-swap); retaining the last-known-good caps keeps
	// the UI from flickering to an empty rate/channel list during that window.
	capsCache map[string]deviceCaps

	pumpDone chan pumpResult

	alive          int   // number of live capture pumps
	lastPumpErr    error // last spontaneous pump failure, for the exit status
	announceCancel context.CancelFunc
	// announceGen counts advertisement rebuilds, so a test can assert that a
	// reconcile did (or did not) rebuild the mDNS set without touching dnssd.
	announceGen int

	// open builds and starts one device's runtime, resolving the hardware open
	// channel count per attempt (see openDeviceRetry). It is a field so tests can
	// inject a fake capture source instead of opening real ALSA hardware; in
	// production it is openDeviceRetry.
	open func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error)

	// resolve maps a configured device id to the hardware it names right now,
	// without opening it. It is a field so tests can inject a host device list
	// instead of reading sysfs; in production it is audio.Resolve.
	resolve func(id string) (audio.Hardware, error)

	// monitors re-arms the condition monitors at the end of every reconcile so a
	// threshold or per-device quiet-alert change applies without a restart. run()
	// assigns a monitor.Group of the signal and host monitors; a nil monitors (a
	// test that wires none) is a no-op here, and tests may inject a recording
	// fake. Never store a typed-nil concrete: it would pass the != nil guard below
	// and call Apply on a nil receiver.
	monitors monitor.Monitors
}

func newAppliance(ctx context.Context, hub *levels.Hub, srv *rtspserver.Server, prov *provider, guard *auth.Guard, notifier notify.Publisher) *appliance {
	// A nil *notify.Center is a no-op Publisher, but an untyped-nil interface is
	// not: calling a method on it panics. Substitute a typed nil so the emission
	// sites can call the notifier unconditionally without a per-site nil check.
	if notifier == nil {
		notifier = (*notify.Center)(nil)
	}
	return &appliance{
		ctx:        ctx,
		hub:        hub,
		srv:        srv,
		prov:       prov,
		guard:      guard,
		notifier:   notifier,
		hw:         map[string]hwResult{},
		devices:    map[string]*deviceRuntime{},
		downReason: map[string]string{},
		capsCache:  map[string]deviceCaps{},
		pumpDone:   make(chan pumpResult, pumpBacklog),
		open:       openDeviceRetry,
		resolve:    audio.Resolve,
	}
}

// hwResult is what one configured device id resolved to: the hardware it names
// on this host right now, or why it names none.
type hwResult struct {
	hw  audio.Hardware
	err error
}

// Cause classes for a device's down condition. A change of class while the
// device stays down re-raises the condition (see markDown).
const (
	downNotConnected = "not-connected"
	downAmbiguous    = "ambiguous"
	downMalformed    = "malformed"
	downResolve      = "resolve-failed"
	downSameHardware = "same-hardware"
	downOpenFailed   = "open-failed"
	downDisconnected = "disconnected"
	downFailed       = "failed"
)

// markDown raises the device's down condition. Onset is idempotent per key, so
// when the device is already down for a different cause the old condition is
// resolved first and the new one raised, so the operator sees the current cause
// rather than the first one.
func (a *appliance) markDown(name, cause string, n *notify.Notification) {
	if prev, ok := a.downReason[name]; ok && prev != cause {
		a.notifier.Resolve(deviceDownKey(name), "the cause changed")
	}
	a.downReason[name] = cause
	a.notifier.Onset(*n)
}

// resolveError explains why a configured device was not opened after its id
// failed to resolve, returning the cause class and the operator-facing message.
func resolveError(dev *config.Device, err error) (cause, msg string) {
	var nf *capture.DeviceNotFoundError
	var amb *capture.AmbiguousDeviceError
	var bad *capture.BadDeviceError
	switch {
	case errors.As(err, &nf):
		return downNotConnected, fmt.Sprintf("not connected: no device matches %s", dev.Device)
	case errors.As(err, &amb):
		return downAmbiguous, fmt.Sprintf("ambiguous: %s matches %d devices (%s); bind it to one of them by port", dev.Device, len(amb.Matches), strings.Join(amb.Matches, ", "))
	case errors.As(err, &bad):
		// A malformed id ("plughw:1,0", "hw:Loopback,1", a typo) is not a card index
		// that "can change across reboots"; it is not in any accepted form and can
		// never open. Report it as malformed so the operator fixes the id rather than
		// waiting for a reboot to fix an address.
		return downMalformed, fmt.Sprintf("malformed device id %s: %v; re-add the device to bind it to real hardware", dev.Device, err)
	default:
		return downResolve, fmt.Sprintf("cannot resolve %s: %v", dev.Device, err)
	}
}

// refreshHardware resolves every configured device id against the host's
// current hardware and publishes the ids the configuration owns to the
// background enumeration. The published set holds both the configured id and
// the stable id it resolved to, so a device configured by a card index is still
// recognised as configured under the stable id the enumeration lists it by.
func (a *appliance) refreshHardware(cfg *config.Config) {
	hw := make(map[string]hwResult, len(cfg.Devices))
	ids := make(map[string]bool, 2*len(cfg.Devices))
	for i := range cfg.Devices {
		id := cfg.Devices[i].Device
		ids[id] = true
		if _, done := hw[id]; done {
			continue
		}
		h, err := a.resolve(id)
		hw[id] = hwResult{hw: h, err: err}
		if err == nil {
			// Claim both the resolved stable id and the port-form id. When two
			// identical USB units share a serial, the enumeration offers each unit
			// under its PortID (offeredIDs), so an owned unit whose config entry
			// resolved cleanly would otherwise reappear under a PortID the set did
			// not hold, be re-probed every tick, and be re-provisionable.
			ids[h.ID] = true
			if h.PortID != "" {
				ids[h.PortID] = true
			}
			continue
		}
		// A config entry naming a same-serial twin by its shared serial resolves
		// ambiguous once the second unit is plugged in: the entry cannot open (it is
		// skipped ambiguous), but the matches are the very units the enumeration now
		// offers under their port ids. Claim every match so neither twin is
		// re-offered as available and re-provisioned into a dead entry while the
		// ambiguous entry stands; the operator's remedy is to delete it and re-add
		// each unit by its port id.
		var amb *capture.AmbiguousDeviceError
		if errors.As(err, &amb) {
			for _, m := range amb.Matches {
				ids[m] = true
			}
		}
	}
	a.hw = hw
	a.prov.setConfiguredIDs(ids)
}

// hardwareOwner returns the name of another serving device that already captures
// from the hardware at hwAddr, or "" when none does. Two config entries can name
// one physical device through different ids (a stable id and a card index), and
// the second open would only fail busy, so it is refused up front with a reason
// the operator can act on.
func (a *appliance) hardwareOwner(hwAddr, name string) string {
	if hwAddr == "" {
		return ""
	}
	for other, rt := range a.devices {
		if other != name && !rt.superseded && rt.hwAddr == hwAddr && rt.currentState() == mgmtserver.StateServing {
			return other
		}
	}
	return ""
}

// deviceDownKey is the notification-center condition key for a device that is
// unavailable: it could not be opened, or it died after opening. Every down
// onset, cause change, recovery clear, and removal or disable resolve for a
// device uses this one key, so its down condition has a single identity from
// onset to clear no matter which site raises or clears it.
func deviceDownKey(name string) string { return "device:" + name + ":down" }

// deviceDownOnset builds the error onset for a device that is unavailable. Every
// site that raises a down condition builds it here, so each carries the same
// severity, category, key and source identity that the recovery clear and the
// removal or disable resolve pair with.
func deviceDownOnset(name, title, message string) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityError,
		Category: notify.CategoryDevice,
		Key:      deviceDownKey(name),
		Source:   name,
		Title:    title,
		Message:  message,
	}
}

// deviceCaps is the last non-empty capability probe for one device id.
type deviceCaps struct {
	rates    []int
	channels []int
}

// rememberCaps records non-empty probe results and substitutes the last-known
// values for an empty one, so a device that is transiently unprobable during a
// hot-reload card swap keeps its previously reported rates and channels instead
// of momentarily reporting none.
func (a *appliance) rememberCaps(id string, rates, channels []int) (keptRates, keptChannels []int) {
	c := a.capsCache[id]
	switch {
	case len(rates) > 0:
		c.rates = rates
	case len(c.rates) > 0:
		rates = c.rates
	}
	switch {
	case len(channels) > 0:
		c.channels = channels
	case len(c.channels) > 0:
		channels = c.channels
	}
	a.capsCache[id] = c
	return rates, channels
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

// pump runs one device's capture-to-RTP loop until its capture ends (a clean stop
// or a failure), then reports the result. The fan-out reader runs on this
// goroutine, locked to its OS thread so the capture read is not descheduled
// mid-period; each stream's pipeline runs on its own goroutine so N encodes fan
// across cores and a slow encoder cannot blow the capture period budget. When the
// capture ends the fan-out closes the stream feeds, so every stage goroutine
// returns, and pump waits for them before reporting so no stage outlives the
// device's teardown.
func (a *appliance) pump(rt *deviceRuntime) {
	runtime.LockOSThread()
	var wg sync.WaitGroup
	// stageErr records the first spontaneous per-stream pipeline fault so it can be
	// reported as the pump's result when the fan-out itself ended cleanly.
	var stageOnce sync.Once
	var stageErr error
	for i := range rt.streams {
		sr := rt.streams[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sr.stage.Run(sr.src, func(f pipeline.Frame) error {
				if !sr.frames.Push(f) {
					drops := sr.dropped.Add(1)
					if drops%50 == 1 {
						log.Printf("%s (%s): dropping frames: the client is not keeping up (total drops: %d)", rt.dev.Name, sr.stream.Path, drops)
					}
				}
				return a.ctx.Err()
			})
			// A non-nil error while the appliance is NOT shutting down is a spontaneous
			// pipeline fault (for example an Opus encoder failure). The capture is shared
			// across the device's streams, and a dead stage would otherwise leave its
			// track mounted and its consumer unread (the fan-out would spin dropping
			// periods, spuriously tripping the drop monitor). So end the whole device:
			// record the fault and close the fan-out, which unblocks the reader below and
			// drives onPumpDone to fail the device (its paths 404 until it restarts), matching
			// the pre-fan-out contract. On shutdown a.ctx.Err() is set and the stage
			// returns that, which is a clean stop, not a fault.
			if err != nil && a.ctx.Err() == nil {
				log.Printf("%s (%s): capture pipeline stopped: %v", rt.dev.Name, sr.stream.Path, err)
				stageOnce.Do(func() { stageErr = err })
				_ = rt.fanout.Close()
			}
		}()
	}
	perr := rt.fanout.Run()
	wg.Wait()
	// A stage fault that ended the device surfaces as the pump result when the
	// fan-out's own read ended cleanly (a closed source reports EOF, i.e. nil here).
	if perr == nil {
		perr = stageErr
	}
	a.pumpDone <- pumpResult{rt: rt, err: perr}
}

// openAndStart opens a device, wires its RTSP track and level meter, and starts
// its pump. A device that fails to open is not fatal: it returns a skipped record
// carrying the open error so GET /devices can report it, exactly as at startup.
//
// The configured id was resolved by refreshHardware. A device whose id names no
// present hardware, or more than one device, is refused rather than opened: the
// id never falls back to whatever card holds an index, so a stream never serves
// the wrong microphone. A card-index id whose resolution failed for another
// reason (the host exposes no /proc/asound listing, as in some containers) is
// still opened, since the open itself addresses the card directly.
func (a *appliance) openAndStart(dev *config.Device) *deviceRuntime {
	res := a.hw[dev.Device]
	hw := res.hw
	if res.err != nil {
		var nf *capture.DeviceNotFoundError
		var amb *capture.AmbiguousDeviceError
		var bad *capture.BadDeviceError
		// A malformed id (*BadDeviceError) is refused here rather than falling
		// through to an open that can only fail: it is not a card index, so it must
		// not be opened unresolved (the container fallback), and reporting it as
		// malformed is more useful than a generic open failure mislabelled as a card
		// index a few lines down.
		if !config.IsCardIndexID(dev.Device) || errors.As(res.err, &nf) || errors.As(res.err, &amb) || errors.As(res.err, &bad) {
			cause, msg := resolveError(dev, res.err)
			title := "Device unavailable"
			switch cause {
			case downNotConnected:
				title = "Device not connected"
			case downAmbiguous:
				title = "Device ambiguous"
			case downMalformed:
				title = "Invalid device id"
			}
			return a.skipDevice(dev, &hw, cause, title, msg)
		}
	}
	if owner := a.hardwareOwner(hw.HWAddr, dev.Name); owner != "" {
		msg := fmt.Sprintf("same hardware as %q: %s is %s, which that device already captures from", owner, dev.Device, hw.HWAddr)
		return a.skipDevice(dev, &hw, downSameHardware, "Device conflict", msg)
	}
	if config.IsCardIndexID(dev.Device) {
		log.Printf("device %q is pinned to card index %s, which can name a different device after a reboot or replug; re-add it to bind it by identity", dev.Name, dev.Device)
	}

	// Probe by the current-boot address the id just resolved to (hw.HWAddr), not
	// the stable id: each probe call re-resolves a stable id (a full host
	// enumeration) inside go-audio-capture before opening, so probing by id costs
	// ~18 re-enumerations per device open for capability lists the UI only
	// displays. The address came from the resolution a few lines above, and the
	// real capture open in openDeviceRetry still goes by the stable id
	// (dev.Device), so a probe by address cannot make the open reach the wrong
	// device. Fall back to dev.Device when there is no resolved address (a
	// card-index id opened without a resolution, the container fallback).
	probeID := dev.Device
	if hw.HWAddr != "" {
		probeID = hw.HWAddr
	}
	// Resolve the hardware channel count for the capability PROBE only. The
	// open itself re-resolves per attempt inside the opener (openDeviceRetry), as
	// close to the open as possible, so a card transiently held right after a
	// restart is opened at its correct count once it frees rather than at a
	// fallback pinned here. A wrong value here costs at most a cosmetic
	// capability list, and rememberCaps below retains the last known good.
	openCh := resolveOpenChannels(probeID, dev.StreamChannelUnion())
	// Probe supported rates and channels for the config UI before opening: once we
	// hold the hw device exclusively the probe would see our own process and report
	// busy. Both use the same non-blocking capability query. Rates are probed at
	// the count we will actually open, since a device's rate set can depend on
	// the channel count.
	rates := audio.ProbeRates(probeID, openCh, audio.CandidateRates())
	channels := audio.ProbeChannels(probeID, audio.CandidateChannels())
	// Keep the last-known caps if this probe came back empty (a transient
	// card-swap window), so the UI does not flicker to an empty list. The cache
	// key stays the configured id so cached caps follow the device across a
	// card-index change of address.
	rates, channels = a.rememberCaps(dev.Device, rates, channels)

	d := *dev
	rt, err := a.open(&d, a.hub)
	if err != nil {
		log.Printf("skipping device %q (%s%s): %v", dev.Name, dev.Device, atAddr(&hw), err)
		// Record the open failure as a down-condition onset. Onset is idempotent,
		// so a device that keeps failing across successive reconciles enters the
		// condition once, not once per retry.
		n := deviceDownOnset(dev.Name, "Device unavailable", fmt.Sprintf("Could not open %s%s: %v", dev.Device, atAddr(&hw), err))
		a.markDown(dev.Name, downOpenFailed, &n)
		return &deviceRuntime{
			dev:               *dev,
			state:             mgmtserver.StateSkipped,
			err:               err.Error(),
			friendlyName:      hw.Label,
			hwAddr:            hw.HWAddr,
			supportedRates:    rates,
			supportedChannels: channels,
		}
	}
	rt.state = mgmtserver.StateServing
	rt.friendlyName = hw.Label
	rt.hwAddr = hw.HWAddr
	rt.supportedRates = rates
	rt.supportedChannels = channels
	for _, sr := range rt.streams {
		a.srv.AddTrack(sr.track)
	}
	a.alive++
	go a.pump(rt)
	if len(rt.streams) == 1 {
		log.Printf("capture %q: %d Hz, %d ch on %s%s serving %s", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, atAddr(&hw), rt.streams[0].stream.Path)
	} else {
		log.Printf("capture %q: %d Hz, %d ch on %s%s serving %d streams", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, atAddr(&hw), len(rt.streams))
	}
	return rt
}

// atAddr renders where a resolved device currently sits, for logs and messages:
// " (hw:4,0, Scarlett Solo 4th Gen)", or "" for a device that did not resolve.
func atAddr(hw *audio.Hardware) string {
	switch {
	case hw.HWAddr == "":
		return ""
	case hw.Label == "":
		return " (" + hw.HWAddr + ")"
	default:
		return " (" + hw.HWAddr + ", " + hw.Label + ")"
	}
}

// skipDevice records a configured device that was refused before any open,
// raising its down condition with the given cause. The last known capabilities
// are kept so the settings form still offers the device's rates.
func (a *appliance) skipDevice(dev *config.Device, hw *audio.Hardware, cause, title, msg string) *deviceRuntime {
	log.Printf("skipping device %q: %s", dev.Name, msg)
	n := deviceDownOnset(dev.Name, title, msg)
	a.markDown(dev.Name, cause, &n)
	rates, channels := a.rememberCaps(dev.Device, nil, nil)
	return &deviceRuntime{
		dev:               *dev,
		state:             mgmtserver.StateSkipped,
		err:               msg,
		friendlyName:      hw.Label,
		hwAddr:            hw.HWAddr,
		supportedRates:    rates,
		supportedChannels: channels,
	}
}

// startDevice opens a device via openAndStart, stores its runtime, and clears the
// device's down condition when a previously skipped or failed device is now
// serving. The open-failure onset is emitted inside openAndStart; the recovery
// clear lives here because it needs the previous record for the name, read before
// the reassignment. A healthy param-change restart (prev already serving) clears
// nothing, and Clear is idempotent, so a first start with no prior condition is
// silent too.
func (a *appliance) startDevice(dev *config.Device) {
	prev := a.devices[dev.Name]
	rt := a.openAndStart(dev)
	a.devices[dev.Name] = rt
	if rt.currentState() != mgmtserver.StateServing {
		return
	}
	delete(a.downReason, dev.Name)
	if prev == nil {
		return
	}
	// A device is "recovered" only when it comes up from a down state (it could
	// not be opened, or it died after opening). A healthy param-change restart
	// (prev already serving) is not a recovery. Clear is idempotent, so a
	// first-time start with no prior condition would be a no-op anyway.
	if s := prev.currentState(); s == mgmtserver.StateSkipped || s == mgmtserver.StateFailed {
		// Clear takes the category, source and key from the stored onset, so only
		// severity, title and message are set here.
		a.notifier.Clear(deviceDownKey(dev.Name), notify.Notification{
			Severity: notify.SeverityInfo,
			Title:    "Device recovered",
			Message:  fmt.Sprintf("Capturing again at %d Hz, %d ch", rt.rate, rt.channels),
		})
	}
}

// stop tears down a serving device the reconcile deliberately removed or is about
// to restart: it retires the RTSP track and level meter and closes the capture
// source, which ends the pump. It marks the runtime superseded so the pump's
// final pumpResult skips the spontaneous-death cleanup. The pump's exit adjusts
// the alive count, so stop does not.
func (a *appliance) stop(rt *deviceRuntime) {
	rt.superseded = true
	for _, sr := range rt.streams {
		a.srv.RemoveTrack(sr.track.Path)
		sr.frames.Close()
	}
	a.hub.RemoveMeter(rt.dev.Name)
	// Close through the fan-out (idempotent) so the base capture is closed exactly
	// once even if the pump's own stage-fault path already closed it.
	_ = rt.fanout.Close()
}

// reconcile applies newCfg to the running pipeline: it starts newly enabled or
// added devices, stops removed or disabled ones, and restarts those whose capture
// parameters changed, while leaving unchanged devices serving. It then republishes
// the device records and, if the serving set, the discovery flag, or the auth
// hint changed, restarts the mDNS advertisement.
func (a *appliance) reconcile(newCfg *config.Config) {
	prevDiscovery := a.prov.discoveryEnabled()
	prevAuth := a.prov.authRequired()

	// Apply the access token and the auth state BEFORE the device work below.
	// PATCH /config already enforces a patched token on the guard before it
	// invokes this reload, so the guard is not the reason for the ordering; the
	// reported state is. Device stops, restarts and opens can take seconds of
	// retries, and GET /status reads this flag per request, so setting it
	// afterwards answered with the old value for that whole window. (The mDNS
	// TXT hint is unaffected either way: restartAnnounce runs at the end of the
	// reconcile and reads the flag then.) Doing it here also makes the reconcile
	// self-sufficient rather than relying on its caller having set the guard.
	// Set is idempotent: an unchanged token does not advance the generation, so
	// this cannot disturb a live RTSP session.
	a.guard.Set(newCfg.Auth.Token)
	a.prov.setAuthRequired(newCfg.AuthRequired())

	// Resolve every configured id against the host's current hardware, and
	// publish the desired configured-device ids BEFORE opening anything, so the
	// background enumeration excludes a device from probing before its capture
	// open begins and the probe and the open never contend for the same device.
	a.refreshHardware(newCfg)

	plan := reload.Reconcile(a.runningParams(), newCfg)
	// Sort each pass into config order rather than the plan's name order, so when
	// two entries resolve to the same hardware the earlier entry in the config
	// wins it (see hardwareOwner) rather than whichever happens to sort first by
	// name. This orders within each pass; the restart pass then opens before the
	// start pass below, and any device left serving keeps its hardware, so a new
	// or restarted entry cannot take a still-serving device's card.
	order := make(map[string]int, len(newCfg.Devices))
	for i := range newCfg.Devices {
		order[newCfg.Devices[i].Name] = i
	}
	byConfigOrder := func(x, y config.Device) int { return order[x.Name] - order[y.Name] }
	slices.SortFunc(plan.Start, byConfigOrder)
	slices.SortFunc(plan.Restart, byConfigOrder)

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
		a.startDevice(&plan.Restart[i])
	}
	for i := range plan.Start {
		a.startDevice(&plan.Start[i])
	}

	a.reconcileRecords(newCfg)

	a.cfg = *newCfg
	a.prov.setDiscovery(newCfg.DiscoveryEnabled())
	a.publish(newCfg)

	// Ask the background enumeration goroutine to refresh the available-device
	// list now that the configured set changed, so a just-provisioned device
	// leaves the list and a just-removed one rejoins it promptly. The actual
	// hardware probing runs on that goroutine, never here: probing opens devices
	// and can be slow, and this reconcile runs on the capture run loop that also
	// drives pump events, reloads and shutdown.
	a.prov.signalEnumerate()

	// Rebuild the mDNS advertisement whenever any device changed, discovery
	// toggled, or the auth hint changed (the TXT record advertises auth=token or
	// auth=none). A param-change restart keeps the serving count identical but
	// alters the advertised path/rate/codec, so gate on the plan being non-empty,
	// not on the count. dnssd cannot retire a single service, so restartAnnounce
	// rebuilds the whole set.
	if !plan.Empty() || newCfg.DiscoveryEnabled() != prevDiscovery || newCfg.AuthRequired() != prevAuth {
		a.restartAnnounce()
	}

	// Re-arm the condition monitors with the new thresholds and per-device
	// quiet-alert opt-outs. Apply swaps the immutable settings value the monitors
	// read each tick; it never restarts a device, so a threshold change moves no
	// capture. A nil monitors (a test that wires none) is a no-op.
	if a.monitors != nil {
		s := monitor.SettingsFrom(newCfg)
		a.monitors.Apply(&s)
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
		//
		// A device disabled while it was down (skipped or failed) never reaches the
		// recovery clear, so resolve its condition here before the fresh disabled
		// record replaces it. Resolve is a no-op when the key is not active, so a
		// healthy device being disabled emits nothing.
		a.notifier.Resolve(deviceDownKey(d.Name), "device disabled")
		delete(a.downReason, d.Name)
		hw := a.hw[d.Device].hw
		a.devices[d.Name] = &deviceRuntime{dev: d, state: mgmtserver.StateDisabled, friendlyName: hw.Label, hwAddr: hw.HWAddr}
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
		// A device removed from the configuration while it was down never reaches
		// the recovery clear either, so resolve its condition before it is dropped.
		// A serving or already-recovered device has no active down key, so this is
		// a no-op for it.
		a.notifier.Resolve(deviceDownKey(name), "device removed from the configuration")
		delete(a.downReason, name)
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
// its own is a device that died after startup: its track and meter are retired
// and its record marked failed. Its paths return 404 until a config reload or a
// change in the host's capture hardware (see retryDown) starts it again. It arms
// an unattended retry only when the device was lost (unplugged or powered off); a
// failure that is not a confirmed loss does not arm one, since re-arming a
// still-present device that keeps failing would restart and re-notify it every
// enumeration tick.
func (a *appliance) onPumpDone(res pumpResult) {
	a.alive--
	if res.rt.superseded {
		return
	}
	for _, sr := range res.rt.streams {
		a.srv.RemoveTrack(sr.track.Path)
		sr.frames.Close()
	}
	_ = res.rt.fanout.Close()
	a.hub.RemoveMeter(res.rt.dev.Name)
	if res.err != nil && a.ctx.Err() == nil {
		a.lastPumpErr = res.err
		res.rt.markFailed(res.err)
		name := res.rt.dev.Name
		// A device that died after opening enters the same down condition as one
		// that never opened. Decide whether the device was LOST (unplugged or
		// powered off) or merely FAILED while still present. ErrDeviceGone is the
		// library's clean loss signal, but it does not cover every way a lost device
		// can surface (its gone-errno set is a fixed few, so some unplugs come back
		// as a raw errno), so when the error is not ErrDeviceGone also re-resolve
		// the configured id: a *DeviceNotFoundError means the id no longer names
		// present hardware, i.e. the device is gone. The re-resolve costs one host
		// enumeration, acceptable on a spontaneous pump death (it is off every hot
		// path).
		lost := errors.Is(res.err, capture.ErrDeviceGone)
		if !lost {
			if _, rerr := a.resolve(res.rt.dev.Device); rerr != nil {
				var nf *capture.DeviceNotFoundError
				lost = errors.As(rerr, &nf)
			}
		}
		if lost {
			// A lost device comes back on its own once reconnected, so arm a retry:
			// the next enumeration restarts it even when the host's hardware
			// signature is unchanged (an unplug and replug at the same card index
			// within one enumeration tick). See provider.retryArmed.
			a.prov.armRetry()
			// retryDown does not restart a card-index entry unattended (its index
			// may name different hardware after a reconnect, the #62 swap), so the
			// message must tell the truth for each: a stable id comes back on
			// reconnect, a card index waits for a config save.
			msg := "Capture stopped because the device was disconnected; it starts again when the device is reconnected"
			if config.IsCardIndexID(res.rt.dev.Device) {
				msg = "Capture stopped because the device was disconnected; it restarts on the next config save (its card index may name different hardware after a reconnect)"
			}
			log.Printf("device %q disconnected: %v; its %d stream path(s) return 404 until it comes back", name, res.err, len(res.rt.streams))
			n := deviceDownOnset(name, "Device disconnected", msg)
			a.markDown(name, downDisconnected, &n)
		} else {
			// The pump died but the device did not read as lost: it still resolves to
			// present hardware, or the failure could not be confirmed as a loss (a
			// deterministic encoder fault, an EIO right after open). Do NOT arm a
			// retry: an armed retry would restart it every enumeration tick, which
			// flaps the onset/clear condition and climbs announceGen forever. It
			// restarts on the next config save or host hardware change instead.
			log.Printf("device %q failed: %v; its %d stream path(s) return 404 until it restarts on a config save or a capture hardware change", name, res.err, len(res.rt.streams))
			n := deviceDownOnset(name, "Device failed", fmt.Sprintf("Capture stopped: %v; the RTSP path(s) return 404 until the device restarts on the next config save or capture hardware change", res.err))
			a.markDown(name, downFailed, &n)
		}
	}
	a.publish(&a.cfg)
}

// retryDown runs when the host's capture hardware changed (a device was plugged,
// unplugged, or renumbered). It re-resolves every configured id and starts each
// enabled device that is down (skipped or failed), so a device reconnected on a
// different card index serves again on the right hardware without a config
// save. Serving devices are left alone: they hold their hardware open, so a
// renumbering of other cards cannot move them.
//
// A card-index entry (config.IsCardIndexID) is NOT restarted here: its index
// names a card by kernel probe order, so after a hardware change the index may
// name a different device than it did when the entry went down, and an
// unattended restart could open the wrong microphone (the #62 swap). Such an
// entry is restarted only by an explicit config save.
func (a *appliance) retryDown() {
	a.refreshHardware(&a.cfg)
	started := false
	for i := range a.cfg.Devices {
		d := a.cfg.Devices[i]
		rt, ok := a.devices[d.Name]
		if !ok || !d.IsEnabled() {
			continue
		}
		if config.IsCardIndexID(d.Device) {
			// Its index may now name different hardware than when it went down, so
			// it waits for an explicit config save rather than restarting here.
			continue
		}
		if s := rt.currentState(); s != mgmtserver.StateSkipped && s != mgmtserver.StateFailed {
			continue
		}
		a.startDevice(&d)
		if a.devices[d.Name].currentState() == mgmtserver.StateServing {
			started = true
		}
	}
	// Rebuild the disabled records so their hardware address follows the change.
	a.reconcileRecords(&a.cfg)
	a.publish(&a.cfg)
	if started {
		a.restartAnnounce()
	}
}

// restartAnnounce cancels the current mDNS advertisement and starts a fresh one
// for the serving set. dnssd cannot retire a single service, so the whole
// advertisement is rebuilt whenever the serving set, the discovery flag, or the
// auth hint (the TXT auth=token/auth=none record) changes.
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
	a.announceGen++
	startAnnounce(actx, a.cfg.Listen, serving, a.prov.authRequired())
}

// closeAll releases every serving device's capture source at shutdown so the
// ALSA hardware is freed promptly rather than at process exit.
func (a *appliance) closeAll() {
	for _, rt := range a.devices {
		if rt.currentState() == mgmtserver.StateServing && rt.fanout != nil {
			_ = rt.fanout.Close()
		}
	}
}
