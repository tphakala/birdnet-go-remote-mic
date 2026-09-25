//go:build linux

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

var errTestEncode = errors.New("encode failed")

// pathMoth and pathBat are the stream paths the encode-fault tests play.
const (
	pathMoth = "/m"
	pathBat  = "/bat"
)

// scriptedStage stands in for a stream's encode stage. It drains its source and
// encodes only when the test "plays" it: a true on play faults the encode, a
// false encodes one frame and keeps running, so a later play can fault it.
type scriptedStage struct{ play chan bool }

func (s scriptedStage) Run(src audio.Source, _ pipeline.Gate, emit func(pipeline.Frame) error) error {
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for {
			if _, err := src.Read(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case fault := <-s.play:
			if fault {
				return errTestEncode
			}
			if err := emit(pipeline.Frame{}); err != nil {
				return err
			}
		case <-ended:
			return nil
		}
	}
}

// scriptedStages wraps the fake opener so every opened stream runs its own
// scriptedStage, and returns a function yielding the play channel of the stream
// at path in the most recently opened runtime. That function fails the test
// when the runtime is not the device's current, serving one: a send to the
// stage of a runtime that already stopped would block the test forever instead
// of failing it.
func scriptedStages(t *testing.T, app *appliance, log *fakeOpenLog) func(path string) chan bool {
	t.Helper()
	// The source reports capture.ErrClosed once closed, as the real capture does,
	// so a stage fault must win over the fan-out's error to be seen.
	next := fakeOpenerWith(log, func(rate, channels int) audio.Source {
		src := newBlockingSource(rate, channels)
		src.closedErr = capture.ErrClosed
		return src
	})
	var latest map[string]chan bool
	var latestRT *deviceRuntime
	app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
		rt, err := next(dev, hub)
		if err != nil {
			return rt, err
		}
		latest = make(map[string]chan bool, len(rt.streams))
		for _, sr := range rt.streams {
			ch := make(chan bool)
			latest[sr.stream.Path] = ch
			sr.stage = scriptedStage{play: ch}
		}
		latestRT = rt
		return rt, nil
	}
	return func(path string) chan bool {
		t.Helper()
		rt := app.devices[latestRT.dev.Name]
		if rt != latestRT || rt.currentState() != mgmtserver.StateServing {
			t.Fatalf("play %s: the latest runtime is not the device's serving one (state %s)", path, rt.currentState())
		}
		return latest[path]
	}
}

// TestRetryAfterEncodeFaultSettlesOnlyAfterEncoding is the #97 case: an encode
// fault surfaces only while a client plays, so a restart that merely serves with
// no client must not count as recovered (that would clear the condition and
// rebuild mDNS, only for the next PLAY to fault again). The condition stays
// raised across restarts and fresh faults, and clears once a restart has
// encoded for retrySettle.
func TestRetryAfterEncodeFaultSettlesOnlyAfterEncoding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})
		genBefore := app.announceGen

		// A client plays and the encode faults: the device fails and is retried.
		play(pathMoth) <- true
		runFor(t, app, 2*time.Minute)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the retry", s)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 while no client has proved the encoder", got)
		}
		if got := app.announceGen - genBefore; got != 0 {
			t.Errorf("announcement rebuilds = %d, want 0 before recovery", got)
		}

		// The next client faults again: still the same outage, no clear.
		play(pathMoth) <- true
		runFor(t, app, 2*time.Minute)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 after a second fault", got)
		}

		// A client plays and encoding succeeds: the condition clears retrySettle
		// after the first encoded frame, not before.
		play(pathMoth) <- false
		runFor(t, app, retrySettle-time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 before the encode has lasted retrySettle", got)
		}
		runFor(t, app, 2*time.Second)
		if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1", got)
		}
		if got := app.announceGen - genBefore; got != 1 {
			t.Errorf("announcement rebuilds = %d, want 1 (on recovery only)", got)
		}

		// A later capture failure is a new outage with no encode fault: its
		// restart settles on time alone, with no client playing. (The backoff
		// carries on from the earlier outage, since the device recovered less than
		// retryResetAfter ago.)
		killDevice(t, app, log, "moth", errTestEIO)
		runFor(t, app, 2*time.Minute)
		if got := countDown(t, app, "moth", notify.KindClear); got != 2 {
			t.Errorf("down clears = %d, want 2: the capture-fault outage settles without an encode", got)
		}
	})
}

