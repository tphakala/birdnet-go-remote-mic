//go:build linux

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
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

// hour61 is a run long enough to count as stable (geometryStableRun).
const hour61 = 61 * time.Minute

func shortfallErr() error {
	return &capture.ShortfallError{Rate: 48000, Window: 2 * time.Second, Expected: 96000, Delivered: 40000}
}

// countGeometry counts the device's capture-period condition entries of kind in
// the notification history (Resolve publishes a clear).
func countGeometry(t *testing.T, app *appliance, name string, kind notify.Kind) int {
	t.Helper()
	n := 0
	ns := applianceCenter(t, app).Snapshot().Notifications
	for i := range ns {
		if ns[i].Key == geometryKey(name) && ns[i].Kind == kind {
			n++
		}
	}
	return n
}

// activeGeometry returns the active capture-period condition of name.
func activeGeometry(t *testing.T, app *appliance, name string) (notify.Notification, bool) {
	t.Helper()
	act := applianceCenter(t, app).Active()
	for i := range act {
		if act[i].Key == geometryKey(name) {
			return act[i], true
		}
	}
	return notify.Notification{}, false
}

// geoHarness drives one device's deaths and retries by hand inside a synctest
// bubble, the way the serve loop would.
type geoHarness struct {
	t    *testing.T
	app  *appliance
	log  *fakeOpenLog
	name string
}

// die fails the device's capture with err and processes the death.
func (h *geoHarness) die(err error) {
	h.t.Helper()
	killDevice(h.t, h.app, h.log, h.name, err)
}

// retry runs the loop's retry handling until the device is opened again.
func (h *geoHarness) retry() {
	h.t.Helper()
	before := len(h.log.stepsFor(h.name))
	for len(h.log.stepsFor(h.name)) == before {
		<-h.app.retryDue
		h.app.onRetryDue()
	}
}

// shortfall kills the device with a shortfall and lets its retry reopen it.
func (h *geoHarness) shortfall() {
	h.t.Helper()
	h.die(shortfallErr())
	h.retry()
}

func (h *geoHarness) steps() []int { return h.log.stepsFor(h.name) }

func (h *geoHarness) wantSteps(want ...int) {
	h.t.Helper()
	if got := h.steps(); !slices.Equal(got, want) {
		h.t.Errorf("open steps = %v, want %v", got, want)
	}
}

func (h *geoHarness) wantGeometry(onsets, clears int) {
	h.t.Helper()
	if got := countGeometry(h.t, h.app, h.name, notify.KindOnset); got != onsets {
		h.t.Errorf("capture-period onsets = %d, want %d", got, onsets)
	}
	if got := countGeometry(h.t, h.app, h.name, notify.KindClear); got != clears {
		h.t.Errorf("capture-period clears = %d, want %d", got, clears)
	}
}

// newGeoHarness opens one stable-id device "moth" through the fake opener.
func newGeoHarness(t *testing.T, cfg *config.Device) (*geoHarness, *fakeHost, func()) {
	t.Helper()
	app, log, cancel := newTestAppliance(t)
	host := &fakeHost{devs: []audio.Hardware{{ID: cfg.Device, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{*cfg}})
	return &geoHarness{t: t, app: app, log: log, name: cfg.Name}, host, func() { shutdownApp(app, cancel) }
}

func mothDevice() config.Device { return testDevice("moth", idMoth, "/m", 48000) }

// TestShortfallsRaiseCapturePeriodStepByStep pins the ladder: two shortfalls per
// step, the top step is held, and the one capture-period condition is raised
// once and rewritten, never re-raised, without disturbing the down condition's
// one onset and one clear.
func TestShortfallsRaiseCapturePeriodStepByStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, log, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		fails := 8
		app.open = fakeOpenerWith(log, func(rate, channels int) audio.Source {
			if fails > 0 {
				fails--
				return erroringSource{rate, channels, shortfallErr()}
			}
			return newBlockingSource(rate, channels)
		})
		app.reconcile(&config.Config{Devices: []config.Device{mothDevice()}})

		runFor(t, app, 30*time.Minute)

		if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("moth state = %s, want serving", s)
		}
		if got, want := log.stepsFor("moth"), []int{0, 0, 1, 1, 2, 2, 3, 3, 3}; !slices.Equal(got, want) {
			t.Errorf("open steps = %v, want %v", got, want)
		}
		if got := countGeometry(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("capture-period onsets = %d, want 1", got)
		}
		if got := countGeometry(t, app, "moth", notify.KindClear); got != 0 {
			t.Errorf("capture-period clears = %d, want 0", got)
		}
		n, ok := activeGeometry(t, app, "moth")
		if !ok {
			t.Fatal("no active capture-period condition at the top step")
		}
		if n.Severity != notify.SeverityWarning || !strings.Contains(n.Message, "40 ms period and 16 periods (640 ms buffer)") {
			t.Errorf("condition = %q (%s), want a warning naming the top step", n.Message, n.Severity)
		}
		if got := countDown(t, app, "moth", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}
		if got := countDown(t, app, "moth", notify.KindClear); got != 1 {
			t.Errorf("down clears = %d, want 1", got)
		}
	})
}

