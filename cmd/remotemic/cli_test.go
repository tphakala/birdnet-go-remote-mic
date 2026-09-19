//go:build linux

package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

const (
	flagConfig  = "-config"
	flagListen  = "-listen"
	listenAddr9 = ":9000"
	mgmtAddr7   = ":7443"
	cfgPathX    = "x.yaml"
)

// TestApplyServeOverridesUnsetLeavesConfig asserts a flag the operator did not
// pass never clobbers the config value (precedence: config wins when no flag).
func TestApplyServeOverridesUnsetLeavesConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = listenAddr9
	applyServeOverrides(&cfg, serveOverrides{set: map[string]bool{}})
	if cfg.Listen != listenAddr9 {
		t.Fatalf("unset --listen clobbered config: got %q, want :9000", cfg.Listen)
	}
}

// TestApplyServeOverridesSetWins asserts a flag the operator passed overrides
// the config value (precedence: flag > config).
func TestApplyServeOverridesSetWins(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = listenAddr9
	cfg.Management.Listen = ":9443"
	cfg.Management.CertDir = "/old"
	applyServeOverrides(&cfg, serveOverrides{
		listen:     ":7000",
		mgmtListen: mgmtAddr7,
		certDir:    "/new",
		set:        map[string]bool{keyListen: true, keyMgmtListen: true, "cert-dir": true},
	})
	if cfg.Listen != ":7000" {
		t.Errorf("--listen not applied: got %q", cfg.Listen)
	}
	if cfg.Management.Listen != mgmtAddr7 {
		t.Errorf("--mgmt-listen not applied: got %q", cfg.Management.Listen)
	}
	if cfg.Management.CertDir != "/new" {
		t.Errorf("--cert-dir not applied: got %q", cfg.Management.CertDir)
	}
}

// TestApplyServeOverridesManagementFalse asserts --management=false disables the
// management API even though the config default is on (nil pointer).
func TestApplyServeOverridesManagementFalse(t *testing.T) {
	cfg := config.Default()
	if !cfg.ManagementEnabled() {
		t.Fatalf("precondition: default config should have management on")
	}
	applyServeOverrides(&cfg, serveOverrides{
		management: false,
		set:        map[string]bool{"management": true},
	})
	if cfg.ManagementEnabled() {
		t.Fatalf("--management=false did not disable management")
	}
}

// TestApplyServeOverridesDiscoveryFalse asserts --discovery=false disables mDNS
// while leaving the rest of the config untouched.
func TestApplyServeOverridesDiscoveryFalse(t *testing.T) {
	cfg := config.Default()
	applyServeOverrides(&cfg, serveOverrides{
		discovery: false,
		set:       map[string]bool{keyDiscovery: true},
	})
	if cfg.DiscoveryEnabled() {
		t.Fatalf("--discovery=false did not disable discovery")
	}
}

