//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	stdlog "log"
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
)

var errTestEIO = errors.New("input/output error")

// wantAutoRestart is the phrase a down message for a stable id carries.
const wantAutoRestart = "restarts automatically"

// failingSource is a capture that opens fine and then fails its first read, as a
// device does after an EIO right after open or a deterministic fault.
type failingSource struct{ rate, channels int }

func (f failingSource) Negotiated() (rate, channels int) { return f.rate, f.channels }
func (failingSource) Read() (audio.Period, error)        { return audio.Period{}, errTestEIO }
func (failingSource) Close() error                       { return nil }

// runFor stands in for the serve run loop for d of (synctest) time: it hands
// retry-timer signals to onRetryDue and pump endings to onPumpDone, the two
// events an unattended retry drives.
func runFor(t *testing.T, app *appliance, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case <-app.retryDue:
			app.onRetryDue()
		case res := <-app.pumpDone:
			app.onPumpDone(res)
		case <-deadline:
			return
		}
	}
}

// failOpenTimes makes the first n opens fail with a busy error and the rest open
// through the fake opener. A failed open is logged like a successful one, so
// opens counts every attempt.
func failOpenTimes(app *appliance, log *fakeOpenLog, n int) {
	next := app.open
	app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
		if n > 0 {
			n--
			log.add("open:" + dev.Name + "@" + dev.Device)
			return nil, errors.New("device or resource busy")
		}
		return next(dev, hub)
	}
}

// countDown counts the device's down-condition entries of the given kind in the
// notification history.
func countDown(t *testing.T, app *appliance, name string, kind notify.Kind) int {
	t.Helper()
	n := 0
	ns := applianceCenter(t, app).Snapshot().Notifications
	for i := range ns {
		if ns[i].Key == deviceDownKey(name) && ns[i].Kind == kind {
			n++
		}
	}
	return n
}

// opens counts how many times the fake opener opened the named device.
func opens(log *fakeOpenLog, name string) int {
	return strings.Count(strings.Join(log.snapshot(), " "), "open:"+name+"@")
}

// shutdownApp stops the appliance at the end of a synctest bubble so every pump
// goroutine and the retry timer are gone before the bubble returns.
func shutdownApp(app *appliance, cancel func()) {
	app.closeAll()
	cancel()
	synctest.Wait()
}

// captureLog redirects the standard logger into a buffer for the rest of the
// test and restores the previous writer afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	prev := stdlog.Writer()
	stdlog.SetOutput(&out)
	t.Cleanup(func() { stdlog.SetOutput(prev) })
	return &out
}

// TestRetryRestartsDeviceThatFailsToOpen is the #92 acceptance case: a device
// that is present but cannot be opened (held by another process) serves again on
// its own once the open succeeds, with no config save, publishing exactly one
// onset and one clear however many attempts it took.
func TestRetryRestartsDeviceThatFailsToOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 3)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
			t.Fatalf("moth state = %s, want skipped after the failed open", s)
		}
		genBefore := app.announceGen

		runFor(t, app, 5*time.Minute)

		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving after the retries", s)
		}
		if !app.srv.HasTrack("/m") {
			t.Error("the restarted device's RTSP track was not registered")
		}
		if got := opens(log, "moth"); got != 4 {
			t.Errorf("opens = %d, want 4 (the startup open and three retries)", got)
		}
		if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1", got)
		}
		if act := applianceCenter(t, app).Active(); len(act) != 0 {
			t.Errorf("active = %+v, want the down condition cleared", act)
		}
		if got := app.announceGen - genBefore; got != 1 {
			t.Errorf("announcement rebuilds = %d, want 1 (on recovery only)", got)
		}
	})
}

