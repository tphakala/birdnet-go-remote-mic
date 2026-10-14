//go:build linux

package main

import (
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// stallErrors are the two ways the capture library reports ErrDeviceStalled.
func stallErrors() map[string]error {
	return map[string]error{
		"stall":     &capture.StallError{Recoveries: 3, Err: syscall.EIO},
		"shortfall": &capture.ShortfallError{Rate: 48000, Window: 2 * time.Second, Expected: 96000, Delivered: 40000},
	}
}

// erroringSource is a capture that opens fine and then fails its reads with err.
type erroringSource struct {
	rate, channels int
	err            error
}

func (e erroringSource) Negotiated() (rate, channels int) { return e.rate, e.channels }
func (e erroringSource) Read() (audio.Period, error)      { return audio.Period{}, e.err }
func (erroringSource) Close() error                       { return nil }

// TestPumpStallOnPresentDeviceRetriesOnBackoff pins that a stall on a device that
// still resolves takes the failed-but-present path (backoff retry, no
// enumeration retry), and is named as a stall in the log and the notification.
func TestPumpStallOnPresentDeviceRetriesOnBackoff(t *testing.T) {
	for name, stallErr := range stallErrors() {
		t.Run(name, func(t *testing.T) {
			logBuf := captureLog(t)
			app, log, cancel := newTestAppliance(t)
			defer cancel()
			defer app.closeAll()
			host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
			withHost(app, host)
			app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
			center := applianceCenter(t, app)

			killDevice(t, app, log, "moth", stallErr)

			if app.prov.retryArmed.Load() {
				t.Error("a stall on a present device armed the enumeration retry")
			}
			if !app.retrying("moth") {
				t.Error("a stalled present device was not scheduled for a backoff retry")
			}
			if got := app.devices["moth"].status().DownCause; got != downFailed {
				t.Errorf("DownCause = %q, want %q", got, downFailed)
			}
			act := center.Active()
			if len(act) != 1 || act[0].Title != titleFailed {
				t.Fatalf("active after the stall = %+v, want one Device failed", act)
			}
			if !strings.Contains(act[0].Message, "stalled") || !strings.Contains(act[0].Message, stallErr.Error()) {
				t.Errorf("notification message = %q, want it to name the stall and carry %q", act[0].Message, stallErr)
			}
			if want := `device "moth" failed: stalled`; !strings.Contains(logBuf.String(), want) {
				t.Errorf("log = %q, want it to contain %q", logBuf.String(), want)
			}
		})
	}
}

// TestPumpStallOnAbsentDeviceIsDisconnect pins that the re-resolve still wins for
// a stall on a device that vanished: it is a disconnect and arms the retry.
func TestPumpStallOnAbsentDeviceIsDisconnect(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})

	host.devs = nil
	killDevice(t, app, log, "moth", &capture.StallError{Recoveries: 3, Err: syscall.EIO})

	if !app.prov.retryArmed.Load() {
		t.Error("a stall on a vanished device did not arm the enumeration retry")
	}
	if got := app.devices["moth"].status().DownCause; got != downDisconnected {
		t.Errorf("DownCause = %q, want %q", got, downDisconnected)
	}
}

// TestRetryStallingDeviceDoesNotFlap runs stall, restart, stall transitions: the
// device stalls three times with alternating error kinds, then stays up. The
// condition stays active across the retries, so there is one onset, one clear
// and one announcement rebuild.
func TestRetryStallingDeviceDoesNotFlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		errs := []error{
			&capture.StallError{Recoveries: 3, Err: syscall.EIO},
			&capture.ShortfallError{Rate: 48000, Window: 2 * time.Second, Expected: 96000, Delivered: 40000},
			&capture.StallError{Recoveries: 3, Err: syscall.EIO},
		}
		app.open = fakeOpenerWith(log, func(rate, channels int) audio.Source {
			if len(errs) > 0 {
				e := errs[0]
				errs = errs[1:]
				return erroringSource{rate, channels, e}
			}
			return newBlockingSource(rate, channels)
		})
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		genBefore := app.announceGen

		runFor(t, app, 5*time.Minute)

		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving once a restart stays up", s)
		}
		if got := opens(log, "moth"); got != 4 {
			t.Errorf("opens = %d, want 4", got)
		}
		if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1", got)
		}
		if got := app.announceGen - genBefore; got != 1 {
			t.Errorf("announcement rebuilds = %d, want 1", got)
		}
	})
}