// TestCapturePeriodStepTransitions walks sequences of deaths, restarts and
// config saves and pins the step each next open asks for and the condition's
// notifications.
func TestCapturePeriodStepTransitions(t *testing.T) {
	plain := errors.New("alsa: read failed")
	otherID := "usb:ffff:0001:s=X:if=0,0"

	cases := []struct {
		name string
		run  func(h *geoHarness, host *fakeHost)
		cfg  func() config.Device
	}{
		{"stable run then a plain death resets", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			runFor(h.t, h.app, hour61)
			h.die(plain)
			h.retry()
			h.wantSteps(0, 0, 1, 0)
			h.wantGeometry(1, 1)
		}, nil},
		{"stable run resets from the middle step", func(h *geoHarness, _ *fakeHost) {
			for range 4 {
				h.shortfall()
			}
			runFor(h.t, h.app, hour61)
			h.die(plain)
			h.retry()
			h.wantSteps(0, 0, 1, 1, 2, 0)
			h.wantGeometry(1, 1)
		}, nil},
		{"stable run resets from the top step", func(h *geoHarness, _ *fakeHost) {
			for range 6 {
				h.shortfall()
			}
			runFor(h.t, h.app, hour61)
			h.die(plain)
			h.retry()
			h.wantSteps(0, 0, 1, 1, 2, 2, 3, 0)
			h.wantGeometry(1, 1)
		}, nil},
		{"stable run then a deliberate restart resets", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			runFor(h.t, h.app, hour61)
			d := mothDevice()
			d.Streams[0].Path = "/m2"
			h.app.reconcile(&config.Config{Devices: []config.Device{d}})
			h.wantSteps(0, 0, 1, 0)
			h.wantGeometry(1, 1)
		}, nil},
		{"intermittent shortfalls do not escalate", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			runFor(h.t, h.app, hour61)
			h.shortfall()
			h.wantSteps(0, 0, 0)
			h.wantGeometry(0, 0)
		}, nil},
		{"short runs do not reset", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			runFor(h.t, h.app, 30*time.Minute)
			h.shortfall()
			h.shortfall()
			h.wantSteps(0, 0, 1, 1, 2)
			h.wantGeometry(1, 0)
		}, nil},
		{"only shortfalls count", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.die(&capture.StallError{Recoveries: 3, Err: syscall.EIO})
			h.retry()
			h.die(plain)
			h.retry()
			h.shortfall()
			h.wantSteps(0, 0, 0, 0, 1)
		}, nil},
		{"a changed rate starts over", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			d := mothDevice()
			d.Rate = 44100
			h.app.reconcile(&config.Config{Devices: []config.Device{d}})
			h.wantSteps(0, 0, 1, 0)
			if _, ok := activeGeometry(h.t, h.app, "moth"); ok {
				h.t.Error("capture-period condition still active after the rate changed")
			}
		}, nil},
		{"a changed channel set starts over", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			d := mothDevice()
			d.Streams[0].Channels = []int{1, 2}
			h.app.reconcile(&config.Config{Devices: []config.Device{d}})
			h.wantSteps(0, 0, 1, 0)
		}, nil},
		{"a changed id that does not resolve starts over", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			d := mothDevice()
			d.Device = otherID
			h.app.reconcile(&config.Config{Devices: []config.Device{d}})
			if _, ok := activeGeometry(h.t, h.app, "moth"); ok {
				h.t.Error("capture-period condition still active after the id changed")
			}
			h.app.reconcile(&config.Config{Devices: []config.Device{mothDevice()}})
			h.wantSteps(0, 0, 1, 0)
		}, nil},
		{"a removed device starts over", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			h.app.reconcile(&config.Config{})
			h.app.reconcile(&config.Config{Devices: []config.Device{mothDevice()}})
			h.wantSteps(0, 0, 1, 0)
			if _, ok := activeGeometry(h.t, h.app, "moth"); ok {
				h.t.Error("capture-period condition still active after the device was removed")
			}
		}, nil},
		{"a disabled device starts over", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.shortfall()
			off := false
			d := mothDevice()
			d.Enabled = &off
			h.app.reconcile(&config.Config{Devices: []config.Device{d}})
			h.app.reconcile(&config.Config{Devices: []config.Device{mothDevice()}})
			h.wantSteps(0, 0, 1, 0)
			if _, ok := activeGeometry(h.t, h.app, "moth"); ok {
				h.t.Error("capture-period condition still active after the device was disabled")
			}
		}, nil},
		{"an unchanged save keeps the step", func(h *geoHarness, _ *fakeHost) {
			h.shortfall()
			h.die(shortfallErr())
			h.app.reconcile(&config.Config{Devices: []config.Device{mothDevice()}})
			h.wantSteps(0, 0, 1)
		}, nil},
		{"a card-index id never escalates", func(h *geoHarness, _ *fakeHost) {
			for range 2 {
				h.die(shortfallErr())
				h.app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", "hw:1,0", "/m", 48000)}})
			}
			h.wantSteps(0, 0, 0)
			h.wantGeometry(0, 0)
		}, func() config.Device { return testDevice("moth", "hw:1,0", "/m", 48000) }},
		{"a disconnect keeps the step", func(h *geoHarness, host *fakeHost) {
			h.shortfall()
			h.shortfall()
			full := host.devs
			host.devs = nil
			h.die(plain)
			host.devs = full
			h.app.retryDown()
			h.wantSteps(0, 0, 1, 1)
		}, nil},
		{"no condition is claimed for a step that never opened", func(h *geoHarness, host *fakeHost) {
			h.shortfall()
			h.die(shortfallErr())
			host.devs = nil
			<-h.app.retryDue
			h.app.onRetryDue()
			h.wantSteps(0, 0)
			h.wantGeometry(0, 0)
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := mothDevice()
				if tc.cfg != nil {
					cfg = tc.cfg()
				}
				h, host, stop := newGeoHarness(t, &cfg)
				defer stop()
				tc.run(h, host)
			})
		})
	}
}