// TestRetryDeviceThatDiesAfterOpenDoesNotFlap pins the settle window: a device
// whose capture opens and then dies at once is not reported recovered on each
// attempt. The condition stays active until a restart stays up for retrySettle,
// so the history holds one onset and one clear, and the advertisement is rebuilt
// once.
func TestRetryDeviceThatDiesAfterOpenDoesNotFlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		deaths := 3
		app.open = fakeOpenerWith(log, func(rate, channels int) audio.Source {
			if deaths > 0 {
				deaths--
				return failingSource{rate, channels}
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

// TestRetryCauseSwitchKeepsOneCondition pins the markDown keep-rule in both
// directions: while an unattended restart is in flight, a failure that moves
// between two retryable causes (the capture died, then the reopen failed busy,
// or the other way round) keeps the one active condition instead of resolving
// it and raising a new onset on the switch.
func TestRetryCauseSwitchKeepsOneCondition(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failOpens is how many opens fail busy; deaths is how many opened
		// captures fail their first read. Opens fail first, then captures die,
		// unless diesFirst: then the first open succeeds and dies before the
		// failing opens.
		failOpens, deaths int
		diesFirst         bool
	}{
		{name: "died then open failed", failOpens: 2, deaths: 1, diesFirst: true},
		{name: "open failed then died", failOpens: 1, deaths: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, log, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				deaths := tc.deaths
				opener := fakeOpenerWith(log, func(rate, channels int) audio.Source {
					if deaths > 0 {
						deaths--
						return failingSource{rate, channels}
					}
					return newBlockingSource(rate, channels)
				})
				// diesFirst: the first open succeeds and dies, then the opens fail.
				fails, opened := tc.failOpens, false
				app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
					if fails > 0 && (!tc.diesFirst || opened) {
						fails--
						log.add("open:" + dev.Name + "@" + dev.Device)
						return nil, errors.New("device or resource busy")
					}
					opened = true
					return opener(dev, hub)
				}
				app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})

				runFor(t, app, 10*time.Minute)

				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving", s)
				}
				if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
					t.Errorf("down onsets = %d, want 1", got)
				}
				if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
					t.Errorf("down clears (including cause-change resolves) = %d, want 1", got)
				}
			})
		})
	}
}

// TestRetryPermanentFailureBacksOff pins the bound on a device that never comes
// back: attempts follow the backoff schedule (5 s, 10 s, 30 s, 1 min, 2 min, then
// every 5 min), so an hour costs 17 opens, and the condition is raised once and
// never cleared or re-announced.
func TestRetryPermanentFailureBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1<<30)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		genBefore := app.announceGen

		runFor(t, app, time.Hour)

		// The startup open, then retries at 5, 15, 45, 105, 225 s and every 300 s
		// from 525 s to 3525 s.
		if got := opens(log, "moth"); got != 17 {
			t.Errorf("opens in an hour = %d, want 17", got)
		}
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
			t.Errorf("moth state = %s, want skipped", s)
		}
		if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 0 {
			t.Errorf("down clears = %d, want 0", got)
		}
		if app.announceGen != genBefore {
			t.Errorf("announceGen = %d, want %d: a failed attempt must not rebuild the advertisement", app.announceGen, genBefore)
		}
		if !app.retrying("moth") {
			t.Error("the device stopped being retried")
		}
	})
}

// TestRetrySkipsCardIndexEntry pins the safety rule: a card-index entry is never
// restarted unattended, since its index may name different hardware by the time
// a retry runs. It waits for a config save.
func TestRetrySkipsCardIndexEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1<<30)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("byindex", addrHW3, "/a", 48000)}})

		runFor(t, app, 10*time.Minute)

		if got := opens(log, "byindex"); got != 1 {
			t.Errorf("opens = %d, want 1: a card-index entry must not be retried unattended", got)
		}
		if app.retrying("byindex") {
			t.Error("a card-index entry has a retry in flight")
		}
	})
}

