//go:build linux

package main

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// geometryLadder is the capture geometries a device that keeps falling short of
// its sample rate is reopened with, one step per geometryShortfalls shortfalls.
// Step 0 leaves both fields zero, so it is the library default (20 ms x 4)
// byte for byte. The ring grows first (step 1: no added period latency or sender
// report error), then the period doubles (step 2), then the ring grows again.
// The period stops at 40 ms: a period reaches a stream's subscriber queue as a
// burst of frames (the queue holds as little as 0.16 s, see subQueueDepth in
// internal/rtspserver/feed.go), and the RTCP sender report pairs one capture
// stamp per period with the last packet, so its mapping error grows with the
// period. The top step is 40 ms x 16, a 640 ms buffer.
var geometryLadder = [...]struct {
	period  time.Duration
	periods int
}{
	{},
	{20 * time.Millisecond, 8},
	{40 * time.Millisecond, 8},
	{40 * time.Millisecond, 16},
}

const (
	// geometryShortfalls is how many shortfalls at one step raise the next.
	geometryShortfalls = 2
	// geometryStableRun is how long a device must have served since its open
	// for its escalation to be forgotten at its next death or open.
	geometryStableRun = time.Hour
)

// geometryFor returns the capture geometry for ladder step at rate: zero for
// step 0 (library default), otherwise the step's period in frames and its
// period count. rate is validated to 8000..384000, so the frame count cannot
// overflow.
func geometryFor(rate, step int) audio.Geometry {
	step = max(0, min(step, len(geometryLadder)-1))
	l := geometryLadder[step]
	if l.periods == 0 {
		return audio.Geometry{}
	}
	return audio.Geometry{PeriodFrames: int(int64(rate) * l.period.Milliseconds() / 1000), Periods: l.periods}
}

// describeGeometry words a geometry at rate for logs and notifications; the
// zero Geometry is the library default.
func describeGeometry(g audio.Geometry, rate int) string {
	if g.Periods == 0 || rate <= 0 {
		return "the default capture period"
	}
	ms := int64(g.PeriodFrames) * 1000 / int64(rate)
	return fmt.Sprintf("a %d ms period and %d periods (%d ms buffer)", ms, g.Periods, ms*int64(g.Periods))
}

// geometryFallback reports whether a failed escalated open should be retried one
// step lower. Busy (another process holds the device), gone and permanent
// errors say nothing about the geometry, so they do not lower the ceiling; any
// other failure, whatever the library calls it, goes back to what worked.
func geometryFallback(err error) bool {
	return !errors.Is(err, capture.ErrDeviceInUse) && !errors.Is(err, capture.ErrDeviceGone) && !permanentOpenError(err)
}

// geometryState is one device's capture geometry escalation: the ladder step
// its next open asks for, and the identity it was earned under. It lives in
// memory only and is touched only on the run loop.
type geometryState struct {
	// device, rate and channels are the capture id, sample rate and stream
	// channel union the step was earned under (the last two set the ring's
	// size); a device opened with any of them changed starts over.
	device   string
	rate     int
	channels []int
	// step is the ladder step the next open asks for. ceiling is the highest
	// step that has not failed to open; escalation stops there.
	step, ceiling int
	// shortfalls counts shortfalls at step since it was entered.
	shortfalls int
}

// geometryKey is the notification condition key for a device's raised capture
// period. Every site that raises, rewrites, clears or resolves it goes through
// here.
func geometryKey(name string) string { return "device:" + name + ":capture-period" }

// geometryOnset builds the warning onset for a device capturing with a larger
// period than the default.
func geometryOnset(name string, g audio.Geometry, rate int) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategoryDevice,
		Key:      geometryKey(name),
		Source:   name,
		Title:    "Capture period raised",
		Message: fmt.Sprintf("Capture fell short of %d Hz repeatedly, so the device now captures with %s instead of the default; this adds latency. It returns to the default once a run of the device ends after an hour of serving.",
			rate, describeGeometry(g, rate)),
	}
}

// geometryStep returns the ladder step dev's next open asks for. It is the
// first thing an open does, before any refusal, so a state earned under a
// different capture id, rate or channel set is dropped even when the new
// configuration cannot open at all.
func (a *appliance) geometryStep(dev *config.Device) int {
	st := a.geometry[dev.Name]
	if st == nil {
		return 0
	}
	if st.device != dev.Device || st.rate != dev.Rate || !slices.Equal(st.channels, dev.StreamChannelUnion()) {
		a.dropGeometry(dev.Name, "the device's capture settings changed")
		return 0
	}
	return st.step
}