// TestRetryEncodeFaultDuringSettleNeedsNewEncode pins that an encode seen by one
// restart does not carry over: a fault during the settle that followed it
// starts a new restart, which must itself encode before the condition clears.
func TestRetryEncodeFaultDuringSettleNeedsNewEncode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})

		play(pathMoth) <- true // fault 1
		runFor(t, app, backoffDelay(1)+time.Second)
		play(pathMoth) <- false // this restart encodes; its settle starts at the first deadline
		runFor(t, app, retrySettle+time.Second)
		play(pathMoth) <- true // fault 2, inside that settle
		// The next restart serves well past its own window with no client.
		runFor(t, app, 5*time.Minute)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0: the earlier restart's encode must not prove this one", got)
		}
		play(pathMoth) <- false
		runFor(t, app, retrySettle+time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 once the new restart has encoded", got)
		}
	})
}

// TestRetryAfterEncodeFaultCountsEncodeInsideWindow pins the other ordering: a
// client that plays during the first settle window (before the deadline) is
// seen at the deadline, and the settle then counts retrySettle from there.
func TestRetryAfterEncodeFaultCountsEncodeInsideWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})

		play(pathMoth) <- true
		// The first retry is due backoffDelay(1) after the fault; let it open.
		runFor(t, app, backoffDelay(1)+time.Second)
		play(pathMoth) <- false
		// Past the first settle deadline (retrySettle after the restart) but not
		// past retrySettle after it: the encode is seen at that deadline and the
		// settle counts from there, so nothing clears yet.
		runFor(t, app, retrySettle+time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 at the first deadline", got)
		}
		runFor(t, app, retrySettle)
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 once the restart has encoded", got)
		}
	})
}

// TestRetryCaptureFaultSettlesOnTime pins that a restart after a capture fault
// (no stream's stage faulted) settles on time alone, retrySettle after it
// opens: the encode proof applies only to an outage with an encode fault.
func TestRetryCaptureFaultSettlesOnTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})
		// The first retry is due backoffDelay(1) after the failed open and succeeds.
		runFor(t, app, backoffDelay(1)+retrySettle-time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 before the restart has served retrySettle", got)
		}
		runFor(t, app, 2*time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 retrySettle after the restart, with no client", got)
		}
	})
}

// TestRetryEncodeFaultNeedsFaultedStreamToEncode pins the multi-stream case: on
// a device fanning out two streams, the stream that faulted must itself encode
// before the condition clears. An always-played healthy stream encoding proves
// nothing about the faulted one; settling on it would clear and rebuild mDNS
// only for the next PLAY of the faulted stream to fault again.
func TestRetryEncodeFaultNeedsFaultedStreamToEncode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		dev := testDevice("moth", idMoth, pathMoth, 48000)
		dev.Streams = append(dev.Streams, config.Stream{Path: pathBat, Mode: config.ModePCM, Channels: []int{1}})
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		genBefore := app.announceGen

		// The bat stream faults on PLAY; the device fails and is retried.
		play(pathBat) <- true
		runFor(t, app, backoffDelay(1)+time.Second)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the retry", s)
		}

		// Let the restart serve its first window, so it waits for an encode; then
		// only the healthy stream plays, and its first frame wakes the run loop:
		// no clear, however long it runs.
		runFor(t, app, retrySettle)
		play(pathMoth) <- false
		runFor(t, app, 3*retrySettle)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 while only the healthy stream has encoded", got)
		}
		if got := app.announceGen - genBefore; got != 0 {
			t.Errorf("announcement rebuilds = %d, want 0 before recovery", got)
		}

		// The faulted stream plays and encodes: it clears retrySettle later.
		play(pathBat) <- false
		runFor(t, app, retrySettle-time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 before the faulted stream's encode has lasted retrySettle", got)
		}
		runFor(t, app, 2*time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 once the faulted stream has encoded", got)
		}
	})
}

