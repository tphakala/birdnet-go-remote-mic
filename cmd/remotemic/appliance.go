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
	"time"

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
// that stopped it. A deliberate stop may still carry an error (a real capture
// returns capture.ErrClosed once closed), so onPumpDone decides by superseded
// and the appliance context, never by a nil error.
type pumpResult struct {
	rt  *deviceRuntime
	err error
	// faultPath is the path of the stream whose stage (an encode or packetize
	// fault) ended the device; empty when err came from the capture itself.
	faultPath string
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
	// idempotent Onset. The exception is a switch between two retryable causes
	// while an unattended retry is in flight, which keeps the first cause (see
	// markDown). An entry exists exactly while the down key is active.
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
	// advertised holds the names of the devices in the current advertisement
	// (empty while discovery is off or nothing serves), so a restart waiting
	// for a client to prove its encoder can tell whether a rebuild during its
	// outage dropped it (see attemptRetry).
	advertised map[string]bool

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

	// retries holds the unattended restart state of each device, keyed by device
	// name, from its first failure for a cause a later restart can fix (see
	// scheduleRetry) through its settle; a recovered device keeps it until its next
	// failure, so the backoff can tell whether it served for retryResetAfter. An
	// entry therefore does not mean the device is down; retrying means its down
	// condition is still active (it may already be serving, in its settle).
	// retryTimer fires at the earliest pending retry or settle deadline and
	// signals retryDue (buffered depth 1, coalescing), which the run loop drains
	// into onRetryDue; retryAt is the deadline it was last armed for, so
	// re-arming for an unchanged deadline is a no-op. It is zero while stopped,
	// and onRetryDue zeroes it before re-arming; a timer left armed across that
	// (a stale signal's pass) only costs one spurious, harmless pass. Between a
	// firing and onRetryDue it still holds the spent deadline, which is safe
	// because the signal is pending.
	// quietDown silences the open's log lines during a retry attempt that
	// logAttempt skips.
	retries    map[string]*retryState
	retryTimer *time.Timer
	retryAt    time.Time
	retryDue   chan struct{}
	quietDown  bool
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
		advertised: map[string]bool{},
		retries:    map[string]*retryState{},
		retryDue:   make(chan struct{}, 1),
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
// device stays down re-raises the condition, except between two retryable
// classes during an unattended retry (see markDown).
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
// rather than the first one. The exception is an unattended restart in flight
// whose failure moves between two retryable causes (a pump that died, then an
// open that failed): the existing condition is kept, so a device alternating
// between them is not re-notified on every attempt. The trade-off is that the
// active notification keeps the first cause's text; the device record's error
// always carries the latest one.
func (a *appliance) markDown(name, cause string, n *notify.Notification) {
	if prev, ok := a.downReason[name]; ok && prev != cause {
		if a.retrying(name) && retryableCause(prev) && retryableCause(cause) {
			return
		}
		a.notifier.Resolve(deviceDownKey(name), "the cause changed")
	}
	a.downReason[name] = cause
	a.notifier.Onset(*n)
}

// resolveError explains why a configured device was not opened after its id
// failed to resolve, returning the cause class and the operator-facing message.
func resolveError(dev *config.Device, err error) (cause, msg string) {
	if _, ok := errors.AsType[*capture.DeviceNotFoundError](err); ok {
		return downNotConnected, fmt.Sprintf("Not connected: no device matches %s", dev.Device)
	}
	if amb, ok := errors.AsType[*capture.AmbiguousDeviceError](err); ok {
		// The remedy works from either surface that shows this message (the web
		// card and the CLI check): remove this entry and add each unit by its own
		// id. The web UI enables each available unit by its id; a CLI user
		// configures each by the id from `devices list`. The old "bind it by port"
		// wording was config-file jargon the web UI could not act on.
		return downAmbiguous, fmt.Sprintf("Ambiguous: %s matches %d devices (%s). Remove this entry and re-add each unit by its own id.", dev.Device, len(amb.Matches), strings.Join(amb.Matches, ", "))
	}
	if _, ok := errors.AsType[*capture.BadDeviceError](err); ok {
		// A malformed id ("plughw:1,0", "hw:Loopback,1", a typo) is in no accepted
		// form and can never open. IsCardIndexID classifies these by shape as card
		// indexes, but unlike a real card index no reboot can make them resolve, so
		// report them as malformed and tell the operator to fix the id.
		return downMalformed, fmt.Sprintf("Malformed device id %s: %v. Re-add the device to bind it to real hardware.", dev.Device, err)
	}
	return downResolve, fmt.Sprintf("Cannot resolve %s: %v", dev.Device, err)
}

// refreshHardware resolves every configured device id against the host's
// current hardware and publishes the ids the configuration owns to the
// background enumeration. The published set holds the configured id plus every id
// form the resolution recognises it under: the stable id it resolved to, its port
// id, and, for an entry that resolves ambiguously (a same-serial twin named by its
// shared serial), each matching unit's port id. So a device configured by a card
// index, or an owned same-serial twin, is still recognised as configured and not
// re-offered as available.
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
		if amb, ok := errors.AsType[*capture.AmbiguousDeviceError](err); ok {
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
// across cores and a slow encoder cannot blow the capture period budget. Each
// stage is gated on its own stream feed's play session, so it encodes only
// while a client plays that stream (an Opus stage starts each client from a
// fresh encoder), and discards unencoded any period it reads with no client.
// The fan-out is gated on the feed's active flag: it sends an idle stream
// nothing, so an idle stage blocks in its read and the fan-out never backs up.
// An idle appliance thus pays for the capture read but not for copying, channel
// extraction, encoding, or waking the idle stages. When the capture ends
// the fan-out closes the stream feeds, so every stage goroutine returns, and
// pump waits for them before reporting so no stage outlives the device's
// teardown. When a stage (an encode fault) ended the device, the result carries
// that stage's error and names its stream; each stream's first encoded frame is
// recorded on it (noteEncoded) for the unattended retry's settle.
func (a *appliance) pump(rt *deviceRuntime) {
	runtime.LockOSThread()
	var wg sync.WaitGroup
	// stageErr records the first spontaneous per-stream pipeline fault, which is
	// reported as the pump's result in place of the fan-out's error.
	var stageOnce sync.Once
	var stageErr error
	var faultPath string
	for i := range rt.streams {
		sr := rt.streams[i]
		wg.Go(func() {
			err := sr.stage.Run(sr.src, sr.frames.Session, func(f pipeline.Frame) error {
				sr.noteEncoded(&rt.awaitEncode, a.signalRetryDue)
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
			// the pre-fan-out contract. On shutdown a stage returns a.ctx.Err() from
			// emit or nil once the closed fan-out ends its source (an idle stage, which
			// never emits, only the latter); either is a clean stop, not a fault.
			// Because a stage encodes only while a client plays, an encode fault
			// surfaces at a client's PLAY, not at open (see retrySettle).
			if err != nil && a.ctx.Err() == nil {
				log.Printf("%s (%s): capture pipeline stopped: %v", rt.dev.Name, sr.stream.Path, err)
				stageOnce.Do(func() { stageErr, faultPath = err, sr.stream.Path })
				_ = rt.fanout.Close()
			}
		})
	}
	perr := rt.fanout.Run()
	wg.Wait()
	// A stage fault that ended the device is the pump result, whatever the
	// fan-out's read returned after the stage closed it: a real capture reports
	// capture.ErrClosed once closed, not EOF, so keying on a clean fan-out end
	// would hide every encode fault on hardware. A stage faults only on its own:
	// when the capture fails first, each stage drains its queued periods, sees
	// its source end and normally returns nil, so stageErr is set only for a
	// genuine stage fault (one on a queued period can still win over the
	// capture error, an unlikely double fault).
	res := pumpResult{rt: rt, err: perr}
	if stageErr != nil {
		res.err, res.faultPath = stageErr, faultPath
	}
	a.pumpDone <- res
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
// restartHint says, in a down notification, how the device comes back: a
// card-index id waits for a config save (it is never restarted unattended),
// anything else is retried by the backoff. The open-failure, resolve-failure
// and failed-while-present messages share it so their promise cannot drift; the
// disconnect message has its own wording (it comes back on reconnect).
func restartHint(dev *config.Device) string {
	if config.IsCardIndexID(dev.Device) {
		return "it restarts on the next config save"
	}
	return "it restarts automatically when capture works again"
}

func (a *appliance) openAndStart(dev *config.Device) *deviceRuntime {
	res := a.hw[dev.Device]
	hw := res.hw
	if res.err != nil {
		_, nf := errors.AsType[*capture.DeviceNotFoundError](res.err)
		_, amb := errors.AsType[*capture.AmbiguousDeviceError](res.err)
		_, bad := errors.AsType[*capture.BadDeviceError](res.err)
		// A malformed id (*BadDeviceError) is refused here rather than falling
		// through to an open that can only fail. IsCardIndexID returns true for a
		// malformed id (for example "plughw:1,0"), so the !IsCardIndexID term does
		// NOT catch it; the explicit bad term is what refuses it, and reporting it
		// as malformed is more useful than a generic open failure mislabelled as a
		// card index a few lines down. A card index whose enumeration merely failed
		// resolves to a wrapped ErrDeviceGone, not *BadDeviceError, so it still
		// falls through to the container-fallback open.
		if !config.IsCardIndexID(dev.Device) || nf || amb || bad {
			cause, msg := resolveError(dev, res.err)
			title := "Device unavailable"
			switch cause {
			case downNotConnected:
				title = "Device not connected"
			case downAmbiguous:
				title = "Device ambiguous"
			case downMalformed:
				title = "Invalid device id"
			case downResolve:
				// Only this cause is retried by the backoff; the others wait for
				// the hardware or the config to change, as their text says.
				msg += "; " + restartHint(dev)
			}
			return a.skipDevice(dev, &hw, cause, title, msg)
		}
	}
	if owner := a.hardwareOwner(hw.HWAddr, dev.Name); owner != "" {
		msg := fmt.Sprintf("Same hardware as %q: %s is %s, which that device already captures from", owner, dev.Device, hw.HWAddr)
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
		a.logAttemptf("skipping device %q (%s%s): %v", dev.Name, dev.Device, atAddr(&hw), err)
		// Record the open failure as a down-condition onset. Onset is idempotent,
		// so a device that keeps failing across successive reconciles enters the
		// condition once, not once per retry.
		n := deviceDownOnset(dev.Name, "Device unavailable", fmt.Sprintf("Could not open %s%s: %v; %s", dev.Device, atAddr(&hw), err, restartHint(dev)))
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
		a.logAttemptf("capture %q: %d Hz, %d ch on %s%s serving %s", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, atAddr(&hw), rt.streams[0].stream.Path)
	} else {
		a.logAttemptf("capture %q: %d Hz, %d ch on %s%s serving %d streams", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, atAddr(&hw), len(rt.streams))
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
	if retryableCause(cause) {
		a.logAttemptf("skipping device %q: %s", dev.Name, msg)
	} else {
		// A cause a retry cannot fix ends any backoff in flight (scheduleRetry
		// drops it without logging), so this line is the only record of why the
		// device stopped being retried; never silence it.
		log.Printf("skipping device %q: %s", dev.Name, msg)
	}
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

// startDevice opens a device via openAndStart at startup, on a config save, or
// on a hardware change (except a device waiting to prove an encoder after an
// encode fault, see retryDown), stores its runtime, and clears the device's
// down condition when a device that was down is now serving. The open-failure
// onset is emitted inside openAndStart. A healthy param-change restart has no
// active down condition, so it clears nothing, and a first start with no prior
// condition is silent too.
//
// None of these is an unattended retry, so it starts the device's backoff over:
// a failure here schedules the first, shortest retry when scheduleRetry accepts
// the cause (a stable id and a retryable cause), and a success ends any
// retry in flight, including a device still waiting out its settle after an
// unattended restart (its condition is still active, so it is cleared here).
func (a *appliance) startDevice(dev *config.Device) {
	delete(a.retries, dev.Name)
	rt := a.openAndStart(dev)
	a.devices[dev.Name] = rt
	if rt.currentState() != mgmtserver.StateServing {
		a.scheduleRetry(dev)
		return
	}
	a.armRetryTimer()
	a.finishRecovery(dev.Name, rt)
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
		delete(a.retries, d.Name)
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
		delete(a.retries, name)
		delete(a.devices, name)
	}
	a.armRetryTimer()
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
// and its record marked failed. Its paths return 404 until it starts again. A
// device that was lost (unplugged or powered off) arms the enumeration retry, so
// it restarts when it is reconnected (see retryDown). A device that failed while
// still present is retried on a backoff instead (see scheduleRetry), since
// re-arming the enumeration retry for a device that keeps failing would restart
// and re-notify it every enumeration tick; after an encode fault that retry
// must also prove its encoder before it counts as recovered (see
// retryState.encodePaths). Neither path restarts a card-index id, which waits
// for a config save.
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
				_, lost = errors.AsType[*capture.DeviceNotFoundError](rerr)
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
			// The hardware-change retry brings it back, not the backoff.
			a.dropRetry(name)
		} else {
			// The pump died but the device did not read as lost: it still resolves to
			// present hardware, or the failure could not be confirmed as a loss (a
			// deterministic encoder fault, an EIO right after open, a pump that died a
			// moment before the kernel removed the card). Do NOT arm the enumeration
			// retry: it would restart the device every enumeration tick, flapping the
			// onset/clear condition and climbing announceGen forever. Retry it on a
			// backoff instead (scheduleRetry), which keeps the condition active across
			// attempts and clears it once a retried restart has stayed up for
			// retrySettle. A config save clears it at once, and so does a hardware
			// change unless the fault was an encode fault (see retryDown). A
			// card-index entry is not retried unattended, so it waits for a config
			// save.
			restart := restartHint(&res.rt.dev)
			if a.nextFailureLogged(name) {
				log.Printf("device %q failed: %v; its %d stream path(s) return 404 until %s", name, res.err, len(res.rt.streams), restart)
			}
			n := deviceDownOnset(name, "Device failed", fmt.Sprintf("Capture stopped: %v; the RTSP path(s) return 404 until %s", res.err, restart))
			a.markDown(name, downFailed, &n)
			a.scheduleRetry(&res.rt.dev)
			if st := a.retries[name]; st != nil && res.faultPath != "" && !slices.Contains(st.encodePaths, res.faultPath) {
				// An encode fault surfaces only while a client plays that stream, so
				// the restart must prove that stream's encoder before it counts as
				// recovered; another stream encoding proves nothing about it.
				st.encodePaths = append(st.encodePaths, res.faultPath)
			}
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
//
// A device down after an encode fault is restarted as an unattended retry
// attempt (attemptRetry), not by startDevice: the hotplug is unrelated to its
// fault, so it keeps its down condition until each faulted stream has encoded.
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
		if !isDown(rt.currentState()) {
			continue
		}
		if st := a.retries[d.Name]; st != nil && len(st.encodePaths) > 0 {
			// A hardware change proves nothing about an encoder that faulted: restart
			// it as an unattended attempt, which keeps its condition until each
			// faulted stream has encoded, rather than clearing it at once and
			// faulting again at the next PLAY. Its pending backoff is consumed by
			// this attempt. Its reannounce result is not needed: any device that
			// serves after this loop triggers the rebuild below. (The proof covers
			// a hotplug of some other device; a disconnect of this device drops its
			// retry state, so its own replug restarts it through startDevice.)
			st.next = time.Time{}
			a.attemptRetry(&d, st)
		} else {
			a.startDevice(&d)
		}
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
	a.armRetryTimer()
}

// restartAnnounce cancels the current mDNS advertisement and starts a fresh one
// for the serving set. dnssd cannot retire a single service, so the whole
// advertisement is rebuilt whenever the serving set, the discovery flag, or the
// auth hint (the TXT auth=token/auth=none record) changes, and when a restart
// waiting to prove an encoder is missing from it (see attemptRetry). It records
// the names it advertises in a.advertised.
func (a *appliance) restartAnnounce() {
	if a.announceCancel != nil {
		a.announceCancel()
		a.announceCancel = nil
	}
	clear(a.advertised)
	if !a.prov.discoveryEnabled() {
		return
	}
	serving := make([]*deviceRuntime, 0, len(a.devices))
	for i := range a.cfg.Devices {
		if rt, ok := a.devices[a.cfg.Devices[i].Name]; ok && rt.currentState() == mgmtserver.StateServing {
			serving = append(serving, rt)
			a.advertised[rt.dev.Name] = true
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
	a.stopRetries()
	for _, rt := range a.devices {
		if rt.currentState() == mgmtserver.StateServing && rt.fanout != nil {
			_ = rt.fanout.Close()
		}
	}
}