// TestParseServeFlagsMapsVisitedFlags drives the real flag parser and asserts
// every override flag name matches the key applyServeOverrides reads, catching a
// flag-name/set-key mismatch that would silently disable an override.
func TestParseServeFlagsMapsVisitedFlags(t *testing.T) {
	cfgPath, ov, check, err := parseServeFlags([]string{
		flagConfig, cfgPathX, flagListen, listenAddr9, "-mgmt-listen", mgmtAddr7,
		"-cert-dir", "/c", "-management=false", "-discovery=false", "-check",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfgPath != cfgPathX {
		t.Errorf("cfgPath = %q, want x.yaml", cfgPath)
	}
	if !check {
		t.Error("--check should parse to check=true")
	}
	for _, k := range []string{keyListen, keyMgmtListen, "cert-dir", "management", keyDiscovery} {
		if !ov.set[k] {
			t.Errorf("ov.set[%q] not marked; flag name and set key disagree", k)
		}
	}
	if ov.listen != listenAddr9 || ov.mgmtListen != mgmtAddr7 || ov.certDir != "/c" {
		t.Errorf("override values wrong: %+v", ov)
	}
	if ov.management || ov.discovery {
		t.Errorf("=false toggles not captured: management=%v discovery=%v", ov.management, ov.discovery)
	}
}

// TestParseServeFlagsUnsetMarksNothing asserts an unset flag never lands in
// ov.set, so config values are preserved.
func TestParseServeFlagsUnsetMarksNothing(t *testing.T) {
	_, ov, check, err := parseServeFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if len(ov.set) != 0 {
		t.Errorf("no flags passed, but ov.set = %v", ov.set)
	}
	if check {
		t.Error("check should default false")
	}
}

// TestParseServeFlagsRejectsPositional asserts stray positional arguments error
// instead of being silently ignored.
func TestParseServeFlagsRejectsPositional(t *testing.T) {
	if _, _, _, err := parseServeFlags([]string{flagConfig, cfgPathX, "stray"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error on a stray positional argument")
	}
}

// TestServeRejectsInvalidListenOverride asserts an invalid --listen override is
// caught by re-validation with a clear config error, before any port is bound.
func TestServeRejectsInvalidListenOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.Default()
	if err := config.Save(path, &cfg); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	err := runServe([]string{flagConfig, path, flagListen, "nothostport"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected serve to reject an invalid --listen override")
	}
}

// TestReportCheckValidConfig asserts --check reports a valid config as OK
// without error.
func TestReportCheckValidConfig(t *testing.T) {
	cfg := config.Default()
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck on valid config: %v", err)
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Errorf("no OK line in output: %q", out.String())
	}
}

// TestReportCheckInvalidConfig asserts --check surfaces a validation error (here
// an opus device at a non-48kHz rate), so `serve --check` exits nonzero.
func TestReportCheckInvalidConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		Name: "bad", Device: "hw:9,0", Rate: 22050, Format: testFmtS16,
		Streams: []config.Stream{{Path: "/b", Mode: config.ModeOpus, Channels: []int{1}}},
	}}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err == nil {
		t.Fatal("expected invalid opus rate to fail check")
	}
}

// swapCLIHardware injects a host device list through the captureDevices and
// resolveDevice seams, resolving an id by exact match on either the stable id or
// the current address (as the library does for a card-index id), so reportCheck
// runs deterministically without ALSA hardware.
func swapCLIHardware(t *testing.T, devs []audio.Hardware, enumErr error) {
	t.Helper()
	prevEnum, prevResolve := captureDevices, resolveDevice
	t.Cleanup(func() { captureDevices, resolveDevice = prevEnum, prevResolve })
	captureDevices = func() ([]audio.Hardware, error) { return devs, enumErr }
	resolveDevice = func(id string) (audio.Hardware, error) {
		var hit []audio.Hardware
		for _, d := range devs {
			if d.ID == id || d.HWAddr == id {
				hit = append(hit, d)
			}
		}
		switch len(hit) {
		case 0:
			return audio.Hardware{}, &capture.DeviceNotFoundError{ID: id}
		case 1:
			return hit[0], nil
		default:
			return audio.Hardware{}, &capture.AmbiguousDeviceError{ID: id, Matches: []string{hit[0].HWAddr, hit[1].HWAddr}}
		}
	}
}

// checkDevice builds a minimal valid single-stream device for reportCheck.
func checkDevice(name, id, path string) config.Device {
	return config.Device{Name: name, Device: id, Rate: 48000, Format: testFmtS16, Streams: []config.Stream{{Path: path, Mode: config.ModePCM, Channels: []int{1}}}}
}

// checkLine returns the report line naming a device, so each status is bound to
// its own device and an inversion cannot pass on a word found elsewhere.
func checkLine(report, name string) string {
	for _, ln := range strings.Split(report, "\n") {
		if strings.Contains(ln, " "+name+" ") {
			return ln
		}
	}
	return ""
}

// TestReportCheckResolvesByIdentity pins that serve --check reports each entry
// by the hardware its id resolves to right now: a stable id is present at its
// current address even when that is not the index it was provisioned at, an
// absent device is not connected, a card-index id is flagged, and a second entry
// naming the same hardware is reported as a duplicate.
func TestReportCheckResolvesByIdentity(t *testing.T) {
	const scarlettID = "usb:1235:8218:s=S1:if=0,0"
	swapCLIHardware(t, []audio.Hardware{
		{ID: scarlettID, HWAddr: addrHW4, Label: nameScarlett, IDStable: true},
	}, nil)

	cfg := config.Default()
	cfg.Devices = []config.Device{
		checkDevice("scarlett", scarlettID, "/a"),
		checkDevice("moth", "usb:16d0:06f3:s=M1:if=0,0", "/b"),
		checkDevice("byindex", addrHW4, "/c"),
	}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	report := out.String()
	if l := checkLine(report, "scarlett"); !strings.Contains(l, "present at hw:4,0 ("+nameScarlett+")") || strings.Contains(l, "card index") {
		t.Errorf("scarlett line = %q, want present at hw:4,0 with no card-index warning", l)
	}
	if l := checkLine(report, "moth"); !strings.Contains(l, "not connected") {
		t.Errorf("moth line = %q, want not connected", l)
	}
	if l := checkLine(report, "byindex"); !strings.Contains(l, "same hardware as \"scarlett\"") {
		t.Errorf("byindex line = %q, want a same-hardware duplicate report", l)
	}
}

