//go:build linux

package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/filesource"
)

const (
	flagConfig    = "-config"
	flagListen    = "-listen"
	listenAddr9   = ":9000"
	mgmtAddr7     = ":7443"
	cfgPathX      = "x.yaml"
	pprofAddr6060 = "127.0.0.1:6060"

	// The --check wording for a file device that cannot play yet, and for a
	// path the installed unit hides, spelled out so a change to either is a
	// deliberate test change.
	wantRetryHint  = "it starts automatically once it can play"
	wantHiddenNote = "(hidden from the installed service)"
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
		set:        map[string]bool{keyMgmt: true},
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
	cfg, ov, check, pprofAddr, err := parseServeFlags([]string{
		flagConfig, cfgPathX, flagListen, listenAddr9, "-mgmt-listen", mgmtAddr7,
		"-cert-dir", "/c", "-management=false", "-discovery=false", "-check",
		"-pprof", pprofAddr6060,
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfg.path != cfgPathX {
		t.Errorf("cfgPath = %q, want x.yaml", cfg.path)
	}
	if !check {
		t.Error("--check should parse to check=true")
	}
	if pprofAddr != pprofAddr6060 {
		t.Errorf("pprofAddr = %q, want 127.0.0.1:6060", pprofAddr)
	}
	for _, k := range []string{keyListen, keyMgmtListen, "cert-dir", keyMgmt, keyDiscovery} {
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
	_, ov, check, pprofAddr, err := parseServeFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if pprofAddr != "" {
		t.Errorf("unset -pprof should yield empty addr, got %q", pprofAddr)
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
	if _, _, _, _, err := parseServeFlags([]string{flagConfig, cfgPathX, "stray"}, &bytes.Buffer{}); err == nil {
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
	for ln := range strings.SplitSeq(report, "\n") {
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
	if l := checkLine(report, "moth"); !strings.Contains(l, "Not connected") {
		t.Errorf("moth line = %q, want not connected", l)
	}
	if l := checkLine(report, "byindex"); !strings.Contains(l, "same hardware as \"scarlett\"") {
		t.Errorf("byindex line = %q, want a same-hardware duplicate report", l)
	}
}

// TestReportCheckDisabledEntryDoesNotClaimHardware pins M4: a disabled entry
// that resolves to the same hardware as a later enabled entry does not own that
// hardware, because the appliance never opens a disabled device. The enabled
// entry is therefore reported present, not as a duplicate that "will not be
// opened" (which it would be).
func TestReportCheckDisabledEntryDoesNotClaimHardware(t *testing.T) {
	const scarlettID = "usb:1235:8218:s=S1:if=0,0"
	swapCLIHardware(t, []audio.Hardware{
		{ID: scarlettID, HWAddr: addrHW4, Label: nameScarlett, IDStable: true},
	}, nil)

	disabled := false
	a := checkDevice("disabled-a", scarlettID, "/a")
	a.Enabled = &disabled
	b := checkDevice("enabled-b", addrHW4, "/b")

	cfg := config.Default()
	cfg.Devices = []config.Device{a, b}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	report := out.String()
	if l := checkLine(report, "enabled-b"); !strings.Contains(l, "present at hw:4,0") || strings.Contains(l, "same hardware") || strings.Contains(l, "will not be opened") {
		t.Errorf("enabled-b line = %q, want present with no duplicate text (a disabled entry does not claim the hardware)", l)
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
	if l := checkLine(out.String(), "cam-a"); !strings.Contains(l, "card index (can change after a reboot)") || !strings.Contains(l, scarlettID) {
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
	if l := checkLine(out.String(), "twin"); !strings.Contains(l, "Ambiguous") || !strings.Contains(l, "hw:3,0, hw:4,0") || strings.Contains(l, "present") {
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
	// The address column is labelled ALSA (matching the web UI), not ADDRESS.
	if h := lines[0]; !strings.Contains(h, "ID") || !strings.Contains(h, "ALSA") || !strings.Contains(h, "LABEL") {
		t.Errorf("header = %q, want ID / ALSA / LABEL columns", h)
	}
	if f := strings.Fields(lines[1]); len(f) < 3 || f[0] != scarlettID || f[1] != addrHW4 || strings.Contains(lines[1], "no stable id") {
		t.Errorf("scarlett row = %q, want id, address, label and no warning", lines[1])
	}
	if !strings.Contains(lines[2], "no stable id") {
		t.Errorf("loop row = %q, want the no-stable-id warning", lines[2])
	}
}

// TestReportCheckCardIndexWithoutStableID pins that --check never suggests a
// card index as the replacement id: a host with no stable form resolves the
// index back to itself, so the hint says the device reports no stable id.
func TestReportCheckCardIndexWithoutStableID(t *testing.T) {
	swapCLIHardware(t, []audio.Hardware{{ID: devHW1, HWAddr: devHW1, Label: nameScarlett}}, nil)

	cfg := config.Default()
	cfg.Devices = []config.Device{checkDevice("cam-a", devHW1, "/a")}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	l := checkLine(out.String(), "cam-a")
	if strings.Contains(l, "use id") || !strings.Contains(l, "reports no stable id") {
		t.Errorf("cam-a line = %q, want the no-stable-id note and no replacement id", l)
	}
}

// TestReportCheckEmptyAddressClaimsNothing pins that two entries resolving to
// devices with no address are not reported as duplicates of each other, and
// that the status does not print a dangling "present at".
func TestReportCheckEmptyAddressClaimsNothing(t *testing.T) {
	swapCLIHardware(t, []audio.Hardware{
		{ID: idScarlett, IDStable: true},
		{ID: idMoth, IDStable: true},
	}, nil)

	cfg := config.Default()
	cfg.Devices = []config.Device{checkDevice("one", idScarlett, "/a"), checkDevice("two", idMoth, "/b")}
	var out bytes.Buffer
	if err := reportCheck(&cfg, &out); err != nil {
		t.Fatalf("reportCheck: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		if l := checkLine(out.String(), name); strings.Contains(l, "same hardware") || strings.Contains(l, "present at") || !strings.Contains(l, "present") {
			t.Errorf("%s line = %q, want plain present with no duplicate report", name, l)
		}
	}
}

// swapOpenFileSource substitutes the file-source open seam for one test.
func swapOpenFileSource(t *testing.T, fn func(filesource.Config) (audio.Source, error)) {
	t.Helper()
	prev := openFileSource
	t.Cleanup(func() { openFileSource = prev })
	openFileSource = fn
}

// fileCheckCfg is a config of file devices for serve --check, named dev0,
// dev1 and so on in id order.
func fileCheckCfg(ids ...string) *config.Config {
	cfg := config.Default()
	cfg.Devices = nil
	for i, id := range ids {
		cfg.Devices = append(cfg.Devices, checkDevice("dev"+strconv.Itoa(i), id, "/p"+strconv.Itoa(i)))
	}
	return &cfg
}

// TestReportCheckFileHiddenPath pins the serve --check note for a file path
// under a directory the installed unit hides from the service: on the
// device's line, ahead of any per-file lines, in place of the retry hint.
func TestReportCheckFileHiddenPath(t *testing.T) {
	t.Run("hidden path", func(t *testing.T) {
		swapOpenFileSource(t, func(cfg filesource.Config) (audio.Source, error) {
			cfg.Logf("skipped %s", "x.flac")
			return geomSource{rate: cfg.Rate, channels: 2}, nil
		})
		var out bytes.Buffer
		if err := reportCheck(fileCheckCfg("file:/home/pi/audio"), &out); err != nil {
			t.Fatal(err)
		}
		if want := "file source, 48000 Hz, 2 ch; under /home " + wantHiddenNote + "\n    skipped x.flac"; !strings.Contains(out.String(), want) {
			t.Errorf("report = %q, want the note on the device line ahead of the skip line: %q", out.String(), want)
		}
	})

	// The installed service never sees a hidden path, so no retry recovers it
	// there: the note stands in for the retry hint rather than contradicting it.
	t.Run("hidden path that fails", func(t *testing.T) {
		swapOpenFileSource(t, func(filesource.Config) (audio.Source, error) {
			return nil, &filesource.NoAudioError{Skipped: []filesource.Skip{
				{Name: "a.flac", Reason: "sample rate 44100", Class: filesource.SkipRate},
			}}
		})
		var out bytes.Buffer
		if err := reportCheck(fileCheckCfg("file:/home/pi/audio"), &out); err != nil {
			t.Fatal(err)
		}
		l := checkLine(out.String(), "dev0")
		if want := "file:/home/pi/audio  filesource: no playable audio (1 sample rate differs); under /home " + wantHiddenNote; !strings.Contains(l, want) || strings.Contains(l, wantRetryHint) {
			t.Errorf("line = %q, want %q and no retry hint", l, want)
		}
		if !strings.Contains(out.String(), "\n    a.flac: sample rate 44100") {
			t.Errorf("report lacks the indented skip line: %q", out.String())
		}
	})
}

// TestReportCheckFileSource pins serve --check for file devices: the source is
// opened as the appliance would open it, never resolved as ALSA hardware, never
// probed when every device is virtual, and bounded so a hung mount cannot hold
// the ExecStartPre of a service start.
func TestReportCheckFileSource(t *testing.T) {
	fileCfg := fileCheckCfg
	probeCalls := 0
	prevEnum, prevResolve := captureDevices, resolveDevice
	t.Cleanup(func() { captureDevices, resolveDevice = prevEnum, prevResolve })
	captureDevices = func() ([]audio.Hardware, error) {
		probeCalls++
		return nil, errors.New("no /proc/asound")
	}
	resolveDevice = func(id string) (audio.Hardware, error) {
		t.Errorf("resolveDevice called for %q", id)
		return audio.Hardware{}, errors.New("unexpected")
	}

	t.Run("playable", func(t *testing.T) {
		swapOpenFileSource(t, func(cfg filesource.Config) (audio.Source, error) {
			cfg.Logf("skipped %s", "x.flac")
			return geomSource{rate: cfg.Rate, channels: 2}, nil
		})
		var out bytes.Buffer
		if err := reportCheck(fileCfg("file:/srv/audio"), &out); err != nil {
			t.Fatal(err)
		}
		if l := checkLine(out.String(), "dev0"); !strings.Contains(l, "file source, 48000 Hz, 2 ch") || strings.Contains(l, wantHiddenNote) {
			t.Errorf("line = %q, want the file source geometry and no hidden-path note", l)
		}
		if !strings.Contains(out.String(), "\n    skipped x.flac") {
			t.Errorf("report lacks the indented skip line: %q", out.String())
		}
		if probeCalls != 0 || strings.Contains(out.String(), "probe unavailable") {
			t.Errorf("a file-only config probed ALSA (%d calls): %q", probeCalls, out.String())
		}
	})

	t.Run("no audio", func(t *testing.T) {
		swapOpenFileSource(t, func(filesource.Config) (audio.Source, error) {
			return nil, &filesource.NoAudioError{Skipped: []filesource.Skip{
				{Name: "a.flac", Reason: "sample rate 44100", Class: filesource.SkipRate},
				{Name: "b.wav", Reason: "no audio", Class: filesource.SkipUndecodable},
			}}
		})
		var out bytes.Buffer
		if err := reportCheck(fileCfg("file:/srv/audio"), &out); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if l, want := checkLine(got, "dev0"), "file:/srv/audio  filesource: no playable audio (1 sample rate differs, 1 undecodable); "+wantRetryHint; !strings.Contains(l, want) {
			t.Errorf("line = %q, want it to contain %q", l, want)
		}
		for _, want := range []string{"\n    a.flac: sample rate 44100", "\n    b.wav: no audio"} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q: %q", want, got)
			}
		}
		if strings.Contains(got, "Malformed") {
			t.Errorf("a file id reported as malformed: %q", got)
		}
	})

	// The next two open through the real filesource.Open (no seam swap), so a
	// prefix the package and this command both add shows up here. The
	// hidden-path note is added by checkStatus, so a temp dir under /tmp does
	// not change fileCheckStatus's text.
	t.Run("missing path through Open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.flac")
		d := checkDevice("dev0", "file:"+path, "/p")
		if got, detail := fileCheckStatus(&d, path); got != "filesource: no such file or directory; "+wantRetryHint || detail != "" {
			t.Errorf("status = %q, detail = %q, want %q and no detail", got, detail, "filesource: no such file or directory; "+wantRetryHint)
		}
	})

	t.Run("no audio through Open", func(t *testing.T) {
		path := t.TempDir()
		d := checkDevice("dev0", "file:"+path, "/p")
		if got, detail := fileCheckStatus(&d, path); got != "filesource: no playable audio; "+wantRetryHint || detail != "" {
			t.Errorf("status = %q, detail = %q, want %q and no detail", got, detail, "filesource: no playable audio; "+wantRetryHint)
		}
	})

	t.Run("probe failure still checks file rows", func(t *testing.T) {
		swapOpenFileSource(t, func(cfg filesource.Config) (audio.Source, error) {
			return geomSource{rate: cfg.Rate, channels: 1}, nil
		})
		probeCalls = 0
		var out bytes.Buffer
		if err := reportCheck(fileCfg("file:/srv/audio", "hw:1,0"), &out); err != nil {
			t.Fatal(err)
		}
		if probeCalls != 1 {
			t.Errorf("captureDevices calls = %d, want 1 for the ALSA row", probeCalls)
		}
		if l := checkLine(out.String(), "dev0"); !strings.Contains(l, "file source, 48000 Hz, 1 ch") {
			t.Errorf("file row = %q, want it checked despite the probe failure", l)
		}
		if l := checkLine(out.String(), "dev1"); !strings.Contains(l, "unknown") {
			t.Errorf("ALSA row = %q, want unknown", l)
		}
	})

	t.Run("hung filesystem", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{}) // inside the bubble, so the blocked open counts as durably blocked
			swapOpenFileSource(t, func(filesource.Config) (audio.Source, error) {
				<-release
				return nil, errors.New("released")
			})
			start := time.Now()
			var out bytes.Buffer
			if err := reportCheck(fileCfg("file:/srv/audio"), &out); err != nil {
				t.Fatal(err)
			}
			if got := time.Since(start); got != fileOpenTimeout {
				t.Errorf("check took %v, want %v", got, fileOpenTimeout)
			}
			if l, want := checkLine(out.String(), "dev0"), "file:/srv/audio  no answer from the filesystem within 5s"; !strings.Contains(l, want) {
				t.Errorf("line = %q, want it to contain %q", l, want)
			}
			close(release)
		})
	})
}
