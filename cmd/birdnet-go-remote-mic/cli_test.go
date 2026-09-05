//go:build linux

package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	capture "github.com/tphakala/go-audio-capture"

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
		Name: "bad", Device: "hw:9,0", Path: "/b",
		Mode: config.ModeOpus, Rate: 22050, Channels: []int{1}, Format: testFmtS16,
	}}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err == nil {
		t.Fatal("expected invalid opus rate to fail check")
	}
}

// TestReportCheckDevicePresence injects a known host device list through the
// captureDevices seam so the present and not-found branches run deterministically
// without ALSA hardware.
func TestReportCheckDevicePresence(t *testing.T) {
	orig := captureDevices
	t.Cleanup(func() { captureDevices = orig })
	captureDevices = func() ([]capture.DeviceInfo, error) {
		return []capture.DeviceInfo{{ID: devHW1, Name: nameScarlett}}, nil
	}

	cfg := config.Default()
	cfg.Devices = []config.Device{
		{Name: "cam-a", Device: devHW1, Path: "/a", Mode: config.ModePCM, Rate: 48000, Channels: []int{1}, Format: testFmtS16},
		{Name: "cam-b", Device: "hw:9,0", Path: "/b", Mode: config.ModePCM, Rate: 48000, Channels: []int{1}, Format: testFmtS16},
	}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	// Bind each status to its device's own line so a present/not-found inversion
	// cannot pass just because both words appear somewhere in the output.
	lineFor := func(name string) string {
		for _, ln := range strings.Split(out.String(), "\n") {
			if strings.Contains(ln, name) {
				return ln
			}
		}
		return ""
	}
	if l := lineFor("cam-a"); !strings.Contains(l, "present") || strings.Contains(l, "not found") {
		t.Errorf("cam-a line = %q, want status present", l)
	}
	if l := lineFor("cam-b"); !strings.Contains(l, "not found") {
		t.Errorf("cam-b line = %q, want status not found", l)
	}
}

// TestReportCheckProbeUnavailable asserts a wholesale enumeration failure is
// reported as a probe-unavailable note with every device marked unknown, rather
// than misreporting present hardware as absent.
func TestReportCheckProbeUnavailable(t *testing.T) {
	orig := captureDevices
	t.Cleanup(func() { captureDevices = orig })
	captureDevices = func() ([]capture.DeviceInfo, error) {
		return nil, errors.New("no ALSA")
	}

	cfg := config.Default()
	cfg.Devices = []config.Device{
		{Name: "cam-a", Device: devHW1, Path: "/a", Mode: config.ModePCM, Rate: 48000, Channels: []int{1}, Format: testFmtS16},
	}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "device probe unavailable") {
		t.Errorf("want a probe-unavailable note, got:\n%s", s)
	}
	if !strings.Contains(s, "unknown") {
		t.Errorf("want unknown status when the probe failed, got:\n%s", s)
	}
}