// TestRetryStopsWhenDeviceDisabled pins that disabling a down device ends its
// retries: nothing reopens a device the operator turned off.
func TestRetryStopsWhenDeviceDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1<<30)
		dev := testDevice("moth", idMoth, "/m", 48000)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if !app.retrying("moth") {
			t.Fatal("precondition: the failing device has no retry in flight")
		}
		dev.Enabled = new(false)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if _, ok := app.retries["moth"]; ok {
			t.Error("disabling the device left its retry state behind")
		}

		runFor(t, app, 10*time.Minute)

		if got := opens(log, "moth"); got != 1 {
			t.Errorf("opens = %d, want 1: a disabled device must not be retried", got)
		}
		if len(app.retries) != 0 {
			t.Errorf("retries = %v, want none after the device was disabled", app.retries)
		}
	})
}

// TestRetryStopsWhenDeviceRemoved pins that removing a down device from the
// configuration ends its retries, and leaves the device that stays alone.
func TestRetryStopsWhenDeviceRemoved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		keep := testDevice("scarlett", idScarlett, "/s", 48000)
		next := fakeOpener(log)
		app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
			if dev.Name == "moth" {
				log.add("open:" + dev.Name + "@" + dev.Device)
				return nil, errors.New("device or resource busy")
			}
			return next(dev, hub)
		}
		app.reconcile(&config.Config{Devices: []config.Device{keep, testDevice("moth", idMoth, "/m", 48000)}})
		if !app.retrying("moth") {
			t.Fatal("precondition: the failing device has no retry in flight")
		}
		app.reconcile(&config.Config{Devices: []config.Device{keep}})
		if _, ok := app.retries["moth"]; ok {
			t.Error("the removed device still has retry state")
		}

		runFor(t, app, 10*time.Minute)

		if got := opens(log, "moth"); got != 1 {
			t.Errorf("moth opens = %d, want 1: a removed device must not be retried", got)
		}
		if s := app.devices["scarlett"].currentState(); s != mgmtserver.StateServing {
			t.Errorf("scarlett state = %s, want serving", s)
		}
	})
}

// TestRetryDropsStateForUnknownDevice pins the run-loop defence: a retry state
// whose name is not a configured device is dropped by the next pass rather than
// re-arming a zero-delay timer forever.
func TestRetryDropsStateForUnknownDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, _, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		app.retries["ghost"] = &retryState{attempts: 1, next: time.Now()}
		app.encodeFaults["ghost"] = encodeFault{paths: []string{"/ghost"}}
		app.armRetryTimer()

		// Drive the run loop by hand so a busy loop fails on a bound instead of
		// hanging: with nothing else due, a minute holds at most a few passes.
		passes := 0
		deadline := time.After(time.Minute)
	loop:
		for {
			select {
			case <-app.retryDue:
				passes++
				if passes > 10 {
					t.Fatalf("onRetryDue ran %d times within a minute, want a bounded few: the unconfigured state keeps re-arming the timer", passes)
				}
				app.onRetryDue()
			case res := <-app.pumpDone:
				app.onPumpDone(res)
			case <-deadline:
				break loop
			}
		}

		if _, ok := app.retries["ghost"]; ok {
			t.Error("retry state for an unconfigured device was not dropped")
		}
		// A device configured again under that name would otherwise inherit the
		// stale encode proof.
		if _, ok := app.encodeFaults["ghost"]; ok {
			t.Error("encode fault for an unconfigured device was not dropped")
		}
	})
}

// TestRetryDropsStateForDisabledDevice pins the other half of the run-loop
// defence: retry state under a name the configuration has but disables is
// dropped by the next pass, even though that name still has a device record.
func TestRetryDropsStateForDisabledDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, _, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		dev := testDevice("moth", idMoth, "/m", 48000)
		dev.Enabled = new(false)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if _, ok := app.devices["moth"]; !ok {
			t.Fatal("precondition: the disabled device has no record")
		}
		app.retries["moth"] = &retryState{attempts: 1, next: time.Now()}
		app.armRetryTimer()

		runFor(t, app, time.Minute)

		if _, ok := app.retries["moth"]; ok {
			t.Error("retry state for a disabled device was not dropped")
		}
	})
}

