//go:build linux

package main

import (
	"errors"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// failOpen makes the appliance's device opener fail, so a reconcile skips the
// device and takes the open-failure path.
func failOpen(app *appliance) {
	app.open = func(*config.Device, *levels.Hub) (*deviceRuntime, error) {
		return nil, errors.New("open boom")
	}
}

// applianceCenter returns the notification center wired into the test appliance.
func applianceCenter(t *testing.T, app *appliance) *notify.Center {
	t.Helper()
	c, ok := app.notifier.(*notify.Center)
	if !ok {
		t.Fatalf("appliance notifier is %T, want *notify.Center", app.notifier)
	}
	return c
}

// lastNotification returns the most recent ring entry, failing when the ring is
// empty rather than indexing out of range.
func lastNotification(t *testing.T, c *notify.Center) notify.Notification {
	t.Helper()
	ns := c.Snapshot().Notifications
	if len(ns) == 0 {
		t.Fatal("no notifications were published")
	}
	return ns[len(ns)-1]
}

func TestReconcileOpenFailureEmitsDownOnset(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	center := applianceCenter(t, app)
	failOpen(app)

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})

	act := center.Active()
	if len(act) != 1 {
		t.Fatalf("active conditions = %d, want 1", len(act))
	}
	n := act[0]
	if n.Key != deviceDownKey("a") {
		t.Errorf("onset key = %q, want %q", n.Key, deviceDownKey("a"))
	}
	if n.Severity != notify.SeverityError || n.Category != notify.CategoryDevice || n.Kind != notify.KindOnset {
		t.Errorf("onset = %+v, want error/device/onset", n)
	}
	if n.Source != "a" {
		t.Errorf("onset source = %q, want a", n.Source)
	}
	if n.Title != "Device unavailable" {
		t.Errorf("onset title = %q, want Device unavailable", n.Title)
	}
	if got := app.devices["a"].status().DownCause; got != downOpenFailed {
		t.Errorf("DownCause after the failed open = %q, want %q", got, downOpenFailed)
	}
}

// TestDownCausesAreWireEnumMembers pins every down-condition class the
// appliance can record to the API's downCause enum, so a class added here
// without a spec change fails instead of putting an undeclared value on the
// wire.
func TestDownCausesAreWireEnumMembers(t *testing.T) {
	t.Parallel()
	for _, c := range []string{
		downNotConnected, downAmbiguous, downMalformed, downResolve,
		downSameHardware, downOpenFailed, downDisconnected, downFailed,
	} {
		if !mgmtapi.DeviceDownCause(c).Valid() {
			t.Errorf("down cause %q is not a member of the API downCause enum", c)
		}
	}
}

func TestReconcileRecoveryEmitsClear(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	center := applianceCenter(t, app)
	cfg := &config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}}

	// First reconcile: the open fails and raises the down onset.
	failOpen(app)
	app.reconcile(cfg)
	if len(center.Active()) != 1 {
		t.Fatalf("after the open failure, active = %d, want 1", len(center.Active()))
	}

	// Second reconcile: the open now succeeds, so the down condition clears.
	app.open = fakeOpener(log)
	app.reconcile(cfg)
	if got := len(center.Active()); got != 0 {
		t.Fatalf("after recovery, active = %d, want 0", got)
	}
	last := lastNotification(t, center)
	if last.Kind != notify.KindClear || last.Category != notify.CategoryDevice || last.Key != deviceDownKey("a") {
		t.Errorf("recovery entry = %+v, want a device clear on the down key", last)
	}
	if last.Severity != notify.SeverityInfo || last.Title != "Device recovered" {
		t.Errorf("recovery entry = %+v, want info/Device recovered", last)
	}
}

func TestOnPumpDoneDeathEmitsDownOnset(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	center := applianceCenter(t, app)

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})
	if got := len(center.Active()); got != 0 {
		t.Fatalf("a healthy start left %d active conditions, want 0", got)
	}

	// The re-resolve that decides lost versus failed is a host enumeration;
	// record what the API would report while it runs, so the record is proven
	// failed before it, not only afterwards.
	rt := app.devices["a"]
	var duringResolve mgmtserver.DeviceState
	resolve := app.resolve
	app.resolve = func(id string) (audio.Hardware, error) {
		duringResolve = rt.currentState()
		return resolve(id)
	}

	// Simulate the device dying after startup: its pump reports a non-nil error
	// while the appliance is not shutting down.
	app.onPumpDone(pumpResult{rt: rt, err: errors.New("device died")})
	if duringResolve != mgmtserver.StateFailed {
		t.Errorf("state during the re-resolve = %q, want %q", duringResolve, mgmtserver.StateFailed)
	}

	act := center.Active()
	if len(act) != 1 {
		t.Fatalf("after the death, active = %d, want 1", len(act))
	}
	n := act[0]
	if n.Key != deviceDownKey("a") || n.Severity != notify.SeverityError || n.Kind != notify.KindOnset {
		t.Errorf("death onset = %+v, want error/onset on the down key", n)
	}
	if n.Title != titleFailed {
		t.Errorf("death onset title = %q, want Device failed", n.Title)
	}
	if got := app.devices["a"].status().DownCause; got != downFailed {
		t.Errorf("DownCause after the death = %q, want %q", got, downFailed)
	}
}