// TestRetryEncodeFaultSurvivesHardwareChange pins that a hotplug does not
// bypass the encode proof. A hardware change restarts every down device (but a
// card-index entry), and a config-save style restart clears the condition at
// once; an encode-faulted device restarted that way would clear on an
// unrelated hotplug and fault again at the next PLAY. It must instead be
// restarted as a retry attempt, keeping its condition until the faulted stream
// encodes.
func TestRetryEncodeFaultSurvivesHardwareChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})

		play(pathMoth) <- true
		// Let the fault land, but not the first retry.
		runFor(t, app, backoffDelay(1)/2)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateFailed {
			t.Fatalf("moth state = %s, want failed after the encode fault", s)
		}

		// Some other device is plugged in: the hardware change restarts moth.
		app.retryDown()
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the hardware change", s)
		}
		// The hotplug attempt consumed the pending backoff attempt, so the timer
		// is armed for the settle, not for a retry of a device already serving.
		if st := app.retries["moth"]; st == nil || !st.next.IsZero() || !app.retryAt.Equal(st.settleAt) {
			t.Errorf("after the hotplug restart: retry state %+v, timer at %v; want no pending attempt and the timer at the settle", st, app.retryAt)
		}
		runFor(t, app, 5*time.Minute)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0: a hotplug proves nothing about the encoder", got)
		}

		play(pathMoth) <- false
		runFor(t, app, retrySettle+time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 once the faulted stream has encoded", got)
		}
	})
}

// TestRetryEncodeWaitReannouncesDroppedDevice pins discovery for a restart that
// waits for an encode. The wait has no deadline and needs a client to play the
// stream, so a device that another device's rebuild dropped from the mDNS
// advertisement during its outage must be advertised again when it serves, or
// a client that relies on discovery never finds it and the wait never ends. A
// device still in the advertisement costs no rebuild.
func TestRetryEncodeWaitReannouncesDroppedDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})

		play(pathMoth) <- true
		runFor(t, app, backoffDelay(1)/2)
		// Another device's recovery rebuilds the advertisement while moth is down.
		app.restartAnnounce()
		if app.advertised["moth"] {
			t.Fatal("a rebuild while moth is down still advertises it")
		}
		gen := app.announceGen

		runFor(t, app, backoffDelay(1))
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the retry", s)
		}
		if got := app.announceGen - gen; got != 1 {
			t.Errorf("announcement rebuilds = %d, want 1 when the restart serves", got)
		}
		if !app.advertised["moth"] {
			t.Error("moth is not advertised while it waits for a client to encode")
		}

		// A fault while still advertised: the next restart needs no rebuild.
		play(pathMoth) <- true
		runFor(t, app, 2*time.Minute)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the second retry", s)
		}
		if got := app.announceGen - gen; got != 1 {
			t.Errorf("announcement rebuilds = %d, want still 1: moth never left the advertisement", got)
		}
	})
}