// failingStepOpener wraps a fake opener so opens at a ladder step for which
// failAt returns an error fail with it.
func failingStepOpener(next func(*config.Device, int, *levels.Hub) (*deviceRuntime, error), log *fakeOpenLog, failAt func(step int) error) func(*config.Device, int, *levels.Hub) (*deviceRuntime, error) {
	return func(dev *config.Device, step int, hub *levels.Hub) (*deviceRuntime, error) {
		if err := failAt(step); err != nil {
			log.add("open:" + dev.Name + "@" + dev.Device)
			log.addStep(dev.Name, step)
			return nil, err
		}
		return next(dev, step, hub)
	}
}

// TestEscalatedOpenFailureFallsBack pins that an escalated open the driver
// refuses goes back to the last step that worked, without skipping the device
// and without asking for the refused step again, while busy and gone errors
// (which say nothing about the geometry) are not retried at another step.
func TestEscalatedOpenFailureFallsBack(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		fallback bool
	}{
		{"geometry error", &capture.GeometryError{Err: errors.New("no accepted geometry")}, true},
		{"bad rate error", &capture.BadRateError{Requested: 48000}, true},
		{"commit ENOMEM", fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM), true},
		{"busy", capture.ErrDeviceInUse, false},
		{"gone", capture.ErrDeviceGone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, stop := newGeoHarness(t, new(mothDevice()))
				defer stop()
				app := h.app
				app.open = failingStepOpener(app.open, h.log, func(step int) error {
					if step == 2 {
						return tc.err
					}
					return nil
				})
				for range 4 {
					h.shortfall()
				}
				if tc.fallback {
					h.wantSteps(0, 0, 1, 1, 2, 1)
					if s := app.devices["moth"].currentState(); s != mgmtserver.StateServing {
						t.Fatalf("state = %s, want serving after the fallback", s)
					}
					n, ok := activeGeometry(t, app, "moth")
					if !ok || !strings.Contains(n.Message, "20 ms period and 8 periods") {
						t.Errorf("condition = %q, %v; want it to name the step that opened", n.Message, ok)
					}
					if st := app.geometry["moth"]; st == nil || st.ceiling != 1 {
						t.Errorf("geometry state = %+v, want the ceiling pinned at 1", st)
					}
					h.shortfall()
					h.shortfall()
					h.wantSteps(0, 0, 1, 1, 2, 1, 1, 1)
					return
				}
				h.wantSteps(0, 0, 1, 1, 2)
				if s := app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
					t.Errorf("state = %s, want skipped as before escalation", s)
				}
				if st := app.geometry["moth"]; st == nil || st.step != 2 || st.ceiling != 3 {
					t.Errorf("geometry state = %+v, want step 2 with the ceiling untouched", st)
				}
			})
		})
	}
}

