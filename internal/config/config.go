// Package config defines the birdnet-go-remote-mic configuration surface and
// its validation. The rules are strict on purpose: an invalid capture rate or a
// mode/rate mismatch fails loudly at startup rather than silently degrading.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
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
	Listen    string    `yaml:"listen"`
	Discovery Discovery `yaml:"discovery"`
	Devices   []Device  `yaml:"devices"`
}

// Device configures one capture device and the stream it serves.
type Device struct {
	Name     string `yaml:"name"`     // DNS-SD instance name and log label; unique
	Device   string `yaml:"device"`   // go-audio-capture device id, e.g. "hw:1,0"
	Path     string `yaml:"path"`     // RTSP path, e.g. "/garden"; unique; default "/stream"
	Mode     Mode   `yaml:"mode"`     // pcm or opus
	Rate     int    `yaml:"rate"`     // capture sample rate in Hz
	Channels int    `yaml:"channels"` // 1 or 2
	Format   string `yaml:"format"`   // only "s16"
	Opus     Opus   `yaml:"opus"`     // used only when Mode is opus
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
	var legacy struct {
		Audio map[string]any `yaml:"audio"`
	}
	if yaml.Unmarshal(data, &legacy) == nil && len(legacy.Audio) > 0 {
		return Config{}, fmt.Errorf("config: %s uses the old single-device format; move the audio settings into a devices: list (see config.example.yaml)", path)
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
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Mode == "" {
			d.Mode = ModePCM
		}
		if d.Channels == 0 {
			d.Channels = 1
		}
		if d.Format == "" {
			d.Format = "s16"
		}
		if d.Path == "" {
			d.Path = "/stream"
		}
	}
}

// Validate checks every field, returning the first *ValidationError found. The
// pointer receiver avoids copying the config and does not mutate it.
func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return &ValidationError{"listen", "must be host:port"}
	}
	if len(c.Devices) == 0 {
		return &ValidationError{"devices", "must list at least one device"}
	}
	names := make(map[string]bool, len(c.Devices))
	paths := make(map[string]bool, len(c.Devices))
	ids := make(map[string]bool, len(c.Devices))
	for i := range c.Devices {
		d := &c.Devices[i]
		field := func(f string) string { return fmt.Sprintf("devices[%d].%s", i, f) }
		if strings.TrimSpace(d.Name) == "" {
			return &ValidationError{field("name"), "must not be empty"}
		}
		if strings.ContainsAny(d.Name, "\r\n") {
			return &ValidationError{field("name"), "must not contain CR or LF"}
		}
		if d.Device == "" {
			return &ValidationError{field("device"), "must not be empty"}
		}
		if reason := validatePath(d.Path); reason != "" {
			return &ValidationError{field("path"), reason}
		}
		switch d.Mode {
		case ModePCM, ModeOpus:
		default:
			return &ValidationError{field("mode"), "must be pcm or opus"}
		}
		if d.Format != "s16" {
			return &ValidationError{field("format"), "must be s16"}
		}
		if d.Rate < 8000 || d.Rate > 384000 {
			return &ValidationError{field("rate"), "must be between 8000 and 384000 Hz"}
		}
		if d.Channels < 1 || d.Channels > 2 {
			return &ValidationError{field("channels"), "must be 1 or 2"}
		}
		if d.Mode == ModeOpus {
			if d.Rate != 48000 {
				return &ValidationError{field("rate"), "opus mode requires 48000 Hz"}
			}
			if d.Channels != 1 {
				return &ValidationError{field("channels"), "opus mode requires 1 channel"}
			}
		}
		if d.Opus.Bitrate < 0 {
			return &ValidationError{field("opus.bitrate"), "must not be negative"}
		}
		if names[d.Name] {
			return &ValidationError{field("name"), "duplicate name " + strconv.Quote(d.Name)}
		}
		if paths[d.Path] {
			return &ValidationError{field("path"), "duplicate path " + strconv.Quote(d.Path) + " (set an explicit unique path per device)"}
		}
		if ids[d.Device] {
			return &ValidationError{field("device"), "duplicate device " + strconv.Quote(d.Device) + " (ALSA hw devices are single-client)"}
		}
		names[d.Name] = true
		paths[d.Path] = true
		ids[d.Device] = true
	}
	return nil
}

// validatePath reports why an RTSP path is invalid, or "" when it is fine.
func validatePath(p string) string {
	switch {
	case !strings.HasPrefix(p, "/"):
		return "must start with /"
	case p == "/":
		return "must not be bare /"
	case strings.HasSuffix(p, "/"):
		return "must not end with /"
	case strings.ContainsAny(p, " \t\r\n"):
		return "must not contain whitespace"
	default:
		return ""
	}
}
