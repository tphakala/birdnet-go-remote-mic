//go:build linux

package main

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

var errTestEncode = errors.New("encode failed")

// scriptedStage stands in for a stream's encode stage. It drains its source and
// encodes nothing until the test "plays" it: a true on play faults the encode,
// a false encodes one frame and then keeps draining.
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
	select {
	case fault := <-s.play:
		if fault {
			return errTestEncode
		}
		if err := emit(pipeline.Frame{}); err != nil {
			return err
		}
		<-ended
		return nil
	case <-ended:
		return nil
	}
}

// scriptedStages wraps the fake opener so every opened stream runs a
// scriptedStage, and returns a function yielding the play channel of the most
// recently opened runtime.
func scriptedStages(app *appliance, log *fakeOpenLog) func() chan bool {
	next := fakeOpenerWith(log, func(rate, channels int) audio.Source { return newBlockingSource(rate, channels) })
	var latest chan bool
	app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
		rt, err := next(dev, hub)
		if err != nil {
			return rt, err
		}
		latest = make(chan bool)
		for _, sr := range rt.streams {
			sr.stage = scriptedStage{play: latest}
		}
		return rt, nil
	}
	return func() chan bool { return latest }
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
		play := scriptedStages(app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		genBefore := app.announceGen

		// A client plays and the encode faults: the device fails and is retried.
		play() <- true
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
		play() <- true
		runFor(t, app, 2*time.Minute)
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Fatalf("down clears = %d, want 0 after a second fault", got)
		}

		// A client plays and encoding succeeds: the condition clears retrySettle
		// after the first encoded frame, not before.
		play() <- false
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
	})
}

// TestRetryAfterEncodeFaultCountsEncodeInsideWindow pins the other ordering: a
// client that plays during the first settle window (before the deadline) is
// seen at the deadline, and the settle then counts retrySettle from there.
func TestRetryAfterEncodeFaultCountsEncodeInsideWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		play := scriptedStages(app, log)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})

		play() <- true
		// The first retry is due 5 s after the fault; let it open.
		runFor(t, app, 6*time.Second)
		play() <- false
		runFor(t, app, 2*retrySettle+time.Second)
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1 once the restart has encoded", got)
		}
	})
}
