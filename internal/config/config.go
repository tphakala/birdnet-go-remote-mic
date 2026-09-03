// Package config defines the birdnet-go-remote-mic configuration surface and
// its validation. The rules are strict on purpose: an invalid capture rate or a
// mode/rate mismatch fails loudly at startup rather than silently degrading.
package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
)

// maxDevices caps the device list, matching the API contract's maxItems.
const maxDevices = 32

// MaxChannels caps a device's channel selection. It matches the OpenAPI
// ProvisionDeviceRequest.channels maxItems and covers common multi-channel USB
// interfaces (2/4/6/8 channels).
const MaxChannels = 8

// NormalizeChannels returns a sorted, de-duplicated copy of a channel selection.
// ApplyDefaults and the device-provisioning path share it so the canonical
// channel order is defined in exactly one place. slices.Sorted already collects
// into a fresh slice, so the result never aliases sel.
func NormalizeChannels(sel []int) []int {
	return slices.Compact(slices.Sorted(slices.Values(sel)))
}

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
	Listen     string     `yaml:"listen"`
	Discovery  Discovery  `yaml:"discovery"`
	Management Management `yaml:"management"`
	Auth       Auth       `yaml:"auth,omitempty"`
	Devices    []Device   `yaml:"devices"`
}

// Auth configures the shared access token that gates the management API and
// web UI (Bearer) and the RTSP stream (Digest). An empty token means open
// access, the default. See auth.ValidToken for the token shape.
type Auth struct {
	Token string `yaml:"token,omitempty"`
}

// AuthRequired reports whether a token is set, so clients must authenticate.
func (c *Config) AuthRequired() bool {
	return c.Auth.Token != ""
}

// Management configures the HTTPS management API. Enabled is a pointer so an
// absent block defaults to on while an explicit "enabled: false" turns it off.
type Management struct {
	Enabled *bool  `yaml:"enabled,omitempty"`  // default on
	Listen  string `yaml:"listen,omitempty"`   // HTTPS listen address (host:port); default ":8443"
	CertDir string `yaml:"cert_dir,omitempty"` // default: the config file's directory
}

// ManagementEnabled reports whether the management API is on (the default).
func (c *Config) ManagementEnabled() bool {
	return c.Management.Enabled == nil || *c.Management.Enabled
}

// Device configures one capture device and the stream it serves.
type Device struct {
	Name     string `yaml:"name"`           // DNS-SD instance name and log label; unique
	Device   string `yaml:"device"`         // go-audio-capture device id, e.g. "hw:1,0"
	Path     string `yaml:"path"`           // RTSP path, e.g. "/garden"; unique; default "/stream"
	Mode     Mode   `yaml:"mode"`           // pcm or opus
	Rate     int    `yaml:"rate"`           // capture sample rate in Hz
	Channels []int  `yaml:"channels"`       // 1-based channel numbers to stream, e.g. [1] or [1,2] or [1,3]
	Format   string `yaml:"format"`         // only "s16"
	Opus     Opus   `yaml:"opus,omitempty"` // used only when Mode is opus
	// Enabled is a pointer so an absent value defaults on: a device is captured
	// and streamed unless explicitly disabled. A disabled device stays in the
	// config (and is shown in the UI) but is not opened; toggling it takes effect
	// at once via a config reload, which starts or stops the device in place.
	Enabled *bool `yaml:"enabled,omitempty"`
}

// IsEnabled reports whether the device is captured and streamed. A device with
// no explicit enabled flag defaults on.
func (d *Device) IsEnabled() bool {
	return d.Enabled == nil || *d.Enabled
}

// Discovery configures mDNS/DNS-SD advertisement. Enabled is a pointer so an
// absent block defaults to on while an explicit "enabled: false" turns it off.
type Discovery struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// DiscoveryEnabled reports whether mDNS advertisement is on (the default).
func (c *Config) DiscoveryEnabled() bool {
	return c.Discovery.Enabled == nil || *c.Discovery.Enabled
}

// Opus configures the Opus encoder (used only when Mode is ModeOpus).
type Opus struct {
	Bitrate int `yaml:"bitrate,omitempty"`
}

// ValidationError reports a single invalid configuration field.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config: %s: %s", e.Field, e.Reason)
}