// TestRetryBackoffResetsAfterStableService pins the reset: a device that served
// for retryResetAfter after recovering starts its next failure at the shortest
// delay, while one that fails again soon after recovering continues its backoff.
func TestRetryBackoffResetsAfterStableService(t *testing.T) {
	for _, tc := range []struct {
		name         string
		served       time.Duration
		wantAttempts int
	}{
		{name: "stable", served: retryResetAfter + time.Minute, wantAttempts: 1},
		// Exactly retryResetAfter counts as stable: the reset is >=, not >.
		{name: "boundary", served: retryResetAfter, wantAttempts: 1},
		{name: "just short", served: retryResetAfter - time.Second, wantAttempts: 2},
		{name: "unstable", served: time.Minute, wantAttempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, log, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				failOpenTimes(app, log, 1)
				app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
				// The retry at 5 s opens it; it settles at 35 s.
				runFor(t, app, retryBackoff[0]+retrySettle+tc.served)
				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving", s)
				}

				// Kill the running device as a still-present failure.
				killDevice(t, app, log, "moth", errTestEIO)

				st := app.retries["moth"]
				if st == nil {
					t.Fatal("no retry was scheduled for the failed device")
				}
				if st.attempts != tc.wantAttempts {
					t.Errorf("attempts = %d, want %d", st.attempts, tc.wantAttempts)
				}
				if got, want := time.Until(st.next), backoffDelay(tc.wantAttempts); got != want {
					t.Errorf("next retry in %s, want %s", got, want)
				}
			})
		})
	}
}

// TestRetryConfigSaveResetsBackoff pins that an explicit restart starts the
// backoff over: a device that has backed off for a while and is then saved
// again (and still fails) is next retried after the shortest delay.
func TestRetryConfigSaveResetsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1<<30)
		cfg := config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}}
		app.reconcile(&cfg)
		runFor(t, app, 2*time.Minute)
		if st := app.retries["moth"]; st == nil || st.attempts < 4 {
			t.Fatalf("precondition: retry state = %+v, want several failures", st)
		}

		app.reconcile(&cfg)

		st := app.retries["moth"]
		if st == nil {
			t.Fatal("no retry was scheduled after the config save")
		}
		if st.attempts != 1 {
			t.Errorf("attempts = %d, want 1 after a config save", st.attempts)
		}
		if got, want := time.Until(st.next), backoffDelay(1); got != want {
			t.Errorf("next retry in %s, want %s", got, want)
		}
	})
}

// TestRetryRecoversFromResolveFailure pins that a resolve failure that is not
// about the device itself (the host's device listing could not be read) is
// retried like an open failure, and that each attempt resolves the id afresh
// rather than reusing the failed resolution.
func TestRetryRecoversFromResolveFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, _, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failures := 2
		resolve := app.resolve
		app.resolve = func(id string) (audio.Hardware, error) {
			if failures > 0 {
				failures--
				return audio.Hardware{}, errors.New("reading the sound card listing: input/output error")
			}
			return resolve(id)
		}
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
			t.Fatalf("moth state = %s, want skipped after the failed resolve", s)
		}

		runFor(t, app, 5*time.Minute)

		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving once the id resolves again", s)
		}
		if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1", got)
		}
	})
}