func TestReconcileRemovedDownDeviceResolves(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	center := applianceCenter(t, app)
	failOpen(app)

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})
	if len(center.Active()) != 1 {
		t.Fatalf("want 1 active down condition after the open failure")
	}

	// Removing the device from the configuration resolves its dangling condition.
	app.reconcile(&config.Config{})
	if got := len(center.Active()); got != 0 {
		t.Fatalf("after removal, active = %d, want 0", got)
	}
	last := lastNotification(t, center)
	if last.Kind != notify.KindClear || last.Key != deviceDownKey("a") {
		t.Errorf("resolve entry = %+v, want a clear on the down key", last)
	}
}

func TestReconcileDisabledDownDeviceResolves(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	center := applianceCenter(t, app)
	failOpen(app)

	dev := testDevice("a", "hw:0", "/a", 48000)
	app.reconcile(&config.Config{Devices: []config.Device{dev}})
	if len(center.Active()) != 1 {
		t.Fatalf("want 1 active down condition after the open failure")
	}

	// Disabling the device resolves its condition rather than leaving it dangling.
	disabled := false
	dev.Enabled = &disabled
	app.reconcile(&config.Config{Devices: []config.Device{dev}})
	if got := len(center.Active()); got != 0 {
		t.Fatalf("after disable, active = %d, want 0", got)
	}
	last := lastNotification(t, center)
	if last.Kind != notify.KindClear || last.Key != deviceDownKey("a") {
		t.Errorf("resolve entry = %+v, want a clear on the down key", last)
	}
}

func TestReconcileServingDeviceRemovedEmitsNothing(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	center := applianceCenter(t, app)

	// A healthy device that never went down has no active condition, so removing it
	// must not publish a spurious resolve.
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})
	if got := len(center.Snapshot().Notifications); got != 0 {
		t.Fatalf("a healthy start published %d entries, want 0", got)
	}
	app.reconcile(&config.Config{})
	if got := len(center.Snapshot().Notifications); got != 0 {
		t.Errorf("removing a healthy serving device published %d entries, want 0", got)
	}
}

func TestReconcileHealthyDeviceDisabledEmitsNothing(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	center := applianceCenter(t, app)

	dev := testDevice("a", "hw:0", "/a", 48000)
	app.reconcile(&config.Config{Devices: []config.Device{dev}})
	if got := len(center.Snapshot().Notifications); got != 0 {
		t.Fatalf("a healthy start published %d entries, want 0", got)
	}
	// Disabling a healthy device has no active condition to resolve.
	disabled := false
	dev.Enabled = &disabled
	app.reconcile(&config.Config{Devices: []config.Device{dev}})
	if got := len(center.Snapshot().Notifications); got != 0 {
		t.Errorf("disabling a healthy serving device published %d entries, want 0", got)
	}
}

func TestReconcileNoDeviceChangeEmitsNothing(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	center := applianceCenter(t, app)
	cfg := &config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}}

	app.reconcile(cfg)
	before := len(center.Snapshot().Notifications)
	if before != 0 {
		t.Errorf("a healthy start published %d entries, want 0", before)
	}

	app.reconcile(cfg)
	if after := len(center.Snapshot().Notifications); after != before {
		t.Errorf("a no-op reconcile published %d new entries, want 0", after-before)
	}
}

func TestReconcileHealthyRestartEmitsNothing(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	center := applianceCenter(t, app)

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})
	oldRT := app.devices["a"]

	// A rate change restarts the device on the same path; a healthy restart is not
	// a recovery, so nothing is published.
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 44100)}})

	// Confirm the change actually restarted the device (a new runtime), so this
	// test cannot silently degrade into a duplicate of the no-op case if the
	// rate-change path stopped triggering a restart.
	if app.devices["a"] == oldRT {
		t.Fatal("the rate change did not restart the device (same runtime)")
	}
	if n := len(center.Snapshot().Notifications); n != 0 {
		t.Errorf("a healthy restart published %d entries, want 0", n)
	}
}