// geometryRunEnded forgets a device's escalation when the run that just ended
// (a death or a deliberate stop) had served for geometryStableRun, so the next
// open uses the default. A state that never escalated is dropped silently.
func (a *appliance) geometryRunEnded(rt *deviceRuntime) {
	name := rt.dev.Name
	st := a.geometry[name]
	if st == nil || rt.started.IsZero() {
		return
	}
	served := time.Since(rt.started)
	if served < geometryStableRun {
		return
	}
	delete(a.geometry, name)
	if st.step == 0 {
		return
	}
	log.Printf("device %q: served %s at %s; its next open uses the default capture period", name, served.Round(time.Minute), describeGeometry(rt.geometryOrStep(st.step), rt.rate))
	a.notifier.Clear(geometryKey(name), notify.Notification{
		Severity: notify.SeverityInfo,
		Title:    "Capture period back to default",
		Message:  "The device served for an hour, so its next open uses the default capture period.",
	})
}

// geometryShortfall counts a shortfall of a device that stays on the backoff
// retry, and raises the step its next open asks for after geometryShortfalls of
// them at one step. It raises no notification: the condition says what is in
// effect, so only an open that succeeded at the step raises it (geometryOpened).
func (a *appliance) geometryShortfall(dev *config.Device) {
	st := a.geometry[dev.Name]
	if st == nil {
		st = &geometryState{device: dev.Device, rate: dev.Rate, channels: dev.StreamChannelUnion(), ceiling: len(geometryLadder) - 1}
		a.geometry[dev.Name] = st
	}
	if st.step >= st.ceiling {
		return
	}
	st.shortfalls++
	if st.shortfalls < geometryShortfalls {
		return
	}
	st.step++
	st.shortfalls = 0
	log.Printf("device %q: fell short of its sample rate %d times at %s; its next open asks for %s", dev.Name, geometryShortfalls,
		describeGeometry(geometryFor(dev.Rate, st.step-1), dev.Rate), describeGeometry(geometryFor(dev.Rate, st.step), dev.Rate))
}

// geometryOpened records the outcome of an open that asked for step asked and
// succeeded at step got (lower after a fallback, firstErr being the failure that
// started it). It raises or rewrites the capture-period condition with the
// geometry the driver granted, or clears it when the device ended up at the
// default.
func (a *appliance) geometryOpened(name string, rt *deviceRuntime, asked, got int, firstErr error) {
	st := a.geometry[name]
	if st == nil {
		return
	}
	if got < asked {
		log.Printf("device %q: opening with %s failed (%v); using %s", name,
			describeGeometry(geometryFor(rt.rate, asked), rt.rate), firstErr, describeGeometry(geometryFor(rt.rate, got), rt.rate))
		st.step, st.ceiling, st.shortfalls = got, got, 0
	}
	if got == 0 {
		a.notifier.Clear(geometryKey(name), notify.Notification{
			Severity: notify.SeverityInfo,
			Title:    "Capture period back to default",
			Message:  "The larger capture period did not open; capture uses the default period.",
		})
		return
	}
	n := geometryOnset(name, rt.geometryOrStep(got), rt.rate)
	if !a.notifier.Onset(n) && a.updater != nil {
		a.updater.Update(n.Key, n.Title, n.Message)
	}
}

// geometryOrStep is the geometry the driver granted this runtime, or ladder
// step's request when its source cannot report one.
func (rt *deviceRuntime) geometryOrStep(step int) audio.Geometry {
	if rt.geometry.Periods != 0 {
		return rt.geometry
	}
	return geometryFor(rt.rate, step)
}

// dropGeometry forgets name's geometry state and resolves its condition, for a
// device that was removed, disabled or reconfigured. Resolve is a no-op when the
// condition is not active.
func (a *appliance) dropGeometry(name, reason string) {
	delete(a.geometry, name)
	a.notifier.Resolve(geometryKey(name), reason)
}

// pruneGeometry drops the state of every name that is not an enabled configured
// device, backstopping reconcileRecords so a device configured again under that
// name cannot inherit a step.
func (a *appliance) pruneGeometry() {
	for name := range maps.Clone(a.geometry) {
		if !a.enabledInConfig(name) {
			a.dropGeometry(name, "device removed from the configuration")
		}
	}
}