// TestRetryEncodeFaultSurvivesReplug pins that unplugging the faulted device
// itself does not clear its encode proof. A disconnect ends the backoff retry
// (the hardware change brings the device back), but the faulted stream is the
// same stream on the same encoder once it is plugged in again, so the replug
// restarts it as a retry attempt that keeps the condition until the stream
// encodes, rather than clearing it only for the next PLAY to fault again.
func TestRetryEncodeFaultSurvivesReplug(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})

		play(pathMoth) <- true
		// The retry opens and waits for a client to prove the encoder.
		runFor(t, app, backoffDelay(1)+time.Second)
		// The device is unplugged, then plugged in again.
		killDevice(t, app, log, "moth", capture.ErrDeviceGone)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateFailed {
			t.Fatalf("moth state = %s, want failed after the disconnect", s)
		}
		// A replug whose open fails keeps the open failure as the condition: the
		// encode-wait text is only for a device that serves again.
		failOpenTimes(app, log, 1)
		app.retryDown()
		for _, n := range applianceCenter(t, app).Active() {
			if n.Key == deviceDownKey("moth") && n.Title == titleFailed {
				t.Errorf("active moth condition after a failed replug = %+v, want the open failure", n)
			}
		}
		runFor(t, app, backoffDelay(1)+time.Second)
		app.retryDown()
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the replug", s)
		}
		// The active condition no longer says the device is disconnected: it
		// serves again and waits for the faulted stream to prove its encoder.
		var active []notify.Notification
		for _, n := range applianceCenter(t, app).Active() {
			if n.Key == deviceDownKey("moth") {
				active = append(active, n)
			}
		}
		if len(active) != 1 || active[0].Title != titleFailed || !strings.Contains(active[0].Message, pathMoth) {
			t.Errorf("active moth condition after the replug = %+v, want one \"Device failed\" naming %s", active, pathMoth)
		}
		// Each change of cause resolves the previous condition (counted as a
		// clear), so count clears from the replug on.
		base := countDown(t, app, "moth", notify.KindClear)
		runFor(t, app, 5*time.Minute)
		if got := countDown(t, app, "moth", notify.KindClear) - base; got != 0 {
			t.Fatalf("down clears since the disconnect = %d, want 0: a replug proves nothing about the encoder", got)
		}

		play(pathMoth) <- false
		runFor(t, app, retrySettle+time.Second)
		if got := countDown(t, app, "moth", notify.KindClear) - base; got != 1 {
			t.Errorf("down clears since the disconnect = %d, want 1 once the faulted stream has encoded", got)
		}
	})
}

// TestRetryEncodeFaultEventRestartRaisesEncodeWait pins the condition text of
// an event-driven restart after a disconnect of the faulted device: a replug or
// an unchanged config save whose open succeeds at once serves the device while
// it waits for the faulted stream to encode, so the active condition must say
// that, not that the device is disconnected.
func TestRetryEncodeFaultEventRestartRaisesEncodeWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart func(*appliance, config.Device)
	}{
		{name: "replug", restart: func(app *appliance, _ config.Device) { app.retryDown() }},
		{name: "unchanged save", restart: func(app *appliance, d config.Device) {
			app.reconcile(&config.Config{Devices: []config.Device{d}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, log, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				play := scriptedStages(t, app, log)
				dev := testDevice("moth", idMoth, pathMoth, 48000)
				app.reconcile(&config.Config{Devices: []config.Device{dev}})

				play(pathMoth) <- true
				runFor(t, app, backoffDelay(1)+time.Second)
				killDevice(t, app, log, "moth", capture.ErrDeviceGone)
				tc.restart(app, dev)
				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving after the %s", s, tc.name)
				}
				var active []notify.Notification
				for _, n := range applianceCenter(t, app).Active() {
					if n.Key == deviceDownKey("moth") {
						active = append(active, n)
					}
				}
				if len(active) != 1 || active[0].Title != titleFailed || !strings.Contains(active[0].Message, pathMoth) {
					t.Errorf("active moth condition after the %s = %+v, want one \"Device failed\" naming %s", tc.name, active, pathMoth)
				}
			})
		})
	}
}