// TestRetryNewOutageIsLoggedFromItsFirstFailure pins that a device which fails
// again after a completed recovery is logged from its first failure, even though
// its backoff carries on from the earlier outage, and that the recovery itself
// logs what the device came back on.
func TestRetryNewOutageIsLoggedFromItsFirstFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := captureLog(t)
		app, opLog, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		// Five failed opens, then opens succeed; deaths makes an opened capture
		// fail its first read.
		fails, deaths := 5, 0
		opener := fakeOpenerWith(opLog, func(rate, channels int) audio.Source {
			if deaths > 0 {
				deaths--
				return failingSource{rate, channels}
			}
			return newBlockingSource(rate, channels)
		})
		app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
			if fails > 0 {
				fails--
				opLog.add("open:" + dev.Name + "@" + dev.Device)
				return nil, errors.New("device or resource busy")
			}
			return opener(dev, hub)
		}
		const rate = 48000
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", rate)}})

		// Outage 1: the startup open and retries 1 and 2 log their failures;
		// retries 3 and 4 are quiet, and retry 5 opens the device.
		runFor(t, app, 5*time.Minute)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("precondition: moth state = %s, want serving", s)
		}
		if got := strings.Count(out.String(), `skipping device "moth"`); got != 3 {
			t.Errorf("logged open failures = %d, want 3: attempts past the logged ones must be quiet\n%s", got, out.String())
		}
		if want := fmt.Sprintf(`device "moth" recovered: capturing at %d Hz`, rate); !strings.Contains(out.String(), want) {
			t.Errorf("no recovery line %q in the log:\n%s", want, out.String())
		}

		// Outage 2 begins inside retryResetAfter, so the backoff carries on to
		// 6 attempts while the outage's own failure count starts at 1.
		out.Reset()
		killDevice(t, app, opLog, "moth", errTestEIO)

		st := app.retries["moth"]
		if st == nil || !app.retrying("moth") {
			t.Fatalf("retry state = %+v, retrying %v; want a retry in flight", st, app.retrying("moth"))
		}
		if st.attempts != 6 || st.failures != 1 {
			t.Errorf("attempts, failures = %d, %d; want 6, 1", st.attempts, st.failures)
		}
		if want := fmt.Sprintf(`device "moth": retrying in %s (failure 1)`, backoffDelay(6)); !strings.Contains(out.String(), want) {
			t.Errorf("no %q in the log:\n%s", want, out.String())
		}

		// Retry 1 of outage 2 opens the device and its capture dies during the
		// settle. Both the retry and the death are within the outage's first
		// logged failures, so both are logged.
		out.Reset()
		deaths = 1
		runFor(t, app, backoffDelay(6)+time.Second)
		for _, want := range []string{`device "moth": retry 1`, `capture "moth"`, `device "moth" failed:`} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("no %q in the log:\n%s", want, out.String())
			}
		}
	})
}

// TestRetryQuietPumpDeathStaysQuiet pins the pump-death half of the retry log
// gate: a device that opens on every attempt and dies right after logs its
// death only on the failures logAttempt picks (the first retryLogFirst, then
// every retryLogEvery-th), like a failed open does. Without the gate in
// onPumpDone, a device that keeps dying would log every attempt, forever.
func TestRetryQuietPumpDeathStaysQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := captureLog(t)
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		app.open = fakeOpenerWith(log, func(rate, channels int) audio.Source {
			return failingSource{rate, channels}
		})
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})

		// Long enough for failure retryLogEvery+1 (the capped backoff repeats),
		// short of the one after it.
		var elapsed time.Duration
		for n := 1; n <= retryLogEvery; n++ {
			elapsed += backoffDelay(n)
		}
		runFor(t, app, elapsed+backoffDelay(retryLogEvery+1)/2)

		st := app.retries["moth"]
		if st == nil || st.failures != retryLogEvery+1 {
			t.Fatalf("retry state = %+v, want %d failures", st, retryLogEvery+1)
		}
		want := 0
		for n := 1; n <= st.failures; n++ {
			if logAttempt(n) {
				want++
			}
		}
		if got := strings.Count(out.String(), `device "moth" failed:`); got != want {
			t.Errorf("logged deaths = %d over %d failures, want %d (only the failures logAttempt picks)\n%s", got, st.failures, want, out.String())
		}
	})
}