// TestEscalatedOpenFailureDownToDefaultClearsCondition pins that when every
// escalated step fails the device opens at the default and the condition that an
// earlier step raised is cleared.
func TestEscalatedOpenFailureDownToDefaultClearsCondition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		failFrom := 99
		h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
			if step >= failFrom {
				return fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM)
			}
			return nil
		})
		for range 4 {
			h.shortfall()
		}
		h.wantGeometry(1, 0)
		failFrom = 1
		h.die(shortfallErr())
		h.retry()
		h.wantSteps(0, 0, 1, 1, 2, 2, 1, 0)
		h.wantGeometry(1, 1)
		if _, ok := activeGeometry(t, h.app, "moth"); ok {
			t.Error("capture-period condition still active at the default period")
		}
	})
}

// TestCapturePeriodConditionAndLogNameGrantedGeometry pins that the capture log
// line and the condition report what the driver granted, not what the ladder
// asked for, and that step 0 keeps its log line without a period suffix.
func TestCapturePeriodConditionAndLogNameGrantedGeometry(t *testing.T) {
	logBuf := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		if got := logBuf.String(); !strings.Contains(got, `capture "moth"`) || strings.Contains(got, ", period ") {
			t.Fatalf("step 0 log = %q, want a capture line without a period suffix", got)
		}
		next := h.app.open
		h.app.open = func(dev *config.Device, step int, hub *levels.Hub) (*deviceRuntime, error) {
			rt, err := next(dev, step, hub)
			if err == nil && step == 1 {
				// The driver moved the requested 20 ms x 8 to 40 ms x 8.
				rt.geometry = audio.Geometry{PeriodFrames: 1920, Periods: 8}
			}
			return rt, err
		}
		h.shortfall()
		h.shortfall()
		if got := logBuf.String(); !strings.Contains(got, "period 1920 frames x 8") {
			t.Errorf("log = %q, want the granted period 1920 frames x 8", got)
		}
		n, ok := activeGeometry(t, h.app, "moth")
		if !ok || !strings.Contains(n.Message, "40 ms period and 8 periods (320 ms buffer)") {
			t.Errorf("condition = %q, %v; want the granted 40 ms x 8", n.Message, ok)
		}
	})
}

