// Package config defines the birdnet-go-remote-mic configuration surface and
// its validation. The rules are strict on purpose: an invalid capture rate or a
// mode/rate mismatch fails loudly at startup rather than silently degrading.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode selects the stream format.
type Mode string

const (
	// ModePCM streams raw L16 at the capture rate (the ultrasonic path).
	ModePCM Mode = "pcm"
	// ModeOpus encodes 48 kHz mono Opus (the normal-audio path).
	ModeOpus Mode = "opus"
)

// Config is the whole configuration surface.
type Config struct {
	Name      string    `yaml:"name"`
	Listen    string    `yaml:"listen"`
	Mode      Mode      `yaml:"mode"`
	Audio     Audio     `yaml:"audio"`
	Opus      Opus      `yaml:"opus"`
	Discovery Discovery `yaml:"discovery"`
}

// Discovery configures mDNS/DNS-SD advertisement. Enabled is a pointer so an
// absent block defaults to on while an explicit "enabled: false" turns it off.
type Discovery struct {
	Enabled *bool `yaml:"enabled"`
}

// DiscoveryEnabled reports whether mDNS advertisement is on (the default).
func (c *Config) DiscoveryEnabled() bool {
	return c.Discovery.Enabled == nil || *c.Discovery.Enabled
}

// Audio configures capture.
type Audio struct {
	Device   string `yaml:"device"`
	Rate     int    `yaml:"rate"`
	Channels int    `yaml:"channels"`
	Format   string `yaml:"format"`
}

// Opus configures the Opus encoder (used only when Mode is ModeOpus).
type Opus struct {
	Bitrate int `yaml:"bitrate"`
}

// ValidationError reports a single invalid configuration field.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config: %s: %s", e.Field, e.Reason)
}

// Load reads, defaults, and validates a YAML config file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied CLI flag
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8554"
	}
	if c.Mode == "" {
		c.Mode = ModePCM
	}
	if c.Audio.Channels == 0 {
		c.Audio.Channels = 1
	}
	if c.Audio.Format == "" {
		c.Audio.Format = "s16"
	}
}

// Validate checks every field, returning the first *ValidationError found. The
// pointer receiver avoids copying the config and does not mutate it.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return &ValidationError{"name", "must not be empty"}
	}
	if strings.ContainsAny(c.Name, "\r\n") {
		return &ValidationError{"name", "must not contain CR or LF"}
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return &ValidationError{"listen", "must be host:port"}
	}
	switch c.Mode {
	case ModePCM, ModeOpus:
	default:
		return &ValidationError{"mode", "must be pcm or opus"}
	}
	if c.Audio.Device == "" {
		return &ValidationError{"audio.device", "must not be empty"}
	}
	if c.Audio.Format != "s16" {
		return &ValidationError{"audio.format", "must be s16"}
	}
	if c.Audio.Rate < 8000 || c.Audio.Rate > 384000 {
		return &ValidationError{"audio.rate", "must be between 8000 and 384000 Hz"}
	}
	if c.Audio.Channels < 1 || c.Audio.Channels > 2 {
		return &ValidationError{"audio.channels", "must be 1 or 2"}
	}
	if c.Mode == ModeOpus {
		if c.Audio.Rate != 48000 {
			return &ValidationError{"audio.rate", "opus mode requires 48000 Hz"}
		}
		if c.Audio.Channels != 1 {
			return &ValidationError{"audio.channels", "opus mode requires 1 channel"}
		}
	}
	if c.Opus.Bitrate < 0 {
		return &ValidationError{"opus.bitrate", "must not be negative"}
	}
	return nil
}