// TestRetryConfigSaveRecoversRetryingDevice pins that a config save that brings
// back a device with a retry in flight, whether it is backing off or waiting
// out its settle, clears its condition exactly once and ends the retry: the
// save deletes the retry state, so nothing else would ever clear it.
func TestRetryConfigSaveRecoversRetryingDevice(t *testing.T) {
	for _, tc := range []struct {
		name string
		// settling lets one retry open the device before the save, so the save
		// restarts a device that is serving but not yet recovered.
		settling bool
	}{
		{name: "backing off"},
		{name: "settling", settling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app, opLog, cancel := newTestAppliance(t)
				defer shutdownApp(app, cancel)
				broken := true
				next := app.open
				app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
					if broken {
						opLog.add("open:" + dev.Name + "@" + dev.Device)
						return nil, errors.New("device or resource busy")
					}
					return next(dev, hub)
				}
				dev := testDevice("moth", idMoth, "/m", 48000)
				app.reconcile(&config.Config{Devices: []config.Device{dev}})
				if tc.settling {
					broken = false
					runFor(t, app, retryBackoff[0]+time.Second)
					if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing || !app.retrying("moth") {
						t.Fatalf("precondition: moth = %s, retrying %v; want serving and settling", s, app.retrying("moth"))
					}
					// A rate change makes the save restart the serving device.
					dev.Rate = 96000
				} else {
					runFor(t, app, time.Minute)
					if !app.retrying("moth") {
						t.Fatal("precondition: no retry in flight")
					}
					broken = false
				}

				app.reconcile(&config.Config{Devices: []config.Device{dev}})

				if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
					t.Fatalf("moth state = %s, want serving after the save", s)
				}
				if _, ok := app.retries["moth"]; ok {
					t.Error("the save left retry state behind")
				}
				runFor(t, app, time.Minute)
				if act := applianceCenter(t, app).Active(); len(act) != 0 {
					t.Errorf("active = %+v, want the down condition cleared", act)
				}
				if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
					t.Errorf("down clears = %d, want 1", got)
				}
			})
		})
	}
}

// TestRetryFailedMessageMatchesRestartPath pins the failed-while-present text:
// a stable id says it restarts on its own, a card-index id (never restarted
// unattended) says it waits for a config save.
func TestRetryFailedMessageMatchesRestartPath(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
	}{
		{name: "stable id", id: idMoth, want: wantAutoRestart},
		{name: "card index", id: addrHW3, want: "restarts on the next config save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, log, cancel := newTestAppliance(t)
			defer cancel()
			defer app.closeAll()
			withHost(app, &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}})
			app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", tc.id, "/m", 48000)}})
			killDevice(t, app, log, "moth", errTestEIO)

			act := applianceCenter(t, app).Active()
			if len(act) != 1 || act[0].Title != titleFailed {
				t.Fatalf("active = %+v, want one Device failed", act)
			}
			if !strings.Contains(act[0].Message, tc.want) {
				t.Errorf("message = %q, want it to say %q", act[0].Message, tc.want)
			}
		})
	}
}

// TestRetryStopsWhenDeviceDisappears pins that a retry which finds the device
// gone hands it to the hardware-change path: the condition is re-raised as not
// connected and the backoff state is dropped.
func TestRetryStopsWhenDeviceDisappears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, opLog, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
		withHost(app, host)
		failOpenTimes(app, opLog, 1<<30)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		if !app.retrying("moth") {
			t.Fatal("precondition: no retry in flight")
		}

		host.devs = nil
		runFor(t, app, time.Minute)

		if _, ok := app.retries["moth"]; ok {
			t.Error("retry state survived the device disappearing")
		}
		if act := applianceCenter(t, app).Active(); len(act) != 1 || act[0].Title != titleNotConnected {
			t.Errorf("active = %+v, want one Device not connected", act)
		}
		if got := opens(opLog, "moth"); got != 1 {
			t.Errorf("opens = %d, want 1: an absent device is not opened", got)
		}
	})
}