// TestOnRetryDuePrunesGeometryOfUnconfiguredDevice pins the backstop that
// drops a geometry state, and resolves its condition, for a name that is no
// longer an enabled configured device even when reconcile did not run.
func TestOnRetryDuePrunesGeometryOfUnconfiguredDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		h.shortfall()
		h.shortfall()
		if _, ok := activeGeometry(t, h.app, "moth"); !ok {
			t.Fatal("no capture-period condition before the prune")
		}
		h.app.cfg = config.Config{}
		h.app.onRetryDue()
		if h.app.geometry["moth"] != nil {
			t.Error("geometry state survived for an unconfigured device")
		}
		if _, ok := activeGeometry(t, h.app, "moth"); ok {
			t.Error("capture-period condition still active for an unconfigured device")
		}
	})
}

// TestFallbackLogNamesGrantedGeometry pins that the line logged when an
// escalated open falls back names the geometry the driver granted for the step
// that opened, not the ladder's request for it.
func TestFallbackLogNamesGrantedGeometry(t *testing.T) {
	logBuf := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		next := failingStepOpener(h.app.open, h.log, func(step int) error {
			if step == 2 {
				return fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM)
			}
			return nil
		})
		h.app.open = func(dev *config.Device, step int, hub *levels.Hub) (*deviceRuntime, error) {
			rt, err := next(dev, step, hub)
			if err == nil && step == 1 {
				// The driver moved the requested 20 ms x 8 to 40 ms x 8.
				rt.geometry = audio.Geometry{PeriodFrames: 1920, Periods: 8}
			}
			return rt, err
		}
		for range 4 {
			h.shortfall()
		}
		h.wantSteps(0, 0, 1, 1, 2, 1)
		if got := logBuf.String(); !strings.Contains(got, "using a 40 ms period and 8 periods (320 ms buffer)") {
			t.Errorf("log = %q, want the fallback line to name the granted 40 ms x 8", got)
		}
	})
}

// TestGeometryRefused pins which open errors count as the driver refusing a
// geometry (and so lower the ceiling) and which do not.
func TestGeometryRefused(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"geometry error", &capture.GeometryError{Err: syscall.EINVAL}, true},
		{"bad rate error", &capture.BadRateError{Requested: 48000}, true},
		{"wrapped commit ENOMEM", fmt.Errorf("open capture: %w", fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM)), true},
		{"raw commit EINVAL", fmt.Errorf("alsa: HW_PARAMS: %w", syscall.EINVAL), true},
		{"EIO right after open", fmt.Errorf("open capture: %w", syscall.EIO), false},
		{"negotiated below open count", errors.New("audio: device negotiated 1 channels but 2 were requested"), false},
		{"device in use", capture.ErrDeviceInUse, false},
		{"device gone", capture.ErrDeviceGone, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := geometryRefused(tc.err); got != tc.want {
			t.Errorf("%s: geometryRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTransientEscalatedOpenFailureDoesNotPinCeiling pins that an open error
// that does not mean the driver refused the geometry falls back for that open
// only: the ceiling and the active condition stay as they were, and the next
// open asks for the escalated step again.
func TestTransientEscalatedOpenFailureDoesNotPinCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		failures := 1
		h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
			if step == 2 && failures > 0 {
				failures--
				return fmt.Errorf("open capture: %w", syscall.EIO)
			}
			return nil
		})
		for range 4 {
			h.shortfall()
		}
		h.wantSteps(0, 0, 1, 1, 2, 1)
		if s := h.app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("state = %s, want serving after the fallback", s)
		}
		if st := h.app.geometry["moth"]; st == nil || st.step != 2 || st.ceiling != 3 {
			t.Errorf("geometry state = %+v, want step 2 and the ceiling untouched", st)
		}
		n, ok := activeGeometry(t, h.app, "moth")
		if !ok || !strings.Contains(n.Message, "20 ms period and 8 periods") {
			t.Errorf("condition = %q, %v; want it untouched at step 1", n.Message, ok)
		}
		h.wantGeometry(1, 0)

		h.die(errors.New("alsa: read failed"))
		h.retry()
		h.wantSteps(0, 0, 1, 1, 2, 1, 2)
		n, ok = activeGeometry(t, h.app, "moth")
		if !ok || !strings.Contains(n.Message, "40 ms period and 8 periods") {
			t.Errorf("condition = %q, %v; want step 2 once it opened", n.Message, ok)
		}
		h.wantGeometry(1, 0)
	})
}

