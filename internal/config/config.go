// Package config defines the birdnet-go-remote-mic configuration surface and
// its validation. The rules are strict on purpose: an invalid capture rate or a
// mode/rate mismatch fails loudly at startup rather than silently degrading.
package config

import (
	"errors"
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
	// ModeOpus encodes 48 kHz Opus, mono or stereo (one or two channels; the
	// normal-audio path).
	ModeOpus Mode = "opus"
)

// Config is the whole configuration surface.
type Config struct {
	Listen        string        `yaml:"listen"`
	Discovery     Discovery     `yaml:"discovery"`
	Management    Management    `yaml:"management"`
	Auth          Auth          `yaml:"auth,omitempty"`
	Notifications Notifications `yaml:"notifications,omitempty"`
	Devices       []Device      `yaml:"devices"`
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

// Notifications configures the condition monitors behind the notification
// center. Every field is a pointer so presence is preserved: an absent field
// (nil) is filled with its default by ApplyDefaults, while an explicitly
// provided value (including an out-of-range one such as 0) survives to Validate
// and is rejected. The clear-side durations are constants in the monitors, not
// config.
type Notifications struct {
	Enabled *bool       `yaml:"enabled,omitempty"` // default on
	Audio   AudioAlerts `yaml:"audio,omitempty"`
	Host    HostAlerts  `yaml:"host,omitempty"`
}

// Default threshold values, shared by ApplyDefaults (fills an absent field) and
// validate (the effective value of an absent field), so the two never drift.
const (
	defAudioQuietDbfs         = -60
	defAudioQuietSeconds      = 600
	defAudioZeroSeconds       = 30
	defAudioClipPercent       = 20
	defAudioClipWindowSeconds = 10
	defHostCPUPercent         = 90
	defHostCPUClearPercent    = 75
	defHostTempCelsius        = 80
	defHostTempClearCelsius   = 75
	defHostDiskPercent        = 90
	defHostDiskClearPercent   = 85
	defHostMemFreePercent     = 10
	defHostMemFreeMiB         = 64
)

// AudioAlerts holds the audio-signal condition thresholds. Each field is a
// pointer: nil means "unset, fill the default", a set value is validated as
// given (so an explicit out-of-range value is rejected, not silently defaulted).
type AudioAlerts struct {
	QuietDbfs         *int `yaml:"quiet_dbfs,omitempty"`          // default -60; -99..-1
	QuietSeconds      *int `yaml:"quiet_seconds,omitempty"`       // default 600; 10..86400
	ZeroSeconds       *int `yaml:"zero_seconds,omitempty"`        // default 30; 5..3600
	ClipPercent       *int `yaml:"clip_percent,omitempty"`        // default 20; 1..100
	ClipWindowSeconds *int `yaml:"clip_window_seconds,omitempty"` // default 10; 1..600
}

// HostAlerts holds the host-health condition thresholds. Each clear threshold
// must sit below its onset to keep a real hysteresis gap (a clear at or above
// the onset would chatter). Each field is a pointer: nil means "unset, fill the
// default", a set value is validated as given.
type HostAlerts struct {
	CPUPercent       *int `yaml:"cpu_percent,omitempty"`        // default 90; 1..100
	CPUClearPercent  *int `yaml:"cpu_clear_percent,omitempty"`  // default 75; 1..99, below cpu_percent
	TempCelsius      *int `yaml:"temp_celsius,omitempty"`       // default 80; 30..120
	TempClearCelsius *int `yaml:"temp_clear_celsius,omitempty"` // default 75; 1..119, below temp_celsius
	DiskPercent      *int `yaml:"disk_percent,omitempty"`       // default 90; 1..100
	DiskClearPercent *int `yaml:"disk_clear_percent,omitempty"` // default 85; 1..99, below disk_percent
	MemFreePercent   *int `yaml:"mem_free_percent,omitempty"`   // default 10; 1..90
	MemFreeMiB       *int `yaml:"mem_free_mib,omitempty"`       // default 64; 1..65536
}

// intOr returns *p, or def when p is nil. It gives the effective value of a
// threshold: an absent (nil) field reads as its default; a set field (including
// an out-of-range 0) reads as its literal value so validate can reject it.
func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// cloneIntPtr returns a copy of p with its own backing storage (nil stays nil).
func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// clone returns a deep copy of n: every pointer field (Enabled and all thresholds)
// gets its own backing storage, so a caller mutating the copy cannot alias the
// original. Used by Config.Clone, where a shallow struct copy would share the
// threshold pointers.
func (n *Notifications) clone() Notifications {
	out := Notifications{
		Audio: AudioAlerts{
			QuietDbfs:         cloneIntPtr(n.Audio.QuietDbfs),
			QuietSeconds:      cloneIntPtr(n.Audio.QuietSeconds),
			ZeroSeconds:       cloneIntPtr(n.Audio.ZeroSeconds),
			ClipPercent:       cloneIntPtr(n.Audio.ClipPercent),
			ClipWindowSeconds: cloneIntPtr(n.Audio.ClipWindowSeconds),
		},
		Host: HostAlerts{
			CPUPercent:       cloneIntPtr(n.Host.CPUPercent),
			CPUClearPercent:  cloneIntPtr(n.Host.CPUClearPercent),
			TempCelsius:      cloneIntPtr(n.Host.TempCelsius),
			TempClearCelsius: cloneIntPtr(n.Host.TempClearCelsius),
			DiskPercent:      cloneIntPtr(n.Host.DiskPercent),
			DiskClearPercent: cloneIntPtr(n.Host.DiskClearPercent),
			MemFreePercent:   cloneIntPtr(n.Host.MemFreePercent),
			MemFreeMiB:       cloneIntPtr(n.Host.MemFreeMiB),
		},
	}
	if n.Enabled != nil {
		v := *n.Enabled
		out.Enabled = &v
	}
	return out
}

// NotificationsEnabled reports whether the condition monitors run (the default).
// An absent block or an absent enabled flag both default on.
func (c *Config) NotificationsEnabled() bool {
	return c.Notifications.Enabled == nil || *c.Notifications.Enabled
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

// Device configures one capture device (opened once, exclusively) and the
// streams it fans out. A device is opened at a single rate and format; each
// Stream carries a chosen subset of the captured channels, encoded in its own
// mode, served at its own RTSP path. A single-stream device is the common case
// and can be written in the legacy flat form (see UnmarshalYAML), which migrates
// to a one-element Streams on load.
type Device struct {
	Name    string   `yaml:"name"`    // DNS-SD instance name and log label; unique
	Device  string   `yaml:"device"`  // go-audio-capture device id, e.g. "hw:1,0"
	Rate    int      `yaml:"rate"`    // capture sample rate in Hz (one ALSA open per device)
	Format  string   `yaml:"format"`  // only "s16"
	Streams []Stream `yaml:"streams"` // one or more streams fanned out from the one capture
	// Enabled is a pointer so an absent value defaults on: a device is captured
	// and streamed unless explicitly disabled. A disabled device stays in the
	// config (and is shown in the UI) but is not opened; toggling it takes effect
	// at once via a config reload, which starts or stops the device in place.
	Enabled *bool `yaml:"enabled,omitempty"`
	// QuietAlert is a pointer so an absent value defaults on: the device raises
	// the very-quiet audio condition unless explicitly opted out (for a bat
	// microphone that is silent by day). Stuck-at-zero and clipping are
	// unaffected by this flag.
	QuietAlert *bool `yaml:"quiet_alert,omitempty"`
}

// Stream is one RTSP stream fanned out from a device's shared capture: a subset
// of the device's channels, encoded in one mode, served at one path. Path is
// unique across every device's streams; Mode/Channels/Opus mirror the fields a
// single-stream device carried before fan-out.
type Stream struct {
	Path     string `yaml:"path"`           // RTSP path, e.g. "/garden"; globally unique; default "/stream" for a lone stream
	Mode     Mode   `yaml:"mode"`           // pcm or opus
	Channels []int  `yaml:"channels"`       // 1-based capture channel numbers to stream, e.g. [1] or [1,2] or [1,3]
	Opus     Opus   `yaml:"opus,omitempty"` // used only when Mode is opus
}

// UnmarshalYAML accepts both the current nested form (a streams: list) and the
// legacy flat single-stream form (path/mode/channels/opus directly on the
// device), migrating the flat form to a one-element Streams. A device that sets
// both the flat fields and streams: is rejected. A minimal device with neither
// (only name/device/rate) becomes a single default stream, matching the
// pre-fan-out behavior where every device served exactly one stream. Save always
// writes the nested form, so the migration is one-way on the first save.
func (d *Device) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Name   string `yaml:"name"`
		Device string `yaml:"device"`
		Rate   int    `yaml:"rate"`
		Format string `yaml:"format"`
		// Streams is a pointer so an omitted key (nil) is distinguished from an
		// explicitly empty list (streams: []): the former is the legacy/minimal flat
		// form to migrate, the latter is a present-but-empty list that must reach
		// Validate and be rejected ("must define at least one stream").
		Streams    *[]Stream `yaml:"streams"`
		Enabled    *bool     `yaml:"enabled"`
		QuietAlert *bool     `yaml:"quiet_alert"`
		// Legacy flat single-stream fields (pre-fan-out configs).
		Path     string `yaml:"path"`
		Mode     Mode   `yaml:"mode"`
		Channels []int  `yaml:"channels"`
		Opus     Opus   `yaml:"opus"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	d.Name = raw.Name
	d.Device = raw.Device
	d.Rate = raw.Rate
	d.Format = raw.Format
	d.Enabled = raw.Enabled
	d.QuietAlert = raw.QuietAlert

	flatUsed := raw.Path != "" || raw.Mode != "" || len(raw.Channels) > 0 || raw.Opus.Bitrate != 0
	if raw.Streams != nil {
		if flatUsed {
			return fmt.Errorf("device %q sets both the legacy flat stream fields (path/mode/channels/opus) and streams:; use streams: only", raw.Name)
		}
		// Keep the list as given, including an explicitly empty one so Validate can
		// reject it rather than a phantom default stream masking the mistake.
		d.Streams = *raw.Streams
		return nil
	}
	// Flat or minimal device (no streams key): synthesize the single stream. Empty
	// fields are filled by ApplyDefaults exactly as the flat device was defaulted
	// before.
	d.Streams = []Stream{{Path: raw.Path, Mode: raw.Mode, Channels: raw.Channels, Opus: raw.Opus}}
	return nil
}

// IsEnabled reports whether the device is captured and streamed. A device with
// no explicit enabled flag defaults on.
func (d *Device) IsEnabled() bool {
	return d.Enabled == nil || *d.Enabled
}

// QuietAlertEnabled reports whether the device raises the very-quiet audio
// condition. A device with no explicit quiet_alert flag defaults on.
func (d *Device) QuietAlertEnabled() bool {
	return d.QuietAlert == nil || *d.QuietAlert
}

// StreamChannelUnion returns the sorted, de-duplicated union of every stream's
// 1-based channel selection: the set of capture channels the device must open to
// serve all its streams from one shared capture. A device with no streams yields
// an empty selection, which the open path rounds up to one channel.
func (d *Device) StreamChannelUnion() []int {
	var all []int
	for i := range d.Streams {
		all = append(all, d.Streams[i].Channels...)
	}
	return NormalizeChannels(all)
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
	// Bitrate is the target in bits per second; zero means the default,
	// OpusDefaultBitrate for the stream's channel count.
	Bitrate int `yaml:"bitrate,omitempty"`
}

// Opus bitrate defaults: 128 kbps for each channel carried, capped at the top of
// the bitrate range Opus supports (510 kbps, RFC 6716 section 2.1.1).
const (
	OpusBitratePerChannel = 128000
	OpusMaxBitrate        = 510000
)

// OpusDefaultBitrate returns the default Opus bitrate for a stream carrying the
// given number of channels.
func OpusDefaultBitrate(channels int) int {
	return min(OpusBitratePerChannel*max(1, channels), OpusMaxBitrate)
}

// EffectiveBitrate returns the configured bitrate, or the default for the
// given channel count when none is configured.
func (o Opus) EffectiveBitrate(channels int) int {
	if o.Bitrate > 0 {
		return o.Bitrate
	}
	return OpusDefaultBitrate(channels)
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

// LoadOrDefault loads and validates the config at path, or returns a fresh
// Default() when the file does not exist yet, so first-run callers boot with a
// usable config without one present. Any other load error (a parse or validation
// failure) is returned unchanged rather than masked as a default. It is the one
// place the first-run load-or-default decision lives, shared by serve and the
// token commands.
func LoadOrDefault(path string) (Config, error) {
	c, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
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
		if d.Format == "" {
			d.Format = "s16"
		}
		// A lone stream defaults its path to "/stream"; a device with several
		// streams has no single obvious path, so an empty one there stays empty and
		// is rejected by validation. singleStream is read before the loop because it
		// does not change within it.
		singleStream := len(d.Streams) == 1
		for j := range d.Streams {
			s := &d.Streams[j]
			if s.Mode == "" {
				s.Mode = ModePCM
			}
			if len(s.Channels) == 0 {
				s.Channels = []int{1}
			} else {
				// Normalize the selection to canonical ascending-unique order so a
				// hand-edited [2,1] or [1,1] stores and validates the same as [1,2],
				// and downstream (open count, extraction) can assume sorted-unique.
				s.Channels = NormalizeChannels(s.Channels)
			}
			if s.Path == "" && singleStream {
				s.Path = "/stream"
			}
		}
	}

	// Notification thresholds: a nil field is unset, so fill its default. An
	// explicit value (even an out-of-range 0) is left non-nil for Validate to
	// reject rather than being silently overwritten. Enabled and the per-device
	// QuietAlert stay nil (both default on) so an unset flag is omitted from the
	// saved YAML, matching Discovery/Management.
	n := &c.Notifications
	fill := func(p **int, def int) {
		if *p == nil {
			v := def
			*p = &v
		}
	}
	fill(&n.Audio.QuietDbfs, defAudioQuietDbfs)
	fill(&n.Audio.QuietSeconds, defAudioQuietSeconds)
	fill(&n.Audio.ZeroSeconds, defAudioZeroSeconds)
	fill(&n.Audio.ClipPercent, defAudioClipPercent)
	fill(&n.Audio.ClipWindowSeconds, defAudioClipWindowSeconds)
	fill(&n.Host.CPUPercent, defHostCPUPercent)
	fill(&n.Host.CPUClearPercent, defHostCPUClearPercent)
	fill(&n.Host.TempCelsius, defHostTempCelsius)
	fill(&n.Host.TempClearCelsius, defHostTempClearCelsius)
	fill(&n.Host.DiskPercent, defHostDiskPercent)
	fill(&n.Host.DiskClearPercent, defHostDiskClearPercent)
	fill(&n.Host.MemFreePercent, defHostMemFreePercent)
	fill(&n.Host.MemFreeMiB, defHostMemFreeMiB)
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
	if err := c.validateDevices(); err != nil {
		return err
	}
	if err := c.Notifications.validate(); err != nil {
		return err
	}
	return nil
}

// validateDevices checks the device list: the count cap, each device's fields,
// and the uniqueness of names, paths and device ids.
func (c *Config) validateDevices() error {
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
		if d.Format != "s16" {
			return &ValidationError{field("format"), "must be s16"}
		}
		if d.Rate < 8000 || d.Rate > 384000 {
			return &ValidationError{field("rate"), "must be between 8000 and 384000 Hz"}
		}
		if len(d.Streams) == 0 {
			return &ValidationError{field("streams"), "must define at least one stream"}
		}
		// Cap the streams per device. Streams may share a capture channel (for
		// example a PCM and an Opus stream of the same mic), so this is a plain
		// policy limit reusing MaxChannels as a sensible bound, not a one-per-channel
		// rule.
		if len(d.Streams) > MaxChannels {
			return &ValidationError{field("streams"), fmt.Sprintf("must not define more than %d streams", MaxChannels)}
		}
		if names[d.Name] {
			return &ValidationError{field("name"), "duplicate name " + strconv.Quote(d.Name)}
		}
		if ids[d.Device] {
			return &ValidationError{field("device"), "duplicate device " + strconv.Quote(d.Device) + " (ALSA hw devices are single-client)"}
		}
		names[d.Name] = true
		ids[d.Device] = true
		if err := validateStreams(d, paths, field); err != nil {
			return err
		}
	}
	return nil
}

// validateStreams checks one device's streams and records their paths in the
// shared, global paths set so a path is unique across every device's streams.
// The opus-rate rule is enforced against the device rate because one ALSA open
// serves every stream at a single rate. field builds a "devices[i].<f>" label;
// per-stream errors extend it to "streams[j].<f>".
func validateStreams(d *Device, paths map[string]bool, field func(string) string) error {
	for j := range d.Streams {
		s := &d.Streams[j]
		sfield := func(f string) string { return field(fmt.Sprintf("streams[%d].%s", j, f)) }
		if reason := validatePath(s.Path); reason != "" {
			return &ValidationError{sfield("path"), reason}
		}
		switch s.Mode {
		case ModePCM, ModeOpus:
		default:
			return &ValidationError{sfield("mode"), "must be pcm or opus"}
		}
		if len(s.Channels) == 0 {
			return &ValidationError{sfield("channels"), "must select at least one channel"}
		}
		for k, ch := range s.Channels {
			if ch < 1 || ch > MaxChannels {
				return &ValidationError{sfield("channels"), fmt.Sprintf("channel numbers must be between 1 and %d", MaxChannels)}
			}
			if k > 0 && ch <= s.Channels[k-1] {
				return &ValidationError{sfield("channels"), "must be ascending with no duplicates"}
			}
		}
		if s.Mode == ModeOpus {
			if d.Rate != 48000 {
				return &ValidationError{field("rate"), "opus mode requires the device rate to be 48000 Hz"}
			}
			if len(s.Channels) > 2 {
				return &ValidationError{sfield("channels"), "opus mode requires one or two channels"}
			}
		}
		if s.Opus.Bitrate < 0 {
			return &ValidationError{sfield("opus.bitrate"), "must not be negative"}
		}
		if s.Opus.Bitrate > OpusMaxBitrate {
			return &ValidationError{sfield("opus.bitrate"), fmt.Sprintf("must be at most %d", OpusMaxBitrate)}
		}
		if paths[s.Path] {
			return &ValidationError{sfield("path"), "duplicate path " + strconv.Quote(s.Path) + " (paths are unique across every device's streams)"}
		}
		paths[s.Path] = true
	}
	return nil
}

// reasonPercent1To100 is the shared validation reason for the percent thresholds
// bounded to the inclusive 1..100 range (clip, cpu and disk onset percentages).
const reasonPercent1To100 = "must be between 1 and 100"

// validate range-checks the notification thresholds using each field's effective
// value: a nil (absent) field reads as its default, which is always valid, while
// a set field is checked as given, so an explicit out-of-range value (such as 0)
// is rejected instead of being silently defaulted. It therefore does not depend
// on ApplyDefaults having run first. Each clear threshold must sit below its
// onset so the hysteresis gap is real; a clear at or above the onset would
// chatter the condition.
func (n *Notifications) validate() error {
	a := &n.Audio
	if v := intOr(a.QuietDbfs, defAudioQuietDbfs); v < -99 || v > -1 {
		return &ValidationError{"notifications.audio.quiet_dbfs", "must be between -99 and -1"}
	}
	if v := intOr(a.QuietSeconds, defAudioQuietSeconds); v < 10 || v > 86400 {
		return &ValidationError{"notifications.audio.quiet_seconds", "must be between 10 and 86400"}
	}
	if v := intOr(a.ZeroSeconds, defAudioZeroSeconds); v < 5 || v > 3600 {
		return &ValidationError{"notifications.audio.zero_seconds", "must be between 5 and 3600"}
	}
	if v := intOr(a.ClipPercent, defAudioClipPercent); v < 1 || v > 100 {
		return &ValidationError{"notifications.audio.clip_percent", reasonPercent1To100}
	}
	if v := intOr(a.ClipWindowSeconds, defAudioClipWindowSeconds); v < 1 || v > 600 {
		return &ValidationError{"notifications.audio.clip_window_seconds", "must be between 1 and 600"}
	}
	h := &n.Host
	cpu := intOr(h.CPUPercent, defHostCPUPercent)
	if cpu < 1 || cpu > 100 {
		return &ValidationError{"notifications.host.cpu_percent", reasonPercent1To100}
	}
	cpuClear := intOr(h.CPUClearPercent, defHostCPUClearPercent)
	if cpuClear < 1 || cpuClear > 99 {
		return &ValidationError{"notifications.host.cpu_clear_percent", "must be between 1 and 99"}
	}
	if cpuClear >= cpu {
		return &ValidationError{"notifications.host.cpu_clear_percent", "must be below cpu_percent"}
	}
	temp := intOr(h.TempCelsius, defHostTempCelsius)
	if temp < 30 || temp > 120 {
		return &ValidationError{"notifications.host.temp_celsius", "must be between 30 and 120"}
	}
	tempClear := intOr(h.TempClearCelsius, defHostTempClearCelsius)
	if tempClear < 1 || tempClear > 119 {
		return &ValidationError{"notifications.host.temp_clear_celsius", "must be between 1 and 119"}
	}
	if tempClear >= temp {
		return &ValidationError{"notifications.host.temp_clear_celsius", "must be below temp_celsius"}
	}
	disk := intOr(h.DiskPercent, defHostDiskPercent)
	if disk < 1 || disk > 100 {
		return &ValidationError{"notifications.host.disk_percent", reasonPercent1To100}
	}
	diskClear := intOr(h.DiskClearPercent, defHostDiskClearPercent)
	if diskClear < 1 || diskClear > 99 {
		return &ValidationError{"notifications.host.disk_clear_percent", "must be between 1 and 99"}
	}
	if diskClear >= disk {
		return &ValidationError{"notifications.host.disk_clear_percent", "must be below disk_percent"}
	}
	if v := intOr(h.MemFreePercent, defHostMemFreePercent); v < 1 || v > 90 {
		return &ValidationError{"notifications.host.mem_free_percent", "must be between 1 and 90"}
	}
	if v := intOr(h.MemFreeMiB, defHostMemFreeMiB); v < 1 || v > 65536 {
		return &ValidationError{"notifications.host.mem_free_mib", "must be between 1 and 65536"}
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

// Clone returns a deep copy of c. The Devices slice and every pointer field (the
// discovery, management and notifications enabled flags, each device's Enabled
// and QuietAlert flags, and the notification thresholds) get their own backing
// storage, so a caller may mutate the copy (for example ApplyDefaults over a
// patched device list) without racing a concurrent reader of the original.
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
	// Notifications carries pointer fields (Enabled plus the presence-aware
	// thresholds); the struct assignment above only shallow-copied them, so give
	// the whole block its own backing storage.
	out.Notifications = c.Notifications.clone()
	if c.Devices != nil {
		out.Devices = make([]Device, len(c.Devices))
		copy(out.Devices, c.Devices)
		// Device carries reference types (the *bool Enabled and QuietAlert flags and
		// a []Stream, each stream with its own []int Channels); give each copy its own
		// backing storage so a caller mutating the clone cannot race or alias the
		// original.
		for i := range c.Devices {
			if c.Devices[i].Enabled != nil {
				v := *c.Devices[i].Enabled
				out.Devices[i].Enabled = &v
			}
			if c.Devices[i].QuietAlert != nil {
				v := *c.Devices[i].QuietAlert
				out.Devices[i].QuietAlert = &v
			}
			if c.Devices[i].Streams != nil {
				out.Devices[i].Streams = make([]Stream, len(c.Devices[i].Streams))
				copy(out.Devices[i].Streams, c.Devices[i].Streams)
				for j := range c.Devices[i].Streams {
					out.Devices[i].Streams[j].Channels = slices.Clone(c.Devices[i].Streams[j].Channels)
				}
			}
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
