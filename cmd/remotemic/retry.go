//go:build linux

package main

import (
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// retryBackoff is the delay before each unattended restart attempt of a device
// that is down while still present, indexed by the number of consecutive failed
// attempts; the last entry repeats. It starts short, so a one-off USB hiccup or
// a device briefly held by another process is back within seconds, and caps at
// a few minutes, so a permanently broken device costs one open every 5 minutes.
var retryBackoff = [...]time.Duration{
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

const (
	// retrySettle is how long a device restarted by an unattended retry must keep
	// serving before it counts as recovered: only then is its down condition
	// cleared and the mDNS advertisement rebuilt. A device that opens and then
	// dies right away (an EIO after open, a configuration the encoder or stage
	// rejects at start) would otherwise publish a clear and a fresh onset, and
	// rebuild the advertisement, on every attempt. Its RTSP paths serve for the
	// whole window. A fault in the encode itself surfaces only while a client
	// plays (a stage encodes only then, see pipeline.Stage), so a restart after
	// an encode fault must also prove that stream's encoder: its settle starts
	// only once the faulted stream has encoded a frame (see
	// appliance.encodeFaults), and until a client plays it the device keeps its
	// down condition while it serves.
	retrySettle = 30 * time.Second
	// retryResetAfter is how long a recovered device must serve before a later
	// failure starts the backoff over from the shortest delay. A device that keeps
	// dying soon after each recovery continues from where its backoff was.
	retryResetAfter = 10 * time.Minute
	// retryLogFirst and retryLogEvery bound the retry logging: the first
	// retryLogFirst failures of each outage are logged, then every
	// retryLogEvery-th, which is about once an hour at the capped delay.
	retryLogFirst = 3
	retryLogEvery = 12
)

// retryState tracks the unattended restart of one down device. It exists from
// the device's first retryable failure, or from a restart restartFaulted makes
// for a device with an encode fault on record, until the device is removed,
// disabled, restarted by a config save or by a hardware change (except a device
// with an encode fault on record whose parameters the save left unchanged, whose
// restart is a retry attempt that keeps the state, see restartFaulted), or goes
// down for a cause a retry cannot fix.
type retryState struct {
	// attempts counts consecutive failures since the backoff last reset; it picks
	// the delay before the next attempt.
	attempts int
	// failures counts consecutive failures in the current outage, starting over
	// when a failure follows a completed settle. It gates the logging, so a new
	// outage is logged from its first failure even when attempts carries on from
	// an earlier one; attempts alone picks the delay.
	failures int
	// next is when the next restart attempt is due; zero while none is pending.
	next time.Time
	// settleAt is when a device restarted by a retry counts as recovered; zero
	// unless it is serving and waiting out retrySettle.
	settleAt time.Time
	// recoveredAt is when the device last completed a settle, used to reset the
	// backoff once it has served for retryResetAfter.
	recoveredAt time.Time
	// encodeSeen records that the current restart's faulted streams (see
	// appliance.encodeFaults) have all encoded, so its settle deadline is set
	// from when the run loop saw that.
	encodeSeen bool
	// awaitEncode marks a restart that served its settle window before every
	// faulted stream encoded. It has no deadline: a stream's first encoded
	// frame wakes the run loop (streamRuntime.noteEncoded).
	awaitEncode bool
}

// encodeFault records the streams of one device whose stage faulted in its
// current outage, and the device configuration they faulted under. A restart
// then counts as recovered only once each listed stream has encoded a frame
// and the device has served retrySettle after that: serving alone proves
// nothing, because with no client playing a stream nothing is encoded on it,
// and another stream encoding proves nothing about it. Settling on time alone
// would clear the condition and rebuild mDNS only for the next PLAY to fault
// again, once per client connect.
//
// It lives beside the retry state rather than in it, because it must outlive
// it: a disconnect of the device ends its backoff retry, and a config save or
// a hardware change restarts it, yet the same stream on the same encoder is
// no better for either. It is dropped when the proof completes, when the
// device is removed or disabled, and when a config save changes its capture or
// stream parameters (which builds a different stream).
type encodeFault struct {
	dev   config.Device
	paths []string
}

// faultedPaths returns the streams of the device whose encode faulted in its
// current outage; none when it had no encode fault.
func (a *appliance) faultedPaths(name string) []string { return a.encodeFaults[name].paths }

// backoffDelay returns the delay before the next attempt, given the backoff's
// attempt count (retryState.attempts, at least 1).
func backoffDelay(attempts int) time.Duration {
	i := min(max(attempts, 1), len(retryBackoff)) - 1
	return retryBackoff[i]
}

// logAttempt reports whether failure number n of the current outage
// (retryState.failures) is one to log, so a permanently failing device does not
// flood the log.
func logAttempt(n int) bool { return n <= retryLogFirst || n%retryLogEvery == 0 }

// nextFailureLogged reports whether the device's next failure is one to log.
// A device with no restart in flight starts a new outage at failure 1, which is
// always logged; one mid-retry is gated by logAttempt on the failure count it
// is about to reach. It is the one place that decision is made, shared by the
// retry scheduling, the retry attempt, and a pump death, so their logging
// cannot drift apart.
func (a *appliance) nextFailureLogged(name string) bool {
	if !a.retrying(name) {
		return true
	}
	return logAttempt(a.retries[name].failures + 1)
}

// enabledInConfig reports whether name is an enabled device in the current
// configuration.
func (a *appliance) enabledInConfig(name string) bool {
	for i := range a.cfg.Devices {
		if a.cfg.Devices[i].Name == name {
			return a.cfg.Devices[i].IsEnabled()
		}
	}
	return false
}

// isDown reports whether a device record is down: skipped (it could not be
// opened) or failed (it died after opening). Both unattended restart paths,
// the backoff retry and the hardware-change retry, restart only a down device.
func isDown(s mgmtserver.DeviceState) bool {
	return s == mgmtserver.StateSkipped || s == mgmtserver.StateFailed
}

// retryableCause reports whether a down cause is one a later restart of the same
// device can fix on its own: an open error (busy in another process, a transient
// driver error), a pump that died while the device stayed present, or a resolve
// failure that was not about the device itself. A device that is absent,
// ambiguous, malformed, or shadowed by another entry needs a hardware change or a
// config save instead, and a disconnect is retried by the hardware-change path.
func retryableCause(cause string) bool {
	switch cause {
	case downOpenFailed, downFailed, downResolve:
		return true
	default:
		return false
	}
}

// retrying reports whether the device has an unattended restart in flight: a
// retry state that has not completed a settle since its last failure. It holds
// across the whole cycle, including while an attempt's open runs (when neither
// deadline is set), so markDown keeps the condition on a cause switch there too.
func (a *appliance) retrying(name string) bool {
	st := a.retries[name]
	return st != nil && st.recoveredAt.IsZero()
}

// scheduleRetry arms the next unattended restart of a device that just went
// down, or drops its retry state when the cause is not one a retry can fix. A
// card-index entry is never restarted unattended: its index names a card by
// kernel probe order, so it may name different hardware by the time the retry
// runs (the #62 swap).
func (a *appliance) scheduleRetry(dev *config.Device) {
	name := dev.Name
	if config.IsCardIndexID(dev.Device) || !retryableCause(a.downReason[name]) {
		a.dropRetry(name)
		return
	}
	loud := a.nextFailureLogged(name)
	now := time.Now()
	st := a.retries[name]
	if st == nil {
		st = &retryState{}
		a.retries[name] = st
	}
	if !st.recoveredAt.IsZero() {
		// A failure after a completed settle starts a new outage.
		st.failures = 0
		if now.Sub(st.recoveredAt) >= retryResetAfter {
			st.attempts = 0
		}
	}
	st.recoveredAt = time.Time{}
	st.settleAt = time.Time{}
	st.encodeSeen = false
	st.awaitEncode = false
	st.attempts++
	st.failures++
	delay := backoffDelay(st.attempts)
	st.next = now.Add(delay)
	if loud {
		log.Printf("device %q: retrying in %s (failure %d)", name, delay, st.failures)
	}
	a.armRetryTimer()
}

// dropRetry ends any unattended restart of the device: for a card-index id,
// which is never restarted unattended, and for a down cause a retry cannot fix
// (a disconnect is brought back by the hardware-change retry).
func (a *appliance) dropRetry(name string) {
	delete(a.retries, name)
	// Re-arm even when there was no state here: startDevice deletes the state
	// before it opens, so the timer may still be armed for that deadline.
	// armRetryTimer is a no-op when the earliest deadline has not changed.
	a.armRetryTimer()
}

// armRetryTimer arms the single retry timer for the earliest pending retry or
// settle deadline, stops it when none is pending, and leaves it alone when that
// deadline has not changed. Callers touching several devices in one pass may
// call it per device; only a changed deadline costs a timer reset. The timer
// only signals retryDue; the run loop does the work in onRetryDue, so the
// appliance state stays single-goroutine.
func (a *appliance) armRetryTimer() {
	var due time.Time
	for _, st := range a.retries {
		for _, t := range [...]time.Time{st.next, st.settleAt} {
			if !t.IsZero() && (due.IsZero() || t.Before(due)) {
				due = t
			}
		}
	}
	if due.Equal(a.retryAt) {
		return
	}
	a.retryAt = due
	if due.IsZero() {
		if a.retryTimer != nil {
			a.retryTimer.Stop()
		}
		return
	}
	if a.retryTimer == nil {
		a.retryTimer = time.AfterFunc(time.Until(due), a.signalRetryDue)
		return
	}
	a.retryTimer.Reset(time.Until(due))
}

// signalRetryDue is the retry timer's callback. The send never blocks: retryDue
// holds one pending signal, and a signal already pending covers this one,
// because onRetryDue checks every deadline against the clock.
func (a *appliance) signalRetryDue() {
	select {
	case a.retryDue <- struct{}{}:
	default:
	}
}

// onRetryDue runs on the run loop when the retry timer fires, or when a stream of
// a restart waiting to prove its encoders encodes its first frame. It completes
// the settle of every restarted device that has served for retrySettle (after
// an encode fault, counted from when the run loop first sees every faulted
// stream encoded), and makes one restart attempt for every down device whose
// backoff has elapsed. It rebuilds the mDNS advertisement once per pass when a
// device recovered, or when a restart that must prove an encoder is missing
// from it. A stale or early signal completes and attempts nothing,
// since each deadline is checked against the clock and each encode wait
// against the runtime.
//
// The run loop's select has no priority, so a pump that died right at the
// settle deadline can be handled after this pass: the settle then completes
// (clear and re-announce) and onPumpDone raises a fresh onset right after. The
// window is one run-loop turn wide, and the device is retried as usual.
func (a *appliance) onRetryDue() {
	// Forget the armed deadline so the armRetryTimer below recomputes and arms
	// afresh rather than trusting it. A stale signal may arrive while the timer
	// is armed for a later deadline; re-arming for that deadline is harmless.
	a.retryAt = time.Time{}
	now := time.Now()
	// Drop retry state for any name that is not an enabled configured device,
	// mirroring the reconcile's cleanup on remove and disable. The pass below only
	// walks configured devices, so a state under an unconfigured name would never
	// have its deadline consumed and armRetryTimer would re-arm a zero delay for it
	// forever; this keeps a missed cleanup from becoming a busy loop. It checks the
	// configuration itself rather than the device records, so it does not depend
	// on reconcileRecords, the cleanup it backstops.
	maps.DeleteFunc(a.retries, func(name string, _ *retryState) bool { return !a.enabledInConfig(name) })
	// An encode fault has no deadline, so a stale one costs no busy loop, but a
	// device configured again under that name would inherit its proof.
	maps.DeleteFunc(a.encodeFaults, func(name string, _ encodeFault) bool { return !a.enabledInConfig(name) })
	recovered := false
	attempted := false
	reannounce := false
	for i := range a.cfg.Devices {
		d := &a.cfg.Devices[i]
		st := a.retries[d.Name]
		if st == nil {
			continue
		}
		rt := a.devices[d.Name]
		switch {
		case st.awaitEncode:
			// Woken by a stream's first encoded frame (or by any other due work);
			// only the listed streams all having encoded moves it on.
			if rt == nil || rt.currentState() != mgmtserver.StateServing || !rt.encodedAll(a.faultedPaths(d.Name)) {
				continue
			}
			st.awaitEncode = false
			st.encodeSeen = true
			st.settleAt = now.Add(retrySettle)
		case !st.settleAt.IsZero() && !now.Before(st.settleAt):
			st.settleAt = time.Time{}
			if rt == nil || rt.currentState() != mgmtserver.StateServing {
				continue
			}
			if paths := a.faultedPaths(d.Name); len(paths) > 0 && !st.encodeSeen {
				// The restart has served its window, but only real encode work on
				// each faulted stream proves its fault is gone. rt.awaitEncode was
				// set when the attempt succeeded, so a frame encoded from here on
				// wakes the loop.
				if rt.encodedAll(paths) {
					st.encodeSeen = true
					st.settleAt = now.Add(retrySettle)
				} else {
					st.awaitEncode = true
					if !logAttempt(st.failures) {
						continue
					}
					log.Printf("device %q: serving again; its failure clears once a client plays and encoding succeeds", d.Name)
				}
				continue
			}
			st.recoveredAt = now
			delete(a.encodeFaults, d.Name)
			st.encodeSeen = false
			a.finishRecovery(d.Name, rt)
			recovered = true
		case !st.next.IsZero() && !now.Before(st.next):
			st.next = time.Time{}
			// Defensive: every path that replaces or restarts a down device
			// either deletes its retry state first (startDevice, reconcileRecords)
			// or consumes its pending deadline (restartFaulted zeroes next), so
			// a due retry is expected to find a skipped or failed record here.
			if rt == nil || !isDown(rt.currentState()) {
				continue
			}
			if !attempted {
				// Resolve every id against the current hardware once per pass, so the
				// attempt opens the device where it is now.
				a.refreshHardware(&a.cfg)
				attempted = true
			}
			if a.attemptRetry(d, st, false) {
				reannounce = true
			}
		}
	}
	if attempted {
		a.publish(&a.cfg)
	}
	if recovered || reannounce {
		a.restartAnnounce()
	}
	a.armRetryTimer()
}

// attemptRetry makes one unattended restart attempt. On success the device
// serves at once but its down condition stays active until it has served for
// retrySettle, and after an encode fault until each faulted stream has also
// encoded (see onRetryDue), with the condition raised again as that failure if
// it read as another cause; on a failure scheduleRetry either schedules the
// next attempt or, for a cause a retry cannot fix, ends the retry. The open's own
// log lines, failure and success alike, are silenced on attempts logAttempt
// skips, except the line for a cause that ends the retry (see skipDevice), and
// except on an attempt an event forced (restartFaulted), which logs its own
// line; finishRecovery logs a recovery either way.
//
// It reports whether the advertisement must be rebuilt: a restart that must
// prove an encoder waits, with no deadline, for a client to play the faulted
// stream, so if a rebuild during the outage dropped the device from the
// advertisement, a client that relies on discovery would never find it. A
// device still advertised needs no rebuild, so this costs at most one rebuild
// per rebuild that dropped it, never one per attempt.
func (a *appliance) attemptRetry(d *config.Device, st *retryState, forced bool) (reannounce bool) {
	// Retry n follows failure n. A timer attempt's open logs are gated on the
	// failure it would become, failure n+1, so they appear exactly when
	// scheduleRetry logs that failure. A forced attempt always logs its open.
	loud := forced || a.nextFailureLogged(d.Name)
	if loud && !forced {
		log.Printf("device %q: retry %d", d.Name, st.failures)
	}
	a.quietDown = !loud
	rt := a.openAndStart(d)
	a.quietDown = false
	a.devices[d.Name] = rt
	if rt.currentState() != mgmtserver.StateServing {
		a.scheduleRetry(d)
		return false
	}
	st.settleAt = time.Now().Add(retrySettle)
	paths := a.faultedPaths(d.Name)
	if len(paths) == 0 {
		return false
	}
	if a.downReason[d.Name] != downFailed {
		// The condition stays active while the restart waits for an encode, so
		// its text must say why. After a disconnect, a skip, or an open failure
		// that re-raised it, it still reads as the device being gone or
		// unopenable; raise it again as the failure it now waits out, even over
		// markDown's keep-the-first-cause exception (an open failure then this
		// restart would otherwise leave "Device unavailable" on a serving
		// device). A device already down as failed keeps its condition.
		n := deviceDownOnset(d.Name, "Device failed", fmt.Sprintf("Encoding failed earlier on %s; the device serves again, and this clears once a client plays it and encoding succeeds", strings.Join(paths, ", ")))
		a.replaceDown(d.Name, downFailed, &n)
	}
	// Ask the stages to wake the run loop at a stream's first encoded frame,
	// before any check of the streams' flags (see deviceRuntime.awaitEncode).
	rt.awaitEncode.Store(true)
	return a.prov.discoveryEnabled() && !a.advertised[d.Name]
}

// restartFaulted restarts a down device whose outage had an encode fault, for
// an event that cannot have fixed its encoder (why names it: a hardware change,
// or a config save that left the device's parameters alone). It restarts the
// device at once, as the event would restart any down device, but as a retry
// attempt, so the device keeps its down condition until each faulted stream
// has encoded (see encodeFault) rather than clearing it only for the next PLAY
// to fault again. A device whose retry state was dropped (a disconnect) gets a
// fresh one; any pending backoff attempt is consumed by this one, and a
// failure schedules the next as usual. resetBackoff starts the delays over, as
// a config save does for any device; a hardware change passes false, so a
// hotplug advances the backoff instead. The attempt is always logged: it is
// the event's doing, not the backoff's, so logAttempt does not gate it.
func (a *appliance) restartFaulted(d *config.Device, why string, resetBackoff bool) (reannounce bool) {
	st := a.retries[d.Name]
	if st == nil {
		st = &retryState{}
		a.retries[d.Name] = st
	}
	st.next = time.Time{}
	if resetBackoff {
		st.attempts, st.failures, st.recoveredAt = 0, 0, time.Time{}
	}
	log.Printf("device %q: %s; restarting it, and its failure clears once a client plays the faulted stream and encoding succeeds", d.Name, why)
	return a.attemptRetry(d, st, true)
}

// finishRecovery clears the down condition of a device that is serving again. A
// config-save or hardware-change restart calls it as soon as the open succeeds;
// an unattended retry (including an event-driven restart after an encode
// fault, see restartFaulted) calls it once the restart has served for retrySettle
// (after an encode fault, once each faulted stream has also encoded).
func (a *appliance) finishRecovery(name string, rt *deviceRuntime) {
	// A device is "recovered" only when it comes up from a down state (it could
	// not be opened, or it died after opening), which is exactly while its down
	// condition is active; a healthy param-change restart has none.
	if _, down := a.downReason[name]; !down {
		return
	}
	delete(a.downReason, name)
	log.Printf("device %q recovered: capturing at %d Hz, %d ch%s", name, rt.rate, rt.channels, atAddr(&audio.Hardware{HWAddr: rt.hwAddr, Label: rt.friendlyName}))
	// Clear takes the category, source and key from the stored onset, so only
	// severity, title and message are set here.
	a.notifier.Clear(deviceDownKey(name), notify.Notification{
		Severity: notify.SeverityInfo,
		Title:    "Device recovered",
		Message:  fmt.Sprintf("Capturing again at %d Hz, %d ch", rt.rate, rt.channels),
	})
}

// logAttemptf logs a line from opening a device unless the current unattended
// retry attempt is one that logAttempt skips, so a device that keeps failing,
// or keeps opening and dying, does not log every attempt.
func (a *appliance) logAttemptf(format string, args ...any) {
	if !a.quietDown {
		log.Printf(format, args...)
	}
}

// stopRetries stops the retry timer at shutdown.
func (a *appliance) stopRetries() {
	if a.retryTimer != nil {
		a.retryTimer.Stop()
		a.retryTimer = nil
	}
	a.retryAt = time.Time{}
}
