//go:build linux

package main

import (
	"errors"
	"slices"
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
// enumeration retry), and is named as a stall in the cause, the log and the
// notification.
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
			if got := app.devices["moth"].status().DownCause; got != downStalled {
				t.Errorf("DownCause = %q, want %q", got, downStalled)
			}
			act := center.Active()
			if len(act) != 1 || act[0].Title != titleStalled {
				t.Fatalf("active after the stall = %+v, want one Device stalled", act)
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

// TestRetryFailedAndStalledShareOneCondition pins that a present device that
// alternates between dying and stalling keeps one down condition for the whole
// outage, with its title and the device record's cause following the latest
// death.
func TestRetryFailedAndStalledShareOneCondition(t *testing.T) {
	stall := &capture.StallError{Recoveries: 3, Err: syscall.EIO}
	fail := errors.New("alsa: read failed")
	for _, tc := range []struct {
		name   string
		deaths []error
	}{
		{"failed, stalled, failed, stalled", []error{fail, stall, fail, stall}},
		{"stalled, failed, stalled", []error{stall, fail, stall}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, log, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				center := applianceCenter(t, app)
				deaths := slices.Clone(tc.deaths)
				app.open = fakeOpenerWith(log, func(rate, channels int) audio.Source {
					if len(deaths) > 0 {
						e := deaths[0]
						deaths = deaths[1:]
						return erroringSource{rate, channels, e}
					}
					return newBlockingSource(rate, channels)
				})
				app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
				genBefore := app.announceGen

				for i, e := range tc.deaths {
					res := <-app.pumpDone
					app.onPumpDone(res)
					wantCause, wantTitle := downFailed, titleFailed
					if errors.Is(e, capture.ErrDeviceStalled) {
						wantCause, wantTitle = downStalled, titleStalled
					}
					if got := app.devices["moth"].status().DownCause; got != wantCause {
						t.Errorf("death %d: DownCause = %q, want %q", i, got, wantCause)
					}
					act := center.Active()
					if len(act) != 1 || act[0].Title != wantTitle {
						t.Fatalf("death %d: active = %+v, want one %q", i, act, wantTitle)
					}
					if i < len(tc.deaths)-1 {
						<-app.retryDue
						app.onRetryDue()
					}
				}
				<-app.retryDue
				app.onRetryDue()
				runFor(t, app, 5*time.Minute)

				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving", s)
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
		})
	}
}

// TestEncodeWaitOverStalledConditionRewritesInPlace pins that the encode-fault
// wait, raised as failed, rewrites a stalled condition's title and text in
// place instead of raising a second onset.
func TestEncodeWaitOverStalledConditionRewritesInPlace(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
	center := applianceCenter(t, app)
	killDevice(t, app, log, "moth", &capture.StallError{Recoveries: 3, Err: syscall.EIO})

	n := deviceDownOnset("moth", titleFailed, "Encoding failed earlier on /m")
	app.replaceDown("moth", downFailed, &n)

	if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
		t.Errorf("down onsets = %d, want 1", got)
	}
	act := center.Active()
	if len(act) != 1 || act[0].Title != titleFailed || !strings.Contains(act[0].Message, "Encoding failed earlier") {
		t.Errorf("active = %+v, want one Device failed with the encode-wait text", act)
	}
	if got := app.downReason["moth"]; got != downFailed {
		t.Errorf("downReason = %q, want %q", got, downFailed)
	}
}