// Default returns a valid configuration with defaults applied and no devices. It
// is used on first run when no config file exists yet, so the appliance boots and
// the web UI can enumerate the host's capture hardware and provision devices; the
// first provisioning writes the config file.
func Default() Config {
	var c Config
	c.ApplyDefaults()
	return c
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
	// Detect the old single-device shape: a top-level `audio:` block and no
	// `devices:` list. Gating on `devices` being absent means a new config may
	// still use a top-level `audio:` YAML anchor to DRY its device blocks.
	if len(c.Devices) == 0 {
		var legacy struct {
			Audio map[string]any `yaml:"audio"`
		}
		if yaml.Unmarshal(data, &legacy) == nil && len(legacy.Audio) > 0 {
			return Config{}, fmt.Errorf("config: %s uses the old single-device format; move the audio settings into a devices: list", path)
		}
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if c.Auth.Token != "" {
		warnIfTokenFileReadable(path)
	}
	return c, nil
}

// warnIfTokenFileReadable logs a warning when path is accessible by group or
// other while it holds a shared access token. The check masks every group and
// other bit (0o077), so a group-writable or world-readable file both trip it:
// read leaks the secret and write lets a local account replace it. Save writes
// 0600, but a hand-edited or hand-copied file can be wider. The token is the
// bearer and Digest secret, so a non-owner-only file exposes it to every local account;
// the warning steers the operator to chmod 600 rather than silently accepting
// it.
func warnIfTokenFileReadable(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Printf("config: %s is accessible by group or other (mode %#o) but holds an access token; restrict it with: chmod 600 %s", path, perm, path)
	}
}

// ApplyDefaults fills in the defaults for any unset fields. It is idempotent, so
// calling it on an already-defaulted config is a no-op. Load applies it before
// validating; the config endpoints apply it to a patched config so an
// API-supplied device defaults identically to a file-loaded one.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8554"
	}
	if c.Management.Listen == "" {
		c.Management.Listen = ":8443"
	}
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Mode == "" {
			d.Mode = ModePCM
		}
		if len(d.Channels) == 0 {
			d.Channels = []int{1}
		} else {
			// Normalize the selection to canonical ascending-unique order so a
			// hand-edited [2,1] or [1,1] stores and validates the same as [1,2],
			// and downstream (open count, extraction) can assume sorted-unique.
			d.Channels = NormalizeChannels(d.Channels)
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
	if c.ManagementEnabled() {
		if _, _, err := net.SplitHostPort(c.Management.Listen); err != nil {
			return &ValidationError{"management.listen", "must be host:port"}
		}
	}
	if reason := auth.ValidToken(c.Auth.Token); reason != "" {
		return &ValidationError{"auth.token", reason}
	}
	// An empty device list is valid: on first run the appliance boots with no
	// configured devices so the web UI can enumerate the host's capture hardware
	// and let the operator enable devices from there. The management API keeps the
	// appliance up while nothing is serving (see run()).
	if len(c.Devices) > maxDevices {
		return &ValidationError{"devices", fmt.Sprintf("must not list more than %d devices", maxDevices)}
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
		if len(d.Channels) == 0 {
			return &ValidationError{field("channels"), "must select at least one channel"}
		}
		for j, ch := range d.Channels {
			if ch < 1 || ch > MaxChannels {
				return &ValidationError{field("channels"), fmt.Sprintf("channel numbers must be between 1 and %d", MaxChannels)}
			}
			if j > 0 && ch <= d.Channels[j-1] {
				return &ValidationError{field("channels"), "must be ascending with no duplicates"}
			}
		}
		if d.Mode == ModeOpus {
			if d.Rate != 48000 {
				return &ValidationError{field("rate"), "opus mode requires 48000 Hz"}
			}
			if len(d.Channels) != 1 {
				return &ValidationError{field("channels"), "opus mode requires exactly one channel"}
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
	case strings.HasSuffix(p, "/trackID=0"):
		return "must not end with /trackID=0 (reserved for the per-track SETUP URL)"
	default:
		return ""
	}
}

// Clone returns a deep copy of c. The Devices slice and every *bool field (the
// discovery and management enabled flags, and each device's Enabled flag) get
// their own backing storage, so a caller may mutate the copy (for example
// ApplyDefaults over a patched device list) without racing a concurrent reader
// of the original.
func (c *Config) Clone() Config {
	out := *c
	if c.Discovery.Enabled != nil {
		v := *c.Discovery.Enabled
		out.Discovery.Enabled = &v
	}
	if c.Management.Enabled != nil {
		v := *c.Management.Enabled
		out.Management.Enabled = &v
	}
	if c.Devices != nil {
		out.Devices = make([]Device, len(c.Devices))
		copy(out.Devices, c.Devices)
		// Device carries reference types (a *bool Enabled and a []int Channels);
		// give each copy its own backing storage so a caller mutating the clone
		// cannot race or alias the original.
		for i := range c.Devices {
			if c.Devices[i].Enabled != nil {
				v := *c.Devices[i].Enabled
				out.Devices[i].Enabled = &v
			}
			out.Devices[i].Channels = slices.Clone(c.Devices[i].Channels)
		}
	}
	return out
}

// Save marshals c to YAML and writes it to path atomically via atomicfile.Write
// (temp file, fsync, rename; symlink-preserving). The file is written 0600
// because the config holds the shared access token.
func Save(path string, c *Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}
