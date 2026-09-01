package config

import (
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
)

func TestLoadExample(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Load(config.example.yaml): %v", err)
	}
	if len(c.Devices) != 2 {
		t.Fatalf("example should configure 2 devices, got %d", len(c.Devices))
	}
	d := c.Devices[0]
	if d.Name != nameGarden || d.Mode != ModeOpus || d.Rate != 48000 || d.Device != deviceHW1 || d.Path != pathGarden {
		t.Errorf("first example device parsed unexpectedly: %+v", d)
	}
	u := c.Devices[1]
	if u.Name != "ultrasonic-mic" || u.Mode != ModePCM || u.Rate != 256000 || u.Path != "/bat" {
		t.Errorf("second example device parsed unexpectedly: %+v", u)
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
	if c.Listen != ":8554" {
		t.Errorf("listen default = %q", c.Listen)
	}
	d := c.Devices[0]
	if d.Mode != ModePCM || !slices.Equal(d.Channels, []int{1}) || d.Format != formatS16 || d.Path != "/stream" {
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
	return Config{
		Listen:     ":8554",
		Management: Management{Listen: ":8443"},
		Devices: []Device{
			{Name: nameGarden, Device: deviceHW1, Path: pathGarden, Mode: ModePCM, Rate: 256000, Channels: []int{1}, Format: formatS16},
			{Name: "bat-mic", Device: "hw:2,0", Path: "/bat", Mode: ModePCM, Rate: 384000, Channels: []int{1}, Format: formatS16},
		},
	}
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
	c.Devices[0].Channels = []int{1, 2}
	clone := c.Clone()
	// Mutating a channel in the clone must not reach the original's backing array.
	clone.Devices[0].Channels[0] = 99
	if c.Devices[0].Channels[0] != 1 {
		t.Errorf("Clone aliased Device.Channels; original changed to %v", c.Devices[0].Channels)
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
		c.Devices[0].Channels = tt.in
		c.ApplyDefaults()
		if !slices.Equal(c.Devices[0].Channels, tt.want) {
			t.Errorf("ApplyDefaults(%v) channels = %v, want %v", tt.in, c.Devices[0].Channels, tt.want)
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
			c.Devices[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
		}, false},
		{"opus at 44100 fails", func(c *Config) { c.Devices[0].Mode = ModeOpus; c.Devices[0].Rate = 44100 }, true},
		{"opus multi-channel fails", func(c *Config) {
			c.Devices[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
			c.Devices[0].Channels = []int{1, 2}
		}, true},
		{"multi-channel pcm selection valid", func(c *Config) { c.Devices[0].Channels = []int{1, 2} }, false},
		{"non-contiguous channel selection valid", func(c *Config) { c.Devices[0].Channels = []int{1, 3} }, false},
		{"max channel number valid", func(c *Config) { c.Devices[0].Channels = []int{1, maxChannels} }, false},
		{"format s24 fails", func(c *Config) { c.Devices[0].Format = "s24" }, true},
		{"name with CRLF fails", func(c *Config) { c.Devices[0].Name = "bad\r\nname" }, true},
		{"empty name fails", func(c *Config) { c.Devices[0].Name = "" }, true},
		{"empty listen fails", func(c *Config) { c.Listen = "" }, true},
		{"no devices is valid (zero-config first run)", func(c *Config) { c.Devices = nil }, false},
		{"rate too high fails", func(c *Config) { c.Devices[0].Rate = 500000 }, true},
		{"rate too low fails", func(c *Config) { c.Devices[0].Rate = 100 }, true},
		{"empty channel selection fails", func(c *Config) { c.Devices[0].Channels = []int{} }, true},
		{"channel number above max fails", func(c *Config) { c.Devices[0].Channels = []int{maxChannels + 1} }, true},
		{"channel number zero fails", func(c *Config) { c.Devices[0].Channels = []int{0} }, true},
		{"unsorted channel selection fails", func(c *Config) { c.Devices[0].Channels = []int{2, 1} }, true},
		{"duplicate channel selection fails", func(c *Config) { c.Devices[0].Channels = []int{1, 1} }, true},
		{"empty device fails", func(c *Config) { c.Devices[0].Device = "" }, true},
		{"unknown mode fails", func(c *Config) { c.Devices[0].Mode = "flac" }, true},
		{"negative opus bitrate fails", func(c *Config) { c.Devices[0].Opus.Bitrate = -1 }, true},
		{"duplicate name fails", func(c *Config) { c.Devices[1].Name = nameGarden }, true},
		{"duplicate path fails", func(c *Config) { c.Devices[1].Path = pathGarden }, true},
		{"duplicate device id fails", func(c *Config) { c.Devices[1].Device = deviceHW1 }, true},
		{"path without slash fails", func(c *Config) { c.Devices[0].Path = "garden" }, true},
		{"path with space fails", func(c *Config) { c.Devices[0].Path = "/gar den" }, true},
		{"bare slash path fails", func(c *Config) { c.Devices[0].Path = "/" }, true},
		{"trailing slash path fails", func(c *Config) { c.Devices[0].Path = "/garden/" }, true},
		{"reserved trackID suffix fails", func(c *Config) { c.Devices[0].Path = "/garden/trackID=0" }, true},
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
