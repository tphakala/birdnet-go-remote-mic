package config

import "testing"

func TestLoadExample(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Load(config.example.yaml): %v", err)
	}
	if c.Name != "garden-mic" || c.Mode != ModePCM || c.Audio.Rate != 256000 || c.Audio.Device != "hw:1,0" {
		t.Errorf("example config parsed unexpectedly: %+v", c)
	}
}

func TestValidate(t *testing.T) {
	base := Config{
		Name:   "garden-mic",
		Listen: ":8554",
		Mode:   ModePCM,
		Audio:  Audio{Device: "hw:1,0", Rate: 256000, Channels: 1, Format: "s16"},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("base config should be valid: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"pcm at 256k", func(*Config) {}, false},
		{"opus at 48k mono", func(c *Config) { c.Mode = ModeOpus; c.Audio.Rate = 48000; c.Audio.Channels = 1 }, false},
		{"opus at 44100 fails", func(c *Config) { c.Mode = ModeOpus; c.Audio.Rate = 44100 }, true},
		{"opus stereo fails", func(c *Config) { c.Mode = ModeOpus; c.Audio.Rate = 48000; c.Audio.Channels = 2 }, true},
		{"format s24 fails", func(c *Config) { c.Audio.Format = "s24" }, true},
		{"name with CRLF fails", func(c *Config) { c.Name = "bad\r\nname" }, true},
		{"empty name fails", func(c *Config) { c.Name = "" }, true},
		{"empty listen fails", func(c *Config) { c.Listen = "" }, true},
		{"rate too high fails", func(c *Config) { c.Audio.Rate = 500000 }, true},
		{"rate too low fails", func(c *Config) { c.Audio.Rate = 100 }, true},
		{"channels 3 fails", func(c *Config) { c.Audio.Channels = 3 }, true},
		{"empty device fails", func(c *Config) { c.Audio.Device = "" }, true},
		{"unknown mode fails", func(c *Config) { c.Mode = "flac" }, true},
		{"negative bitrate fails", func(c *Config) { c.Opus.Bitrate = -1 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}