// TestRefusedAboveTransientFallbackSetsCeilingBelowRefusal pins the ceiling
// bookkeeping when one fallback chain mixes errors: step 3 is refused and step
// 2 fails transiently, so the device serves at step 1 but the ceiling only
// drops to 2 (below the refusal, not to the step that opened).
func TestRefusedAboveTransientFallbackSetsCeilingBelowRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		armed := false
		h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
			switch {
			case armed && step == 3:
				return fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM)
			case armed && step == 2:
				return fmt.Errorf("open capture: %w", syscall.EIO)
			}
			return nil
		})
		for range 5 {
			h.shortfall()
		}
		armed = true
		h.shortfall()
		h.wantSteps(0, 0, 1, 1, 2, 2, 3, 2, 1)
		if s := h.app.devices["moth"].currentState(); s != mgmtserver.StateServing {
			t.Fatalf("state = %s, want serving at step 1", s)
		}
		if st := h.app.geometry["moth"]; st == nil || st.ceiling != 2 || st.step != 2 {
			t.Errorf("geometry state = %+v, want step 2 and ceiling 2", st)
		}
	})
}

// TestFallbackToDefaultClearsCondition pins that an open that lands at the
// default period after an escalated ask clears the capture-period condition,
// whether the larger sizes were refused or failed transiently, and that the
// escalated step stays in state unless the ceiling forbids it.
func TestFallbackToDefaultClearsCondition(t *testing.T) {
	enomem := fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM)
	eio := fmt.Errorf("open capture: %w", syscall.EIO)
	cases := []struct {
		name                  string
		at2, at1              error
		wantStep, wantCeiling int
		wantMsg               string
	}{
		{"refused then transient", enomem, eio, 1, 1, "did not open this time"},
		{"transient then transient", eio, eio, 2, 3, "did not open this time"},
		{"refused down to the default", enomem, enomem, 0, 0, "driver refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, stop := newGeoHarness(t, new(mothDevice()))
				defer stop()
				armed := false
				h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
					switch {
					case armed && step == 2:
						return tc.at2
					case armed && step == 1:
						return tc.at1
					}
					return nil
				})
				for range 3 {
					h.shortfall()
				}
				h.wantGeometry(1, 0)
				armed = true
				h.shortfall()
				h.wantSteps(0, 0, 1, 1, 2, 1, 0)
				h.wantGeometry(1, 1)
				if _, ok := activeGeometry(t, h.app, "moth"); ok {
					t.Error("capture-period condition still active while capturing at the default period")
				}
				if got := lastNotification(t, applianceCenter(t, h.app)).Message; !strings.Contains(got, tc.wantMsg) {
					t.Errorf("clear message = %q, want it to contain %q", got, tc.wantMsg)
				}
				if st := h.app.geometry["moth"]; st == nil || st.step != tc.wantStep || st.ceiling != tc.wantCeiling {
					t.Errorf("geometry state = %+v, want step %d and ceiling %d", st, tc.wantStep, tc.wantCeiling)
				}
			})
		})
	}
}

// TestShortfallFromFallbackRunDoesNotAdvanceLadder pins that a shortfall counts
// only against the step the failing run actually opened at: a run that fell
// back below the step in state neither counts nor resets, so the ladder does
// not climb on evidence from a geometry it did not ask for.
func TestShortfallFromFallbackRunDoesNotAdvanceLadder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		armed := false
		h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
			if armed && step >= 2 {
				return fmt.Errorf("open capture: %w", syscall.EIO)
			}
			return nil
		})
		for range 3 {
			h.shortfall()
		}
		armed = true
		for range 3 {
			h.shortfall()
		}
		h.wantSteps(0, 0, 1, 1, 2, 1, 2, 1, 2, 1)
		if st := h.app.geometry["moth"]; st == nil || st.step != 2 || st.shortfalls != 0 {
			t.Errorf("geometry state = %+v, want step 2 with no shortfalls counted", st)
		}
	})
}