// TestRetryEncodeFaultConfigSave pins how a config save treats a device down
// after an encode fault. A save that leaves the device's capture and stream
// parameters alone (here a device's quiet-alert opt-out) cannot have fixed the
// encoder, so it restarts the device at once but keeps its condition until the
// faulted stream encodes. A save that changes them builds a different stream,
// so the old fault proves nothing and the device clears as soon as it serves.
func TestRetryEncodeFaultConfigSave(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*config.Device)
		wantClear int
	}{
		// A device field that is not a capture or stream parameter changes, so the
		// save is not merely identical to the running config.
		{name: "unrelated save", change: func(d *config.Device) { d.QuietAlert = new(false) }, wantClear: 0},
		{name: "parameter change", change: func(d *config.Device) { d.Rate = 96000 }, wantClear: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, log, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				play := scriptedStages(t, app, log)
				dev := testDevice("moth", idMoth, pathMoth, 48000)
				app.reconcile(&config.Config{Devices: []config.Device{dev}})

				play(pathMoth) <- true
				runFor(t, app, backoffDelay(1)/2)
				if s := app.devices["moth"].currentState(); s != mgmtserver.StateFailed {
					t.Fatalf("moth state = %s, want failed after the encode fault", s)
				}
				tc.change(&dev)
				app.reconcile(&config.Config{Devices: []config.Device{dev}})
				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving right after the save", s)
				}
				runFor(t, app, 5*time.Minute)
				if got := countDown(t, app, "moth", notify.KindClear); got != tc.wantClear {
					t.Fatalf("down clears = %d, want %d", got, tc.wantClear)
				}
				if tc.wantClear == 1 {
					// The old fault is forgotten, not merely cleared: a later capture
					// fault is a new outage whose restart settles on time alone.
					killDevice(t, app, log, "moth", errTestEIO)
					runFor(t, app, 2*time.Minute)
					if got := countDown(t, app, "moth", notify.KindClear); got != 2 {
						t.Errorf("down clears = %d, want 2: the capture-fault outage settles without an encode", got)
					}
					return
				}
				play(pathMoth) <- false
				runFor(t, app, retrySettle+time.Second)
				if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
					t.Errorf("down clears = %d, want 1 once the faulted stream has encoded", got)
				}
			})
		})
	}
}

// TestRetryEncodeFaultConfigSaveRestartsBackoff pins that a config save starts
// the delays over even for an encode-faulted device it restarts as a retry
// attempt: an operator who saves while another process still holds the device
// gets the next attempt 5 s later, not after the long delay the earlier faults
// had built up.
func TestRetryEncodeFaultConfigSaveRestartsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		dev := testDevice("moth", idMoth, pathMoth, 48000)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})

		// Build up the backoff: each retry opens and faults again at the next PLAY.
		for n := 1; n <= 3; n++ {
			play(pathMoth) <- true
			runFor(t, app, backoffDelay(n)+time.Second)
		}
		play(pathMoth) <- true
		runFor(t, app, time.Second)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateFailed {
			t.Fatalf("moth state = %s, want failed after the last fault", s)
		}
		// The save's attempt finds the device busy.
		out := captureLog(t)
		failOpenTimes(app, log, 1)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
			t.Fatalf("moth state = %s, want skipped after the busy save", s)
		}
		// The failure count starts over too, so the failure is logged as the
		// first of a new outage.
		if want := fmt.Sprintf(`device "moth": retrying in %s (failure 1)`, backoffDelay(1)); !strings.Contains(out.String(), want) {
			t.Errorf("log = %q, want %q", out.String(), want)
		}
		runFor(t, app, backoffDelay(1)+time.Second)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Errorf("moth state = %s %s after the save, want serving: the save must start the delays over", s, backoffDelay(1)+time.Second)
		}
	})
}

// TestRetryEncodeFaultCardIndexSaveClears pins that a card-index device keeps
// the behaviour it had before the encode proof outlived the retry state: it is
// never retried unattended, so it records no encode fault, and the config save
// its condition points the operator at restarts it and clears the condition as
// soon as it opens.
func TestRetryEncodeFaultCardIndexSaveClears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		dev := testDevice("moth", "hw:1", pathMoth, 48000)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})

		play(pathMoth) <- true
		runFor(t, app, time.Minute)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateFailed {
			t.Fatalf("moth state = %s, want failed: a card-index device is not retried unattended", s)
		}
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the save", s)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 as soon as the save opens the device", got)
		}
	})
}

