//go:build linux

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

const (
	addrHW3    = "hw:3,0"
	addrHW4    = "hw:4,0"
	addrHW5    = "hw:5,0"
	idScarlett = "usb:1235:8218:s=S1:if=0,0"
	idMoth     = "usb:16d0:06f3:p=0000:01:00.0-1.1:if=0,0"

	// titleDisconnected is the onset title a lost device raises; several tests
	// assert it, so it lives here rather than as a repeated literal.
	titleDisconnected = "Device disconnected"
)

// fakeHost is a host device list the appliance resolves configured ids against,
// standing in for the capture library's sysfs resolution. An id resolves when it
// equals a device's stable id or its current address (as a card-index id does);
// no match is *DeviceNotFoundError and several are *AmbiguousDeviceError, which is
// the library's contract.
type fakeHost struct {
	devs []audio.Hardware
	// fail, when set, is returned for every resolution, as when the host has no
	// readable device listing.
	fail error
}

func (h *fakeHost) resolve(id string) (audio.Hardware, error) {
	if h.fail != nil {
		// The library wraps a wholesale enumeration failure (no readable device
		// listing) in capture.ErrDeviceGone, so a caller that classified it by
		// errors.Is(ErrDeviceGone) instead of the typed *DeviceNotFoundError would
		// misread it as a specific absent device. Wrap it the same way so the tests
		// pin the errors.As (typed) classification, not errors.Is.
		return audio.Hardware{}, fmt.Errorf("%w: %w", capture.ErrDeviceGone, h.fail)
	}
	var hit []audio.Hardware
	for _, d := range h.devs {
		if d.ID == id || d.HWAddr == id {
			hit = append(hit, d)
		}
	}
	switch len(hit) {
	case 0:
		return audio.Hardware{}, &capture.DeviceNotFoundError{ID: id}
	case 1:
		return hit[0], nil
	}
	matches := make([]string, 0, len(hit))
	for _, d := range hit {
		matches = append(matches, d.HWAddr)
	}
	return audio.Hardware{}, &capture.AmbiguousDeviceError{ID: id, Matches: matches}
}

// withHost points the appliance's resolver at host.
func withHost(app *appliance, host *fakeHost) { app.resolve = host.resolve }

// opened reports whether the fake opener was asked to open the named device.
func opened(log *fakeOpenLog, name string) bool {
	return slices.ContainsFunc(log.snapshot(), func(e string) bool { return strings.HasPrefix(e, "open:"+name+"@") })
}

// TestReconcileBindsEachDeviceToItsIdentityAcrossReorder is the #62 reboot case:
// the kernel numbered the two cards in the opposite order from when they were
// provisioned. Each entry must open by its own stable id and report the address
// its own hardware now has, so neither stream serves the other microphone.
func TestReconcileBindsEachDeviceToItsIdentityAcrossReorder(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	// After the reboot the AudioMoth probed first and took card 3.
	withHost(app, &fakeHost{devs: []audio.Hardware{
		{ID: idMoth, HWAddr: addrHW3, Label: nameAudioMoth, IDStable: true},
		{ID: idScarlett, HWAddr: addrHW4, Label: nameScarlett, IDStable: true},
	}})

	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("scarlett", idScarlett, "/s", 48000),
		testDevice("moth", idMoth, "/m", 48000),
	}})

	for name, want := range map[string]string{"scarlett": addrHW4, "moth": addrHW3} {
		rt := app.devices[name]
		if rt.currentState() != mgmtserver.StateServing {
			t.Fatalf("%s state = %s (%s), want serving", name, rt.currentState(), rt.err)
		}
		if st := rt.status(); st.HWAddr != want || !st.IDStable {
			t.Errorf("%s status hwAddr=%q idStable=%v, want %q and true", name, st.HWAddr, st.IDStable, want)
		}
	}
	for _, want := range []string{"open:scarlett@" + idScarlett, "open:moth@" + idMoth} {
		if !slices.Contains(log.snapshot(), want) {
			t.Errorf("open log %v lacks %q: the open must pass the stable id, not a card index", log.snapshot(), want)
		}
	}
}

