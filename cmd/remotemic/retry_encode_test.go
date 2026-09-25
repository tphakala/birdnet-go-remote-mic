//go:build linux

package main

import (
	"errors"
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

func (s scriptedStage) Run(src audio.Source, _ func() bool, emit func(pipeline.Frame) error) error {
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