// TestRetryEncodeFaultDroppedOnDisableAndRemove pins that disabling or
// removing a device ends its encode proof: the operator ended that outage, so
// enabling or adding the device again with the same parameters is a fresh
// start that clears nothing later and carries no retry, not a restart still
// waiting for the old stream to encode.
func TestRetryEncodeFaultDroppedOnDisableAndRemove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(config.Device) *config.Config
	}{
		{name: "disable", leave: func(d config.Device) *config.Config {
			d.Enabled = new(false)
			return &config.Config{Devices: []config.Device{d}}
		}},
		{name: "remove", leave: func(config.Device) *config.Config { return &config.Config{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, log, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				play := scriptedStages(t, app, log)
				dev := testDevice("moth", idMoth, pathMoth, 48000)
				app.reconcile(&config.Config{Devices: []config.Device{dev}})

				play(pathMoth) <- true
				runFor(t, app, backoffDelay(1)/2)
				if len(app.faultedPaths("moth")) != 1 {
					t.Fatalf("precondition: faulted %v, want the encode fault on record", app.faultedPaths("moth"))
				}
				app.reconcile(tc.leave(dev))
				app.reconcile(&config.Config{Devices: []config.Device{dev}})
				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving once it is back", s)
				}
				if app.retrying("moth") || len(app.faultedPaths("moth")) != 0 {
					t.Errorf("moth back after %s: retrying %v, faulted %v; want a fresh start with neither", tc.name, app.retrying("moth"), app.faultedPaths("moth"))
				}
			})
		})
	}
}

// TestRetryEncodeFaultHotplugAttemptFails pins a hardware-change restart of an
// encode-faulted device whose open fails: the failure schedules the next
// backoff attempt (the hotplug consumed the pending one, so without a re-arm
// the device would stay down until the next hotplug), and the attempt is
// logged even late in an outage, where a timer-driven attempt is quiet.
func TestRetryEncodeFaultHotplugAttemptFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(t, app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, pathMoth, 48000)}})

		// Fault past the failures logAttempt logs, each retry opening and
		// faulting again at the next PLAY.
		for n := 1; n <= retryLogFirst+1; n++ {
			play(pathMoth) <- true
			runFor(t, app, backoffDelay(n)+time.Second)
		}
		play(pathMoth) <- true
		runFor(t, app, time.Second)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateFailed {
			t.Fatalf("moth state = %s, want failed after the last fault", s)
		}
		out := captureLog(t)
		failOpenTimes(app, log, 1)
		app.retryDown()
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
			t.Fatalf("moth state = %s, want skipped after the failed hotplug attempt", s)
		}
		if !strings.Contains(out.String(), `device "moth": hardware changed`) {
			t.Errorf("log = %q, want the hotplug attempt logged", out.String())
		}
		if !strings.Contains(out.String(), `skipping device "moth"`) {
			t.Errorf("log = %q, want the failed open's reason logged", out.String())
		}
		// The event names itself, so the timer's "retry N" line is not logged.
		if strings.Contains(out.String(), `device "moth": retry `) {
			t.Errorf("log = %q, want no timer retry line for a hotplug attempt", out.String())
		}
		if app.retryAt.IsZero() {
			t.Fatal("no retry armed after the failed hotplug attempt")
		}
		// A hotplug advances the backoff rather than starting it over.
		// Five encode faults and the failed hotplug attempt make six failures, the
		// capped delay (synctest time does not move within this pass).
		if got, want := time.Until(app.retryAt), backoffDelay(len(retryBackoff)); got != want {
			t.Errorf("next attempt in %s, want the advanced delay %s", got, want)
		}
		runFor(t, app, backoffDelay(len(retryBackoff))+time.Second)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the next backoff attempt", s)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Errorf("down clears = %d, want 0 until the faulted stream encodes", got)
		}
	})
}

// TestRestartAnnounceWithDiscoveryOffForgetsAdvertised pins where the
// advertised set is reset: a rebuild with discovery off advertises nothing, so
// it must also forget what was advertised, or a restart waiting for an encode
// would read as still advertised and skip its re-announce once discovery is
// turned back on.
func TestRestartAnnounceWithDiscoveryOffForgetsAdvertised(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, _, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		dev := testDevice("moth", idMoth, pathMoth, 48000)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if !app.advertised["moth"] {
			t.Fatal("precondition: moth is not advertised with discovery on")
		}
		app.reconcile(&config.Config{Devices: []config.Device{dev}, Discovery: config.Discovery{Enabled: new(false)}})
		if app.advertised["moth"] {
			t.Error("moth still reads as advertised after discovery was turned off")
		}
	})
}