// TestReconcileRefusesAbsentDevice pins "refuse, don't guess": an entry whose
// hardware is not connected is skipped with a not-connected reason and is never
// opened, even though another device that would accept its rate is present.
func TestReconcileRefusesAbsentDevice(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	withHost(app, &fakeHost{devs: []audio.Hardware{{ID: idScarlett, HWAddr: addrHW3, IDStable: true}}})

	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("scarlett", idScarlett, "/s", 48000),
		testDevice("moth", idMoth, "/m", 48000),
	}})

	rt := app.devices["moth"]
	if rt.currentState() != mgmtserver.StateSkipped || !strings.Contains(rt.err, "not connected") {
		t.Fatalf("moth = %s %q, want skipped as not connected", rt.currentState(), rt.err)
	}
	if opened(log, "moth") {
		t.Errorf("an absent device was opened: %v", log.snapshot())
	}
	act := applianceCenter(t, app).Active()
	if len(act) != 1 || act[0].Key != deviceDownKey("moth") || act[0].Title != "Device not connected" {
		t.Errorf("active = %+v, want one Device not connected condition for moth", act)
	}
}

// TestReconcileRefusesAmbiguousDevice pins that an id matching two units is
// never opened (the library refuses to pick one, and so does the appliance).
func TestReconcileRefusesAmbiguousDevice(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	withHost(app, &fakeHost{devs: []audio.Hardware{
		{ID: idMoth, HWAddr: addrHW3, IDStable: true},
		{ID: idMoth, HWAddr: addrHW4, IDStable: true},
	}})

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})

	rt := app.devices["moth"]
	if rt.currentState() != mgmtserver.StateSkipped || !strings.Contains(rt.err, "ambiguous") || !strings.Contains(rt.err, "hw:3,0, hw:4,0") {
		t.Fatalf("moth = %s %q, want skipped as ambiguous naming both addresses", rt.currentState(), rt.err)
	}
	if opened(log, "moth") {
		t.Errorf("an ambiguous device was opened: %v", log.snapshot())
	}
}

// TestReconcileRefusesSecondEntryForSameHardware pins that two entries naming
// one physical device through different ids (a stable id and a card index) do
// not both try to open it: the second is refused with the owner's name.
func TestReconcileRefusesSecondEntryForSameHardware(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	withHost(app, &fakeHost{devs: []audio.Hardware{{ID: idScarlett, HWAddr: addrHW3, IDStable: true}}})

	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("scarlett", idScarlett, "/s", 48000),
		testDevice("alias", addrHW3, "/a", 48000),
	}})

	if app.devices["scarlett"].currentState() != mgmtserver.StateServing {
		t.Fatalf("scarlett state = %s, want serving", app.devices["scarlett"].currentState())
	}
	rt := app.devices["alias"]
	if rt.currentState() != mgmtserver.StateSkipped || !strings.Contains(rt.err, `same hardware as "scarlett"`) {
		t.Fatalf("alias = %s %q, want skipped as the same hardware as scarlett", rt.currentState(), rt.err)
	}
	if opened(log, "alias") {
		t.Errorf("the duplicate entry was opened: %v", log.snapshot())
	}
	if rt.status().IDStable {
		t.Error("a card-index entry reported idStable=true")
	}
}

// TestReconcilePublishesResolvedIDsAsConfigured pins that a card-index entry
// hides its hardware from the available list under the stable id the
// enumeration reports, so the same device is not offered for provisioning twice.
func TestReconcilePublishesResolvedIDsAsConfigured(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	withHost(app, &fakeHost{devs: []audio.Hardware{{ID: idScarlett, HWAddr: addrHW3, IDStable: true}}})

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("byindex", addrHW3, "/a", 48000)}})

	ids := app.prov.configuredIDs()
	if !ids[addrHW3] || !ids[idScarlett] {
		t.Errorf("configured ids = %v, want both the configured hw:3,0 and its stable id", ids)
	}
}

// TestReconcileOpensCardIndexWhenHostHasNoListing pins the container fallback: a
// card-index id whose resolution fails for a reason other than absence (no
// device listing to resolve against) is still opened, since the open addresses
// the card directly, while a stable id with the same failure is refused.
func TestReconcileOpensCardIndexWhenHostHasNoListing(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	withHost(app, &fakeHost{fail: errors.New("no /proc/asound")})

	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("byindex", addrHW3, "/a", 48000),
		testDevice("stable", idScarlett, "/s", 48000),
	}})

	if st := app.devices["byindex"].currentState(); st != mgmtserver.StateServing || !opened(log, "byindex") {
		t.Errorf("byindex state = %s, want opened and serving", st)
	}
	if rt := app.devices["stable"]; rt.currentState() != mgmtserver.StateSkipped || opened(log, "stable") {
		t.Errorf("stable state = %s (opened=%v), want skipped without an open", rt.currentState(), opened(log, "stable"))
	}
}

