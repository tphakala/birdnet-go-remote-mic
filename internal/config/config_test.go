package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	nameGarden = "garden-mic"
	pathGarden = "/garden"
	formatS16  = "s16"
	deviceHW1  = "hw:1,0"

	// Repeated validation field paths, extracted so the notification tests do not
	// trip goconst on the cpu and clear-threshold cases.
	fieldCPUPercent = "notifications.host.cpu_percent"
	fieldCPUClear   = "notifications.host.cpu_clear_percent"
	fieldTempClear  = "notifications.host.temp_clear_celsius"
	fieldDiskClear  = "notifications.host.disk_clear_percent"
)

func TestLoadMultiDevice(t *testing.T) {
	t.Parallel()
	yaml := `listen: ":8554"
devices:
  - name: garden-mic
    device: "hw:1,0"
    path: /garden
    mode: opus
    rate: 48000
    channels: [1]
    format: s16
  - name: ultrasonic-mic
    device: "hw:2,0"
    path: /bat
    mode: pcm
    rate: 256000
    channels: [1]
    format: s16
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Devices) != 2 {
		t.Fatalf("want 2 devices, got %d", len(c.Devices))
	}
	d := c.Devices[0]
	if d.Name != nameGarden || d.Streams[0].Mode != ModeOpus || d.Rate != 48000 || d.Device != deviceHW1 || d.Streams[0].Path != pathGarden {
		t.Errorf("first device parsed unexpectedly: %+v", d)
	}
	u := c.Devices[1]
	if u.Name != "ultrasonic-mic" || u.Streams[0].Mode != ModePCM || u.Rate != 256000 || u.Streams[0].Path != "/bat" {
		t.Errorf("second device parsed unexpectedly: %+v", u)
	}
}

func TestDefaultIsValidAndDeviceless(t *testing.T) {
	t.Parallel()
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("Default() must validate (zero-config first run), got %v", err)
	}
	if len(c.Devices) != 0 {
		t.Errorf("Default() devices = %d, want 0", len(c.Devices))
	}
	if c.Listen == "" || c.Management.Listen == "" {
		t.Errorf("Default() must apply listen defaults, got %+v", c)
	}
}

func TestLegacyConfigRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.yaml")
	old := "name: garden-mic\nlisten: \":8554\"\nmode: pcm\naudio:\n  device: \"hw:1,0\"\n  rate: 256000\n"
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	// Assert the distinctive migration message, not just "devices:", which the
	// empty-devices fallback error also contains: this must fail if the legacy
	// detection block is removed.
	if err == nil || !strings.Contains(err.Error(), "old single-device format") {
		t.Fatalf("legacy config should be rejected with the migration hint, got %v", err)
	}
}

func TestTopLevelAudioAnchorAccepted(t *testing.T) {
	// A new config may carry a top-level `audio:` key as a YAML anchor to DRY
	// the device blocks. Because a devices list is present, it must NOT be
	// mistaken for the old single-device shape.
	p := filepath.Join(t.TempDir(), "anchor.yaml")
	cfg := "audio: &def\n  rate: 48000\ndevices:\n  - name: mic\n    device: \"hw:1,0\"\n    <<: *def\n"
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("config with a top-level audio anchor should load, got %v", err)
	}
	if len(c.Devices) != 1 || c.Devices[0].Rate != 48000 {
		t.Errorf("anchor did not merge into the device: %+v", c.Devices)
	}
}

func TestDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "min.yaml")
	minimal := "devices:\n  - name: mic\n    device: \"hw:1,0\"\n    rate: 48000\n"
	if err := os.WriteFile(p, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load minimal: %v", err)
	}
	if c.Listen != cfgListen {
		t.Errorf("listen default = %q", c.Listen)
	}
	d := c.Devices[0]
	if d.Streams[0].Mode != ModePCM || !slices.Equal(d.Streams[0].Channels, []int{1}) || d.Format != formatS16 || d.Streams[0].Path != "/stream" {
		t.Errorf("device defaults not applied: %+v", d)
	}
}

func TestDiscoveryEnabledDefault(t *testing.T) {
	c := Config{}
	if !c.DiscoveryEnabled() {
		t.Error("discovery should default to enabled when the block is absent")
	}
	off := false
	c.Discovery.Enabled = &off
	if c.DiscoveryEnabled() {
		t.Error("discovery should be off when explicitly disabled")
	}
	on := true
	c.Discovery.Enabled = &on
	if !c.DiscoveryEnabled() {
		t.Error("discovery should be on when explicitly enabled")
	}
}

func TestManagementDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "min.yaml")
	minimal := "devices:\n  - name: mic\n    device: \"hw:1,0\"\n    rate: 48000\n"
	if err := os.WriteFile(p, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load minimal: %v", err)
	}
	if c.Management.Listen != ":8443" {
		t.Errorf("management listen default = %q, want :8443", c.Management.Listen)
	}
	if !c.ManagementEnabled() {
		t.Error("management should default to enabled when the block is absent")
	}
}

func TestManagementEnabledDefault(t *testing.T) {
	c := Config{}
	if !c.ManagementEnabled() {
		t.Error("management should default to enabled when the block is absent")
	}
	off := false
	c.Management.Enabled = &off
	if c.ManagementEnabled() {
		t.Error("management should be off when explicitly disabled")
	}
	on := true
	c.Management.Enabled = &on
	if !c.ManagementEnabled() {
		t.Error("management should be on when explicitly enabled")
	}
}

func validBase() Config {
	c := Config{
		Listen:     cfgListen,
		Management: Management{Listen: ":8443"},
		Devices: []Device{
			{Name: nameGarden, Device: deviceHW1, Rate: 256000, Format: formatS16, Streams: []Stream{{Path: pathGarden, Mode: ModePCM, Channels: []int{1}}}},
			{Name: "bat-mic", Device: "hw:2,0", Rate: 384000, Format: formatS16, Streams: []Stream{{Path: "/bat", Mode: ModePCM, Channels: []int{1}}}},
		},
	}
	// Apply defaults so the notifications block is populated: Validate runs after
	// ApplyDefaults in production (Load, PATCH), and a zero threshold block is out
	// of range, so a "valid base" must carry the defaulted thresholds.
	c.ApplyDefaults()
	return c
}

func TestDeviceIsEnabled(t *testing.T) {
	t.Parallel()
	tru, fls := true, false
	cases := []struct {
		name string
		flag *bool
		want bool
	}{
		{"absent defaults on", nil, true},
		{"explicit true", &tru, true},
		{"explicit false", &fls, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Device{Enabled: tc.flag}
			if got := d.IsEnabled(); got != tc.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCloneDeepCopiesDeviceEnabled(t *testing.T) {
	t.Parallel()
	on := true
	c := validBase()
	c.Devices[0].Enabled = &on
	clone := c.Clone()
	// Mutating the clone's device flag must not reach through to the original's
	// backing storage.
	*clone.Devices[0].Enabled = false
	if !*c.Devices[0].Enabled {
		t.Error("Clone aliased Device.Enabled; mutating the clone changed the original")
	}
}

func TestCloneDeepCopiesDeviceChannels(t *testing.T) {
	t.Parallel()
	c := validBase()
	c.Devices[0].Streams[0].Channels = []int{1, 2}
	clone := c.Clone()
	// Mutating a channel in the clone must not reach the original's backing array.
	clone.Devices[0].Streams[0].Channels[0] = 99
	if c.Devices[0].Streams[0].Channels[0] != 1 {
		t.Errorf("Clone aliased Device.Channels; original changed to %v", c.Devices[0].Streams[0].Channels)
	}
}

func TestApplyDefaultsNormalizesChannels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want []int
	}{
		{[]int{2, 1}, []int{1, 2}},          // unsorted -> ascending
		{[]int{1, 1}, []int{1}},             // duplicates removed
		{[]int{3, 1, 2, 1}, []int{1, 2, 3}}, // both
		{nil, []int{1}},                     // empty defaults to mono
	}
	for _, tt := range cases {
		c := validBase()
		c.Devices[0].Streams[0].Channels = tt.in
		c.ApplyDefaults()
		if !slices.Equal(c.Devices[0].Streams[0].Channels, tt.want) {
			t.Errorf("ApplyDefaults(%v) channels = %v, want %v", tt.in, c.Devices[0].Streams[0].Channels, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	base := validBase()
	if err := base.Validate(); err != nil {
		t.Fatalf("base config should be valid: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"two pcm devices", func(*Config) {}, false},
		{"disabled device stays valid", func(c *Config) { f := false; c.Devices[0].Enabled = &f }, false},
		{"disabled device is still validated", func(c *Config) { f := false; c.Devices[0].Enabled = &f; c.Devices[0].Rate = 100 }, true},
		{"opus at 48k mono", func(c *Config) {
			c.Devices[0].Streams[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
		}, false},
		{"opus at 44100 fails", func(c *Config) { c.Devices[0].Streams[0].Mode = ModeOpus; c.Devices[0].Rate = 44100 }, true},
		{"opus stereo (two channels) valid", func(c *Config) {
			c.Devices[0].Streams[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
			c.Devices[0].Streams[0].Channels = []int{1, 2}
		}, false},
		{"opus three-channel fails", func(c *Config) {
			c.Devices[0].Streams[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
			c.Devices[0].Streams[0].Channels = []int{1, 2, 3}
		}, true},
		{"multi-channel pcm selection valid", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{1, 2} }, false},
		{"non-contiguous channel selection valid", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{1, 3} }, false},
		{"max channel number valid", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{1, MaxChannels} }, false},
		{"format s24 fails", func(c *Config) { c.Devices[0].Format = "s24" }, true},
		{"name with CRLF fails", func(c *Config) { c.Devices[0].Name = "bad\r\nname" }, true},
		{"empty name fails", func(c *Config) { c.Devices[0].Name = "" }, true},
		{"empty listen fails", func(c *Config) { c.Listen = "" }, true},
		{"no devices is valid (zero-config first run)", func(c *Config) { c.Devices = nil }, false},
		{"rate too high fails", func(c *Config) { c.Devices[0].Rate = 500000 }, true},
		{"rate too low fails", func(c *Config) { c.Devices[0].Rate = 100 }, true},
		{"empty channel selection fails", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{} }, true},
		{"channel number above max fails", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{MaxChannels + 1} }, true},
		{"channel number zero fails", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{0} }, true},
		{"unsorted channel selection fails", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{2, 1} }, true},
		{"duplicate channel selection fails", func(c *Config) { c.Devices[0].Streams[0].Channels = []int{1, 1} }, true},
		{"empty device fails", func(c *Config) { c.Devices[0].Device = "" }, true},
		{"unknown mode fails", func(c *Config) { c.Devices[0].Streams[0].Mode = "flac" }, true},
		{"negative opus bitrate fails", func(c *Config) { c.Devices[0].Streams[0].Opus.Bitrate = -1 }, true},
		{"duplicate name fails", func(c *Config) { c.Devices[1].Name = nameGarden }, true},
		{"duplicate path fails", func(c *Config) { c.Devices[1].Streams[0].Path = pathGarden }, true},
		{"duplicate device id fails", func(c *Config) { c.Devices[1].Device = deviceHW1 }, true},
		{"path without slash fails", func(c *Config) { c.Devices[0].Streams[0].Path = "garden" }, true},
		{"path with space fails", func(c *Config) { c.Devices[0].Streams[0].Path = "/gar den" }, true},
		{"bare slash path fails", func(c *Config) { c.Devices[0].Streams[0].Path = "/" }, true},
		{"trailing slash path fails", func(c *Config) { c.Devices[0].Streams[0].Path = "/garden/" }, true},
		{"reserved trackID suffix fails", func(c *Config) { c.Devices[0].Streams[0].Path = "/garden/trackID=0" }, true},
		{"management bad listen fails", func(c *Config) { c.Management.Listen = "nope" }, true},
		{"management disabled skips listen check", func(c *Config) {
			off := false
			c.Management.Enabled = &off
			c.Management.Listen = "nope"
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel() // each subtest builds isolated state via validBase()
			c := validBase()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Error("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("want valid, got %v", err)
			}
		})
	}
}

func TestNotificationsEnabledDefault(t *testing.T) {
	t.Parallel()
	c := Config{}
	if !c.NotificationsEnabled() {
		t.Error("notifications should default to enabled when the block is absent")
	}
	off := false
	c.Notifications.Enabled = &off
	if c.NotificationsEnabled() {
		t.Error("notifications should be off when explicitly disabled")
	}
	on := true
	c.Notifications.Enabled = &on
	if !c.NotificationsEnabled() {
		t.Error("notifications should be on when explicitly enabled")
	}
}

func TestDeviceQuietAlertEnabled(t *testing.T) {
	t.Parallel()
	tru, fls := true, false
	cases := []struct {
		name string
		flag *bool
		want bool
	}{
		{"absent defaults on", nil, true},
		{"explicit true", &tru, true},
		{"explicit false", &fls, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Device{QuietAlert: tc.flag}
			if got := d.QuietAlertEnabled(); got != tc.want {
				t.Errorf("QuietAlertEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ptrInt returns a pointer to n, for building presence-aware threshold fields.
func ptrInt(n int) *int { return &n }

// wantInt fails unless p is non-nil and *p == want.
func wantInt(t *testing.T, label string, p *int, want int) {
	t.Helper()
	if p == nil || *p != want {
		t.Errorf("%s = %v, want %d", label, p, want)
	}
}

func TestApplyDefaultsFillsNotifications(t *testing.T) {
	t.Parallel()
	c := Config{} // every notification field nil (unset)
	c.ApplyDefaults()
	n := c.Notifications
	if !c.NotificationsEnabled() {
		t.Error("NotificationsEnabled() = false after defaults, want on (Enabled left nil)")
	}
	if n.Enabled != nil {
		t.Errorf("ApplyDefaults set Enabled = %v, want nil (omitted from YAML, defaults on)", n.Enabled)
	}
	wantInt(t, "audio.quiet_dbfs", n.Audio.QuietDbfs, -60)
	wantInt(t, "audio.quiet_seconds", n.Audio.QuietSeconds, 600)
	wantInt(t, "audio.zero_seconds", n.Audio.ZeroSeconds, 30)
	wantInt(t, "audio.clip_percent", n.Audio.ClipPercent, 20)
	wantInt(t, "audio.clip_window_seconds", n.Audio.ClipWindowSeconds, 10)
	wantInt(t, "host.cpu_percent", n.Host.CPUPercent, 90)
	wantInt(t, "host.cpu_clear_percent", n.Host.CPUClearPercent, 75)
	wantInt(t, "host.temp_celsius", n.Host.TempCelsius, 80)
	wantInt(t, "host.temp_clear_celsius", n.Host.TempClearCelsius, 75)
	wantInt(t, "host.disk_percent", n.Host.DiskPercent, 90)
	wantInt(t, "host.disk_clear_percent", n.Host.DiskClearPercent, 85)
	wantInt(t, "host.mem_free_percent", n.Host.MemFreePercent, 10)
	wantInt(t, "host.mem_free_mib", n.Host.MemFreeMiB, 64)
	// The defaults must themselves validate (clear thresholds below their onsets).
	if err := c.Validate(); err != nil {
		t.Errorf("defaulted notifications should validate, got %v", err)
	}
	// Idempotent: a second pass leaves every (now non-nil) field unchanged.
	before := c.Notifications
	c.ApplyDefaults()
	if c.Notifications != before {
		t.Error("ApplyDefaults not idempotent: a second pass replaced a threshold pointer")
	}
}

// TestValidateRejectsExplicitZeroAfterDefaults covers the production order
// (decode then ApplyDefaults then Validate): an explicitly provided out-of-range
// value survives ApplyDefaults (which only fills nil) and is rejected by Validate,
// rather than being silently overwritten with the default.
func TestValidateRejectsExplicitZeroAfterDefaults(t *testing.T) {
	t.Parallel()
	c := validBase()
	c.Notifications.Host.CPUPercent = ptrInt(0) // explicit, out of range
	c.ApplyDefaults()                           // must NOT overwrite the explicit 0
	if c.Notifications.Host.CPUPercent == nil || *c.Notifications.Host.CPUPercent != 0 {
		t.Fatalf("ApplyDefaults overwrote an explicit 0: %v", c.Notifications.Host.CPUPercent)
	}
	err := c.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != fieldCPUPercent {
		t.Fatalf("Validate() = %v, want a cpu_percent error for an explicit 0", err)
	}
}

// TestValidateRejectsExplicitZeroViaLoad exercises the same through the real file
// path: a YAML threshold of 0 loads, defaults, and fails validation.
func TestValidateRejectsExplicitZeroViaLoad(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "listen: \":8554\"\nnotifications:\n  host:\n    cpu_percent: 0\ndevices: []\n"
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != fieldCPUPercent {
		t.Fatalf("Load with cpu_percent: 0 = %v, want a cpu_percent validation error", err)
	}
}

func TestValidateNotifications(t *testing.T) {
	// Each case starts from a defaulted, valid config (validBase applies defaults,
	// so every threshold pointer is non-nil), deref-assigns one threshold to an
	// out-of-range or non-hysteretic value, and asserts the exact dotted field
	// path. An empty wantField means the mutation is valid.
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{"defaults valid", func(*Config) {}, ""},
		{"quiet_dbfs too low", func(c *Config) { *c.Notifications.Audio.QuietDbfs = -100 }, "notifications.audio.quiet_dbfs"},
		{"quiet_dbfs too high", func(c *Config) { *c.Notifications.Audio.QuietDbfs = 0 }, "notifications.audio.quiet_dbfs"},
		{"quiet_seconds too low", func(c *Config) { *c.Notifications.Audio.QuietSeconds = 9 }, "notifications.audio.quiet_seconds"},
		{"quiet_seconds too high", func(c *Config) { *c.Notifications.Audio.QuietSeconds = 86401 }, "notifications.audio.quiet_seconds"},
		{"zero_seconds too low", func(c *Config) { *c.Notifications.Audio.ZeroSeconds = 4 }, "notifications.audio.zero_seconds"},
		{"zero_seconds too high", func(c *Config) { *c.Notifications.Audio.ZeroSeconds = 3601 }, "notifications.audio.zero_seconds"},
		{"clip_percent too low", func(c *Config) { *c.Notifications.Audio.ClipPercent = 0 }, "notifications.audio.clip_percent"},
		{"clip_percent too high", func(c *Config) { *c.Notifications.Audio.ClipPercent = 101 }, "notifications.audio.clip_percent"},
		{"clip_window too low", func(c *Config) { *c.Notifications.Audio.ClipWindowSeconds = 0 }, "notifications.audio.clip_window_seconds"},
		{"clip_window too high", func(c *Config) { *c.Notifications.Audio.ClipWindowSeconds = 601 }, "notifications.audio.clip_window_seconds"},
		{"cpu_percent too low", func(c *Config) { *c.Notifications.Host.CPUPercent = 0 }, fieldCPUPercent},
		{"cpu_percent too high", func(c *Config) { *c.Notifications.Host.CPUPercent = 101 }, fieldCPUPercent},
		{"cpu_clear out of range", func(c *Config) { *c.Notifications.Host.CPUClearPercent = 100 }, fieldCPUClear},
		{"cpu_clear below min (zero)", func(c *Config) { *c.Notifications.Host.CPUClearPercent = 0 }, fieldCPUClear},
		{"cpu_clear not below onset", func(c *Config) {
			*c.Notifications.Host.CPUPercent = 50
			*c.Notifications.Host.CPUClearPercent = 60
		}, fieldCPUClear},
		{"cpu_clear equals onset", func(c *Config) {
			*c.Notifications.Host.CPUPercent = 50
			*c.Notifications.Host.CPUClearPercent = 50
		}, fieldCPUClear},
		{"temp_celsius too low", func(c *Config) { *c.Notifications.Host.TempCelsius = 29 }, "notifications.host.temp_celsius"},
		{"temp_celsius too high", func(c *Config) { *c.Notifications.Host.TempCelsius = 121 }, "notifications.host.temp_celsius"},
		{"temp_clear out of range", func(c *Config) { *c.Notifications.Host.TempClearCelsius = 120 }, fieldTempClear},
		{"temp_clear below min (zero)", func(c *Config) { *c.Notifications.Host.TempClearCelsius = 0 }, fieldTempClear},
		{"temp_clear not below onset", func(c *Config) { *c.Notifications.Host.TempClearCelsius = 85 }, fieldTempClear},
		{"temp_clear equals onset", func(c *Config) { *c.Notifications.Host.TempClearCelsius = 80 }, fieldTempClear},
		{"disk_percent too low", func(c *Config) { *c.Notifications.Host.DiskPercent = 0 }, "notifications.host.disk_percent"},
		{"disk_percent too high", func(c *Config) { *c.Notifications.Host.DiskPercent = 101 }, "notifications.host.disk_percent"},
		{"disk_clear below min (negative)", func(c *Config) { *c.Notifications.Host.DiskClearPercent = -1 }, fieldDiskClear},
		{"disk_clear below min (zero)", func(c *Config) { *c.Notifications.Host.DiskClearPercent = 0 }, fieldDiskClear},
		{"disk_clear above max", func(c *Config) { *c.Notifications.Host.DiskClearPercent = 100 }, fieldDiskClear},
		{"disk_clear not below onset", func(c *Config) { *c.Notifications.Host.DiskClearPercent = 95 }, fieldDiskClear},
		{"disk_clear equals onset", func(c *Config) { *c.Notifications.Host.DiskClearPercent = 90 }, fieldDiskClear},
		{"mem_free_percent too low", func(c *Config) { *c.Notifications.Host.MemFreePercent = 0 }, "notifications.host.mem_free_percent"},
		{"mem_free_percent too high", func(c *Config) { *c.Notifications.Host.MemFreePercent = 91 }, "notifications.host.mem_free_percent"},
		{"mem_free_mib too low", func(c *Config) { *c.Notifications.Host.MemFreeMiB = 0 }, "notifications.host.mem_free_mib"},
		{"mem_free_mib too high", func(c *Config) { *c.Notifications.Host.MemFreeMiB = 65537 }, "notifications.host.mem_free_mib"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validBase()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantField == "" {
				if err != nil {
					t.Errorf("want valid, got %v", err)
				}
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate() = %v, want a *ValidationError for %q", err, tt.wantField)
			}
			if verr.Field != tt.wantField {
				t.Errorf("field = %q, want %q (reason: %s)", verr.Field, tt.wantField, verr.Reason)
			}
		})
	}
}

func TestCloneDeepCopiesNotificationsEnabled(t *testing.T) {
	t.Parallel()
	on := true
	c := validBase()
	c.Notifications.Enabled = &on
	clone := c.Clone()
	*clone.Notifications.Enabled = false
	if !*c.Notifications.Enabled {
		t.Error("Clone aliased Notifications.Enabled; mutating the clone changed the original")
	}
}

func TestCloneDeepCopiesNotificationThresholds(t *testing.T) {
	t.Parallel()
	c := validBase() // ApplyDefaults populated every threshold pointer
	clone := c.Clone()
	// Mutating a cloned threshold must not reach through to the original's storage.
	*clone.Notifications.Host.CPUPercent = 42
	*clone.Notifications.Audio.QuietDbfs = -10
	if *c.Notifications.Host.CPUPercent == 42 || *c.Notifications.Audio.QuietDbfs == -10 {
		t.Error("Clone aliased a notification threshold; mutating the clone changed the original")
	}
}

// TestNotificationsZeroValueValidatesAndClones covers the nil-pointer paths:
// validate reads an absent (nil) threshold as its default (so a zero-value block
// validates) and clone leaves a nil threshold nil. These paths are otherwise only
// reached when Validate/Clone run on an un-defaulted config.
func TestNotificationsZeroValueValidatesAndClones(t *testing.T) {
	t.Parallel()
	var n Notifications // every threshold pointer and Enabled nil
	if err := n.validate(); err != nil {
		t.Errorf("zero-value Notifications.validate() = %v, want nil (nil reads as the default)", err)
	}
	clone := n.clone()
	if clone.Enabled != nil || clone.Audio.QuietDbfs != nil || clone.Host.CPUPercent != nil || clone.Host.MemFreeMiB != nil {
		t.Errorf("clone of a zero-value Notifications should keep nil pointers, got %+v", clone)
	}
}

func TestCloneDeepCopiesDeviceQuietAlert(t *testing.T) {
	t.Parallel()
	off := false
	c := validBase()
	c.Devices[0].QuietAlert = &off
	clone := c.Clone()
	*clone.Devices[0].QuietAlert = true
	if *c.Devices[0].QuietAlert {
		t.Error("Clone aliased Device.QuietAlert; mutating the clone changed the original")
	}
}