// TestRefusalThenTotalOpenFailureLowersCeiling pins that a refusal is recorded
// even when the whole open then fails, so the next retry does not ask for a
// geometry the driver already refused.
func TestRefusalThenTotalOpenFailureLowersCeiling(t *testing.T) {
	cases := []struct {
		name  string
		at1   error
		at0   error
		steps []int
	}{
		{"chain exhausted", fmt.Errorf("open capture: %w", syscall.EIO), fmt.Errorf("open capture: %w", syscall.EIO), []int{0, 0, 1, 1, 2, 1, 0}},
		{"busy below the refusal", capture.ErrDeviceInUse, nil, []int{0, 0, 1, 1, 2, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, stop := newGeoHarness(t, new(mothDevice()))
				defer stop()
				armed := false
				h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
					switch {
					case armed && step == 2:
						return fmt.Errorf("alsa: HW_PARAMS: %w", syscall.ENOMEM)
					case armed && step == 1:
						return tc.at1
					case armed && step == 0:
						return tc.at0
					}
					return nil
				})
				for range 3 {
					h.shortfall()
				}
				armed = true
				h.die(shortfallErr())
				h.retry()
				h.wantSteps(tc.steps...)
				if s := h.app.devices["moth"].currentState(); s != mgmtserver.StateSkipped {
					t.Fatalf("state = %s, want skipped when every open fails", s)
				}
				if st := h.app.geometry["moth"]; st == nil || st.ceiling != 1 || st.step != 1 {
					t.Errorf("geometry state = %+v, want the refusal recorded as ceiling 1 and step 1", st)
				}
			})
		})
	}
}

// TestRunEndedLogNamesOpenedGeometry pins that the reset log names the geometry
// the run actually served at, not the step in state, when the run had fallen back.
func TestRunEndedLogNamesOpenedGeometry(t *testing.T) {
	logBuf := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		armed := false
		h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
			if armed && step == 2 {
				return fmt.Errorf("open capture: %w", syscall.EIO)
			}
			return nil
		})
		for range 3 {
			h.shortfall()
		}
		armed = true
		h.shortfall()
		h.wantSteps(0, 0, 1, 1, 2, 1)
		runFor(t, h.app, hour61)
		h.die(errors.New("alsa: read failed"))
		if got := logBuf.String(); !strings.Contains(got, "at a 20 ms period and 8 periods (160 ms buffer); its next open uses the default") {
			t.Errorf("log = %q, want the reset line to name the step 1 geometry the run served at", got)
		}
	})
}

// TestTransientFallbackLeavesConditionAtEscalatedStep pins that a transient
// fallback to another escalated step leaves the condition as it was, naming the
// step the next open asks for again, while the device serves one step lower.
func TestTransientFallbackLeavesConditionAtEscalatedStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, stop := newGeoHarness(t, new(mothDevice()))
		defer stop()
		armed := false
		h.app.open = failingStepOpener(h.app.open, h.log, func(step int) error {
			if armed && step == 2 {
				return fmt.Errorf("open capture: %w", syscall.EIO)
			}
			return nil
		})
		for range 4 {
			h.shortfall()
		}
		n, ok := activeGeometry(t, h.app, "moth")
		if !ok || !strings.Contains(n.Message, "40 ms period and 8 periods") {
			t.Fatalf("condition = %q, %v; want step 2 before the fallback", n.Message, ok)
		}
		armed = true
		h.die(errors.New("alsa: read failed"))
		h.retry()
		h.wantSteps(0, 0, 1, 1, 2, 2, 1)
		n, ok = activeGeometry(t, h.app, "moth")
		if !ok || !strings.Contains(n.Message, "40 ms period and 8 periods") {
			t.Errorf("condition = %q, %v; want it left at step 2", n.Message, ok)
		}
	})
}