// TestReconcileRefusesCardIndexWithNoCardAtIndex pins T4: a card-index id whose
// index names no present card resolves to *DeviceNotFoundError, which is refused
// as not connected and never opened. This is the contrast to the container
// fallback above: only a resolve failure that is NOT a specific absent device
// (nor an ambiguous one) lets a card-index id open unresolved.
func TestReconcileRefusesCardIndexWithNoCardAtIndex(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	// A card is present, but not at index 3, so hw:3,0 matches nothing.
	withHost(app, &fakeHost{devs: []audio.Hardware{{ID: idScarlett, HWAddr: addrHW4, IDStable: true}}})

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("byindex", addrHW3, "/a", 48000)}})

	rt := app.devices["byindex"]
	if rt.currentState() != mgmtserver.StateSkipped || !strings.Contains(rt.err, "not connected") {
		t.Fatalf("byindex = %s %q, want skipped as not connected", rt.currentState(), rt.err)
	}
	if opened(log, "byindex") {
		t.Errorf("a card index naming no present card was opened: %v", log.snapshot())
	}
}

// TestRetryDownStartsReconnectedDevice is the hotplug case: a device that was
// absent comes back on a new card index. The hardware-change retry starts it on
// its new address with no config change and clears its down condition, and
// leaves the device that was already serving alone.
func TestRetryDownStartsReconnectedDevice(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{devs: []audio.Hardware{{ID: idScarlett, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("scarlett", idScarlett, "/s", 48000),
		testDevice("moth", idMoth, "/m", 48000),
	}})
	if app.devices["moth"].currentState() != mgmtserver.StateSkipped {
		t.Fatalf("moth state = %s, want skipped while absent", app.devices["moth"].currentState())
	}
	scarlett := app.devices["scarlett"]

	genBefore := app.announceGen
	host.devs = append(host.devs, audio.Hardware{ID: idMoth, HWAddr: addrHW5, IDStable: true})
	app.retryDown()

	rt := app.devices["moth"]
	if rt.currentState() != mgmtserver.StateServing || rt.hwAddr != addrHW5 {
		t.Fatalf("moth = %s at %q, want serving at hw:5,0", rt.currentState(), rt.hwAddr)
	}
	if app.announceGen <= genBefore {
		t.Errorf("announceGen = %d, want > %d: starting a reconnected device must rebuild the mDNS advertisement", app.announceGen, genBefore)
	}
	if !app.srv.HasTrack("/m") {
		t.Error("the reconnected device's RTSP track was not registered")
	}
	if app.devices["scarlett"] != scarlett {
		t.Error("the retry restarted a device that was already serving")
	}
	if n := strings.Count(strings.Join(log.snapshot(), " "), "open:scarlett@"); n != 1 {
		t.Errorf("scarlett opened %d times, want 1", n)
	}
	if act := applianceCenter(t, app).Active(); len(act) != 0 {
		t.Errorf("active after reconnect = %+v, want the down condition cleared", act)
	}
}

// TestDisconnectThenAbsentReraisesWithNewCause pins the cause tracking: a device
// lost mid-stream is reported as disconnected, and when the retry then finds it
// absent the condition is re-raised as not connected rather than kept at the
// first cause by the idempotent onset.
func TestDisconnectThenAbsentReraisesWithNewCause(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
	center := applianceCenter(t, app)

	rt := app.devices["moth"]
	app.stop(rt) // retire the fake source so the pump goroutine ends
	rt.superseded = false
	var drain pumpResult
	select {
	case drain = <-app.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the retired pump to report done")
	}
	app.onPumpDone(pumpResult{rt: drain.rt, err: capture.ErrDeviceGone})
	if !app.prov.retryArmed.Load() {
		t.Error("a lost-device pump failure did not arm a retry")
	}
	if act := center.Active(); len(act) != 1 || act[0].Title != titleDisconnected {
		t.Fatalf("active after the loss = %+v, want one Device disconnected", act)
	}

	host.devs = nil
	app.retryDown()
	if act := center.Active(); len(act) != 1 || act[0].Title != "Device not connected" {
		t.Errorf("active after the retry = %+v, want the condition re-raised as Device not connected", act)
	}
}

// TestRetryDownSkipsCardIndexEntry pins H1: a down card-index entry is NOT
// restarted by an unattended hardware-change retry. Its index names a card by
// kernel probe order, so after a hardware change the index may name a different
// microphone than when the entry went down (the #62 swap). Restarting it here
// could open the wrong device, so it waits for an explicit config save.
func TestRetryDownSkipsCardIndexEntry(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("byindex", addrHW3, "/a", 48000),
	}})
	if app.devices["byindex"].currentState() != mgmtserver.StateSkipped {
		t.Fatalf("byindex state = %s, want skipped while nothing is at hw:3,0", app.devices["byindex"].currentState())
	}

	// A different microphone now occupies card index 3; the entry only ever
	// matched the bare index, not this device's stable id.
	host.devs = []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}
	app.retryDown()

	rt := app.devices["byindex"]
	if rt.currentState() != mgmtserver.StateSkipped {
		t.Errorf("byindex = %s, want still skipped; a card-index entry must not restart unattended", rt.currentState())
	}
	if opened(log, "byindex") {
		t.Errorf("a card-index entry was reopened onto different hardware on a hardware change: %v", log.snapshot())
	}
}

