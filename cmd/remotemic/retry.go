//go:build linux

package main

import (
	"fmt"
	"log"
	"maps"
	"time"

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
	// dies right away (an EIO after open, a deterministic encoder fault) would
	// otherwise publish a clear and a fresh onset, and rebuild the advertisement,
	// on every attempt. Its RTSP paths serve for the whole window.
	retrySettle = 30 * time.Second
	// retryResetAfter is how long a recovered device must serve before a later
	// failure starts the backoff over from the shortest delay. A device that keeps
	// dying soon after each recovery continues from where its backoff was.
	retryResetAfter = 10 * time.Minute
	// retryLogFirst and retryLogEvery bound the per-attempt logging: the first
	// retryLogFirst attempts are logged, then every retryLogEvery-th, which is
	// about once an hour at the capped delay.
	retryLogFirst = 3
	retryLogEvery = 12
)

// retryState tracks the unattended restart of one down device. It exists from
// the device's first retryable failure until the device is removed, disabled,
// restarted by a config save or hardware change, or goes down for a cause a
// retry cannot fix.
type retryState struct {
	// attempts counts consecutive failures since the backoff last reset; it picks
	// the delay before the next attempt.
	attempts int
	// next is when the next restart attempt is due; zero while none is pending.
	next time.Time
	// settleAt is when a device restarted by a retry counts as recovered; zero
	// unless it is serving and waiting out retrySettle.
	settleAt time.Time
	// recoveredAt is when the device last completed a settle, used to reset the
	// backoff once it has served for retryResetAfter.
	recoveredAt time.Time
}

// backoffDelay returns the delay before the attempt that follows the given
// number of consecutive failures (at least 1).
func backoffDelay(failures int) time.Duration {
	i := min(max(failures, 1), len(retryBackoff)) - 1
	return retryBackoff[i]
}

// logAttempt reports whether the given attempt number is one to log, so a
// permanently failing device does not flood the log.
func logAttempt(n int) bool { return n <= retryLogFirst || n%retryLogEvery == 0 }

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
	now := time.Now()
	st := a.retries[name]
	if st == nil {
		st = &retryState{}
		a.retries[name] = st
	}
	if !st.recoveredAt.IsZero() && now.Sub(st.recoveredAt) >= retryResetAfter {
		st.attempts = 0
	}
	st.recoveredAt = time.Time{}
	st.settleAt = time.Time{}
	st.attempts++
	delay := backoffDelay(st.attempts)
	st.next = now.Add(delay)
	if logAttempt(st.attempts) {
		log.Printf("device %q: retrying in %s (failure %d)", name, delay, st.attempts)
	}
	a.armRetryTimer()
}

// dropRetry ends any unattended restart of the device, for a down cause a retry
// cannot fix (a disconnect is brought back by the hardware-change retry).
func (a *appliance) dropRetry(name string) {
	delete(a.retries, name)
	a.armRetryTimer()
}

// armRetryTimer (re)arms the single retry timer for the earliest pending retry
// or settle deadline. The timer only signals retryDue; the run loop does the
// work in onRetryDue, so the appliance state stays single-goroutine.
func (a *appliance) armRetryTimer() {
	if a.retryTimer != nil {
		a.retryTimer.Stop()
		a.retryTimer = nil
	}
	var due time.Time
	for _, st := range a.retries {
		for _, t := range [...]time.Time{st.next, st.settleAt} {
			if !t.IsZero() && (due.IsZero() || t.Before(due)) {
				due = t
			}
		}
	}
	if due.IsZero() {
		return
	}
	a.retryTimer = time.AfterFunc(time.Until(due), func() {
		select {
		case a.retryDue <- struct{}{}:
		default:
		}
	})
}

// onRetryDue runs on the run loop when the retry timer fires. It completes the
// settle of every restarted device that has served for retrySettle, and makes
// one restart attempt for every down device whose backoff has elapsed. A stale
// or early signal does nothing, since each deadline is checked against the
// clock.
func (a *appliance) onRetryDue() {
	now := time.Now()
	// The pass below walks the configured devices, so a retry state under any
	// other name would never have its deadline consumed, and armRetryTimer would
	// re-arm a zero delay for it forever. The reconcile drops such state when a
	// device is removed or disabled; dropping it here too keeps a missed cleanup
	// from turning into a busy loop on the run loop.
	enabled := make(map[string]bool, len(a.cfg.Devices))
	for i := range a.cfg.Devices {
		if a.cfg.Devices[i].IsEnabled() {
			enabled[a.cfg.Devices[i].Name] = true
		}
	}
	maps.DeleteFunc(a.retries, func(name string, _ *retryState) bool { return !enabled[name] })
	recovered := false
	attempted := false
	for i := range a.cfg.Devices {
		d := a.cfg.Devices[i]
		st := a.retries[d.Name]
		if st == nil {
			continue
		}
		rt := a.devices[d.Name]
		switch {
		case !st.settleAt.IsZero() && !now.Before(st.settleAt):
			st.settleAt = time.Time{}
			if rt == nil || rt.currentState() != mgmtserver.StateServing {
				continue
			}
			st.recoveredAt = now
			a.finishRecovery(d.Name, rt)
			recovered = true
		case !st.next.IsZero() && !now.Before(st.next):
			st.next = time.Time{}
			if rt == nil || !d.IsEnabled() {
				continue
			}
			if s := rt.currentState(); s != mgmtserver.StateSkipped && s != mgmtserver.StateFailed {
				continue
			}
			if !attempted {
				// Resolve every id against the current hardware once per pass, so the
				// attempt opens the device where it is now.
				a.refreshHardware(&a.cfg)
				attempted = true
			}
			a.attemptRetry(&d, st)
		}
	}
	if attempted {
		a.publish(&a.cfg)
	}
	if recovered {
		a.restartAnnounce()
	}
	a.armRetryTimer()
}

// attemptRetry makes one unattended restart attempt. On success the device
// serves at once but its down condition stays active until it has served for
// retrySettle (see onRetryDue); on failure the next attempt is scheduled. Failure
// logs inside the open are silenced on attempts logAttempt skips.
func (a *appliance) attemptRetry(d *config.Device, st *retryState) {
	n := st.attempts + 1
	if logAttempt(n) {
		log.Printf("device %q: retry attempt %d", d.Name, n)
	}
	a.quietDown = !logAttempt(n)
	rt := a.openAndStart(d)
	a.quietDown = false
	a.devices[d.Name] = rt
	if rt.currentState() != mgmtserver.StateServing {
		a.scheduleRetry(d)
		return
	}
	st.settleAt = time.Now().Add(retrySettle)
}

// finishRecovery clears the down condition of a device that is serving again. A
// config-save or hardware-change restart calls it as soon as the open succeeds;
// an unattended retry calls it once the restart has served for retrySettle.
func (a *appliance) finishRecovery(name string, rt *deviceRuntime) {
	// A device is "recovered" only when it comes up from a down state (it could
	// not be opened, or it died after opening), which is exactly while its down
	// condition is active; a healthy param-change restart has none.
	if _, down := a.downReason[name]; !down {
		return
	}
	delete(a.downReason, name)
	log.Printf("device %q recovered", name)
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
}