// TestRetryQuietAttemptLogsEndOfRetry pins that a retry attempt whose own log
// lines are silenced still logs a cause that ends the retry: the device went
// missing, the backoff is dropped, and nothing else would record why.
func TestRetryQuietAttemptLogsEndOfRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := captureLog(t)
		app, opLog, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
		withHost(app, host)
		failOpenTimes(app, opLog, 1<<30)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		// Failures at 0, 5 and 15 s; the retry due at 45 s is the first quiet one.
		runFor(t, app, 20*time.Second)
		if st := app.retries["moth"]; st == nil || app.nextFailureLogged("moth") {
			t.Fatalf("precondition: retry state = %+v, want the next attempt quiet", st)
		}

		out.Reset()
		host.devs = nil
		runFor(t, app, 30*time.Second)

		if app.retrying("moth") {
			t.Fatal("precondition: the retry did not end on the missing device")
		}
		if !strings.Contains(out.String(), `skipping device "moth": Not connected`) {
			t.Errorf("the quiet attempt that ended the retry left no log line:\n%s", out.String())
		}
	})
}

// TestRetryLostDeviceDropsRetry pins that a device lost while its retried
// restart is settling leaves the backoff: the hardware-change retry brings a
// disconnected device back, so a lingering retry state would only keep it
// marked as retrying.
func TestRetryLostDeviceDropsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, opLog, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, opLog, 1)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		runFor(t, app, retryBackoff[0]+time.Second)
		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing || !app.retrying("moth") {
			t.Fatalf("precondition: moth = %s, retrying %v; want serving and settling", s, app.retrying("moth"))
		}

		killDevice(t, app, opLog, "moth", capture.ErrDeviceGone)

		if _, ok := app.retries["moth"]; ok {
			t.Error("a lost device kept its retry state")
		}
		if act := applianceCenter(t, app).Active(); len(act) != 1 || act[0].Title != titleDisconnected {
			t.Errorf("active = %+v, want one Device disconnected", act)
		}
	})
}

// TestRetryOpenFailureMessageMatchesRestartPath pins the open-failure and
// resolve-failure onset text: a stable id is retried automatically, a
// card-index id waits for a config save.
func TestRetryOpenFailureMessageMatchesRestartPath(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
		resolveFails   bool
	}{
		{name: "open fails, stable id", id: idMoth, want: wantAutoRestart},
		{name: "open fails, card index", id: addrHW3, want: "restarts on the next config save"},
		{name: "resolve fails, stable id", id: idMoth, want: wantAutoRestart, resolveFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, cancel := newTestAppliance(t)
			defer cancel()
			defer app.closeAll()
			if tc.resolveFails {
				app.resolve = func(string) (audio.Hardware, error) {
					return audio.Hardware{}, errors.New("reading the sound card listing: input/output error")
				}
			}
			failOpen(app)
			app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", tc.id, "/m", 48000)}})

			act := applianceCenter(t, app).Active()
			if len(act) != 1 {
				t.Fatalf("active = %+v, want one down condition", act)
			}
			if !strings.Contains(act[0].Message, tc.want) {
				t.Errorf("message = %q, want it to say %q", act[0].Message, tc.want)
			}
		})
	}
}

// TestStopRetriesStopsTheTimer pins shutdown: once stopRetries has run, a
// pending retry never fires, so no attempt reopens a device after closeAll.
func TestStopRetriesStopsTheTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1)
		app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
		if app.retryTimer == nil {
			t.Fatal("precondition: no retry timer armed after the failed open")
		}

		app.stopRetries()
		if app.retryTimer != nil || !app.retryAt.IsZero() {
			t.Errorf("after stopRetries: timer %v, retryAt %v; want nil and zero", app.retryTimer, app.retryAt)
		}
		runFor(t, app, 10*retryBackoff[0])
		if got := opens(log, "moth"); got != 1 {
			t.Errorf("opens = %d, want 1: a retry fired after stopRetries", got)
		}
	})
}

// TestRetryTimerSignalCoalesces pins the non-blocking send: with a signal
// already pending, a second firing is dropped rather than blocking the timer
// goroutine; the pending signal already covers it.
func TestRetryTimerSignalCoalesces(t *testing.T) {
	t.Parallel()
	// In a synctest bubble a blocked send is a deadlock the bubble reports at
	// once, with no wall-clock timeout to wait out.
	synctest.Test(t, func(t *testing.T) {
		app := &appliance{retryDue: make(chan struct{}, 1)}
		app.signalRetryDue()
		app.signalRetryDue()
		if got := len(app.retryDue); got != 1 {
			t.Errorf("pending signals = %d, want 1", got)
		}
	})
}