// TestReportCheckFlagsCardIndex pins the card-index warning, which names the
// stable id to use instead.
func TestReportCheckFlagsCardIndex(t *testing.T) {
	const scarlettID = "usb:1235:8218:s=S1:if=0,0"
	swapCLIHardware(t, []audio.Hardware{{ID: scarlettID, HWAddr: devHW1, Label: nameScarlett, IDStable: true}}, nil)

	cfg := config.Default()
	cfg.Devices = []config.Device{checkDevice("cam-a", devHW1, "/a")}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	if l := checkLine(out.String(), "cam-a"); !strings.Contains(l, "pinned to a card index") || !strings.Contains(l, scarlettID) {
		t.Errorf("cam-a line = %q, want a card-index warning naming %s", l, scarlettID)
	}
}

// TestReportCheckAmbiguous pins that an id matching two devices is reported as
// ambiguous with both addresses, never as present.
func TestReportCheckAmbiguous(t *testing.T) {
	const twinID = "usb:16d0:06f3:s=SAME:if=0,0"
	swapCLIHardware(t, []audio.Hardware{
		{ID: twinID, HWAddr: addrHW3, IDStable: true},
		{ID: twinID, HWAddr: addrHW4, IDStable: true},
	}, nil)

	cfg := config.Default()
	cfg.Devices = []config.Device{checkDevice("twin", twinID, "/a")}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	if l := checkLine(out.String(), "twin"); !strings.Contains(l, "ambiguous") || !strings.Contains(l, "hw:3,0, hw:4,0") || strings.Contains(l, "present") {
		t.Errorf("twin line = %q, want ambiguous naming both addresses", l)
	}
}

// TestReportCheckProbeUnavailable asserts a wholesale enumeration failure is
// reported as a probe-unavailable note with every device marked unknown, rather
// than misreporting present hardware as absent.
func TestReportCheckProbeUnavailable(t *testing.T) {
	swapCLIHardware(t, nil, errors.New("no ALSA"))

	cfg := config.Default()
	cfg.Devices = []config.Device{checkDevice("cam-a", devHW1, "/a")}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "device probe unavailable") {
		t.Errorf("want a probe-unavailable note, got:\n%s", s)
	}
	if l := checkLine(s, "cam-a"); !strings.Contains(l, "unknown") {
		t.Errorf("cam-a line = %q, want unknown status when the probe failed", l)
	}
}

// TestRunListDevicesShowsIdentity pins the list columns: the stable id to
// configure, the current address, and a warning on a device with no stable id.
func TestRunListDevicesShowsIdentity(t *testing.T) {
	const scarlettID = "usb:1235:8218:s=S1:if=0,0"
	swapCLIHardware(t, []audio.Hardware{
		{ID: scarlettID, HWAddr: addrHW4, Label: nameScarlett, IDStable: true},
		{ID: addrHW5, HWAddr: addrHW5, Label: "Loop"},
	}, nil)
	var out bytes.Buffer
	if err := runListDevices(&out); err != nil {
		t.Fatalf("runListDevices: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header plus 2:\n%s", len(lines), out.String())
	}
	if f := strings.Fields(lines[1]); len(f) < 3 || f[0] != scarlettID || f[1] != addrHW4 || strings.Contains(lines[1], "no stable id") {
		t.Errorf("scarlett row = %q, want id, address, label and no warning", lines[1])
	}
	if !strings.Contains(lines[2], "no stable id") {
		t.Errorf("loop row = %q, want the no-stable-id warning", lines[2])
	}
}