// TestPersistentNonHardwareFailureDoesNotArm pins the fix for a device that opens
// fine and then keeps dying for a non-hardware reason (a deterministic encoder
// fault, an EIO right after open): while the device is still present, the failure
// is reported as failed and does NOT arm an unattended retry. Arming it would
// restart and re-notify the device every enumeration tick, flapping the down
// condition forever.
func TestPersistentNonHardwareFailureDoesNotArm(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
	center := applianceCenter(t, app)

	rt := app.devices["moth"]
	app.stop(rt) // retire the fake source so the pump goroutine ends
	rt.superseded = false
	var drain pumpResult
	select {
	case drain = <-app.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the retired pump to report done")
	}
	// The device is still present in the host listing, so this is a fault in a
	// present device, not a loss.
	app.onPumpDone(pumpResult{rt: drain.rt, err: errors.New("encoder fault")})

	if app.prov.retryArmed.Load() {
		t.Error("a persistent non-hardware failure armed a retry; it would flap the condition forever")
	}
	if act := center.Active(); len(act) != 1 || act[0].Title != "Device failed" {
		t.Fatalf("active after the failure = %+v, want one Device failed", act)
	}
}

// TestAbsentDeviceFailureIsDisconnectAndArms pins that a pump death whose error is
// NOT ErrDeviceGone but whose device no longer resolves is treated as a loss:
// ErrDeviceGone does not cover every lost-device error, so onPumpDone re-resolves
// the id and, finding it absent, reports a disconnect and arms a retry (a
// same-index replug within one enumeration tick must still be retried).
func TestAbsentDeviceFailureIsDisconnectAndArms(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	host := &fakeHost{devs: []audio.Hardware{{ID: idMoth, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("moth", idMoth, "/m", 48000)}})
	center := applianceCenter(t, app)

	rt := app.devices["moth"]
	app.stop(rt) // retire the fake source so the pump goroutine ends
	rt.superseded = false
	var drain pumpResult
	select {
	case drain = <-app.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the retired pump to report done")
	}
	// The unplug removed the device from the host listing, and the pump reported
	// a raw errno rather than ErrDeviceGone.
	host.devs = nil
	app.onPumpDone(pumpResult{rt: drain.rt, err: errors.New("file descriptor in bad state")})

	if !app.prov.retryArmed.Load() {
		t.Error("a lost device (absent on re-resolve) did not arm a retry")
	}
	if act := center.Active(); len(act) != 1 || act[0].Title != titleDisconnected {
		t.Fatalf("active after the loss = %+v, want one Device disconnected", act)
	}
}

// TestCardIndexDeviceLostRestartsOnConfigSave pins that when a card-index device is
// lost, the disconnected message tells the operator it restarts on the next config
// save, not "when reconnected": retryDown never restarts a card-index entry
// unattended, since its index may name different hardware after a reconnect.
func TestCardIndexDeviceLostRestartsOnConfigSave(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	// byindex binds the bare card index hw:3,0, which matches the present card's
	// address, so it opens and serves.
	host := &fakeHost{devs: []audio.Hardware{{ID: idScarlett, HWAddr: addrHW3, IDStable: true}}}
	withHost(app, host)
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("byindex", addrHW3, "/a", 48000)}})
	if app.devices["byindex"].currentState() != mgmtserver.StateServing {
		t.Fatalf("byindex state = %s, want serving", app.devices["byindex"].currentState())
	}
	center := applianceCenter(t, app)

	rt := app.devices["byindex"]
	app.stop(rt) // retire the fake source so the pump goroutine ends
	rt.superseded = false
	var drain pumpResult
	select {
	case drain = <-app.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the retired pump to report done")
	}
	app.onPumpDone(pumpResult{rt: drain.rt, err: capture.ErrDeviceGone})

	act := center.Active()
	if len(act) != 1 || act[0].Title != titleDisconnected {
		t.Fatalf("active after the loss = %+v, want one Device disconnected", act)
	}
	if !strings.Contains(act[0].Message, "restarts on the next config save") {
		t.Errorf("card-index disconnect message = %q, want it to say it restarts on the next config save", act[0].Message)
	}
}
