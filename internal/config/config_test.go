package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if d.Name != "garden-mic" || d.Mode != ModeOpus || d.Rate != 48000 || d.Device != "hw:1,0" || d.Path != "/garden" {
		t.Errorf("first example device parsed unexpectedly: %+v", d)
	}
	u := c.Devices[1]
	if u.Name != "ultrasonic-mic" || u.Mode != ModePCM || u.Rate != 256000 || u.Path != "/bat" {
		t.Errorf("second example device parsed unexpectedly: %+v", u)
	}
}

func TestLegacyConfigRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.yaml")
	old := "name: garden-mic\nlisten: \":8554\"\nmode: pcm\naudio:\n  device: \"hw:1,0\"\n  rate: 256000\n"
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "devices:") {
		t.Fatalf("legacy config should be rejected with a pointer to devices:, got %v", err)
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
	if d.Mode != ModePCM || d.Channels != 1 || d.Format != "s16" || d.Path != "/stream" {
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

func validBase() Config {
	return Config{
		Listen: ":8554",
		Devices: []Device{
			{Name: "garden-mic", Device: "hw:1,0", Path: "/garden", Mode: ModePCM, Rate: 256000, Channels: 1, Format: "s16"},
			{Name: "bat-mic", Device: "hw:2,0", Path: "/bat", Mode: ModePCM, Rate: 384000, Channels: 1, Format: "s16"},
		},
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
		{"opus at 48k mono", func(c *Config) {
			c.Devices[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
		}, false},
		{"opus at 44100 fails", func(c *Config) { c.Devices[0].Mode = ModeOpus; c.Devices[0].Rate = 44100 }, true},
		{"opus stereo fails", func(c *Config) {
			c.Devices[0].Mode = ModeOpus
			c.Devices[0].Rate = 48000
			c.Devices[0].Channels = 2
		}, true},
		{"format s24 fails", func(c *Config) { c.Devices[0].Format = "s24" }, true},
		{"name with CRLF fails", func(c *Config) { c.Devices[0].Name = "bad\r\nname" }, true},
		{"empty name fails", func(c *Config) { c.Devices[0].Name = "" }, true},
		{"empty listen fails", func(c *Config) { c.Listen = "" }, true},
		{"no devices fails", func(c *Config) { c.Devices = nil }, true},
		{"rate too high fails", func(c *Config) { c.Devices[0].Rate = 500000 }, true},
		{"rate too low fails", func(c *Config) { c.Devices[0].Rate = 100 }, true},
		{"channels 3 fails", func(c *Config) { c.Devices[0].Channels = 3 }, true},
		{"empty device fails", func(c *Config) { c.Devices[0].Device = "" }, true},
		{"unknown mode fails", func(c *Config) { c.Devices[0].Mode = "flac" }, true},
		{"negative opus bitrate fails", func(c *Config) { c.Devices[0].Opus.Bitrate = -1 }, true},
		{"duplicate name fails", func(c *Config) { c.Devices[1].Name = "garden-mic" }, true},
		{"duplicate path fails", func(c *Config) { c.Devices[1].Path = "/garden" }, true},
		{"duplicate device id fails", func(c *Config) { c.Devices[1].Device = "hw:1,0" }, true},
		{"path without slash fails", func(c *Config) { c.Devices[0].Path = "garden" }, true},
		{"path with space fails", func(c *Config) { c.Devices[0].Path = "/gar den" }, true},
		{"bare slash path fails", func(c *Config) { c.Devices[0].Path = "/" }, true},
		{"trailing slash path fails", func(c *Config) { c.Devices[0].Path = "/garden/" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