// TestArmRetryTimerFollowsEarliestDeadline pins what the timer is armed for:
// the earliest pending deadline across devices, unmoved by a later one, moved by
// an earlier one (which then fires on time), and nothing once none is pending.
// The timer is reused across re-arms rather than re-created.
func TestArmRetryTimerFollowsEarliestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := &appliance{retryDue: make(chan struct{}, 1), retries: map[string]*retryState{}}
		defer app.stopRetries()
		first := time.Now().Add(time.Minute)
		app.retries["a"] = &retryState{next: first}
		app.armRetryTimer()
		timer := app.retryTimer
		if timer == nil || !app.retryAt.Equal(first) {
			t.Fatalf("armed for %v (timer %v), want %v", app.retryAt, timer, first)
		}

		app.retries["b"] = &retryState{next: first.Add(time.Minute)}
		app.armRetryTimer()
		if app.retryTimer != timer || !app.retryAt.Equal(first) {
			t.Errorf("a later deadline moved the timer to %v", app.retryAt)
		}

		earlier := time.Now().Add(10 * time.Second)
		app.retries["c"] = &retryState{settleAt: earlier}
		app.armRetryTimer()
		if !app.retryAt.Equal(earlier) || app.retryTimer != timer {
			t.Errorf("armed for %v (same timer %v), want the earlier deadline %v on the same timer", app.retryAt, app.retryTimer == timer, earlier)
		}
		time.Sleep(15 * time.Second)
		synctest.Wait() // let the timer's callback goroutine run
		if got := len(app.retryDue); got != 1 {
			t.Errorf("pending signals at the earlier deadline = %d, want 1", got)
		}
		<-app.retryDue

		// With a deadline pending, dropping every state must stop the timer: no
		// signal arrives when that deadline passes.
		delete(app.retries, "c")
		app.armRetryTimer()
		if !app.retryAt.Equal(first) {
			t.Fatalf("precondition: armed for %v, want %v", app.retryAt, first)
		}
		clear(app.retries)
		app.armRetryTimer()
		if !app.retryAt.IsZero() {
			t.Errorf("armed for %v with nothing pending, want zero", app.retryAt)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := len(app.retryDue); got != 0 {
			t.Errorf("pending signals after the dropped deadline = %d, want 0: the timer was not stopped", got)
		}
	})
}

// TestStartDeviceNonRetryableDisarmsTimer pins dropRetry's re-arm: a device in
// backoff that a config save restarts into a cause a retry cannot fix loses its
// state in startDevice, and the timer must follow rather than stay armed for
// the deleted deadline.
func TestStartDeviceNonRetryableDisarmsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		failOpenTimes(app, log, 1)
		dev := testDevice("moth", idMoth, "/m", 48000)
		app.reconcile(&config.Config{Devices: []config.Device{dev}})
		if app.retryAt.IsZero() {
			t.Fatal("precondition: no retry deadline armed after the failed open")
		}

		app.resolve = func(id string) (audio.Hardware, error) {
			return audio.Hardware{}, &capture.DeviceNotFoundError{ID: id}
		}
		app.refreshHardware(&app.cfg)
		app.startDevice(&dev)

		if _, ok := app.retries["moth"]; ok {
			t.Fatal("precondition: a not-connected device kept its retry state")
		}
		if !app.retryAt.IsZero() {
			t.Errorf("timer still armed for %v after the only retry state was dropped", app.retryAt)
		}
	})
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 30 * time.Second},
		{4, time.Minute},
		{5, 2 * time.Minute},
		{6, 5 * time.Minute},
		{100, 5 * time.Minute},
	} {
		if got := backoffDelay(tc.attempts); got != tc.want {
			t.Errorf("backoffDelay(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}
