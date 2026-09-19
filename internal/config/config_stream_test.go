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
	cfgListen = ":8554"
	pathNorth = "/north"
	pathSouth = "/south"
)

// loadYAML writes body to a temp config file and loads it.
func loadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadFlatDeviceMigratesToSingleStream(t *testing.T) {
	c, err := loadYAML(t, `listen: ":8554"
devices:
  - name: mic
    device: "hw:1,0"
    path: /garden
    mode: pcm
    rate: 48000
    channels: [1]
    format: s16
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Devices[0]
	if len(d.Streams) != 1 {
		t.Fatalf("flat device produced %d streams, want 1", len(d.Streams))
	}
	s := d.Streams[0]
	if s.Path != "/garden" || s.Mode != ModePCM || !slices.Equal(s.Channels, []int{1}) {
		t.Errorf("migrated stream = %+v, want /garden pcm [1]", s)
	}
}

func TestLoadNestedStreams(t *testing.T) {
	c, err := loadYAML(t, `listen: ":8554"
devices:
  - name: iface
    device: "hw:1,0"
    rate: 48000
    format: s16
    streams:
      - { path: /north, mode: pcm, channels: [1] }
      - { path: /south, mode: opus, channels: [2] }
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Devices[0]
	if len(d.Streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(d.Streams))
	}
	if d.Streams[0].Path != pathNorth || d.Streams[0].Mode != ModePCM {
		t.Errorf("stream[0] = %+v, want /north pcm", d.Streams[0])
	}
	if d.Streams[1].Path != pathSouth || d.Streams[1].Mode != ModeOpus {
		t.Errorf("stream[1] = %+v, want /south opus", d.Streams[1])
	}
}

func TestLoadRejectsBothFlatAndStreams(t *testing.T) {
	_, err := loadYAML(t, `listen: ":8554"
devices:
  - name: mic
    device: "hw:1,0"
    rate: 48000
    format: s16
    path: /garden
    mode: pcm
    channels: [1]
    streams:
      - { path: /north, mode: pcm, channels: [1] }
`)
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("device with flat fields AND streams should be rejected, got %v", err)
	}
}

func TestLoadMinimalDeviceGetsDefaultStream(t *testing.T) {
	// A device with neither flat stream fields nor streams becomes one defaulted
	// stream, preserving the pre-fan-out behavior where every device served one.
	c, err := loadYAML(t, `listen: ":8554"
devices:
  - name: mic
    device: "hw:1,0"
    rate: 48000
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Devices[0]
	if len(d.Streams) != 1 {
		t.Fatalf("minimal device produced %d streams, want 1", len(d.Streams))
	}
	if d.Streams[0].Path != "/stream" || d.Streams[0].Mode != ModePCM || !slices.Equal(d.Streams[0].Channels, []int{1}) {
		t.Errorf("defaulted stream = %+v, want /stream pcm [1]", d.Streams[0])
	}
}

func TestLoadRejectsExplicitlyEmptyStreams(t *testing.T) {
	// An explicit `streams: []` is present-but-empty, distinct from an omitted key:
	// it must reach validation and be rejected, not silently get a default stream.
	_, err := loadYAML(t, `listen: ":8554"
devices:
  - name: a
    device: "hw:1,0"
    rate: 48000
    format: s16
    streams: []
`)
	if err == nil || !strings.Contains(err.Error(), "must define at least one stream") {
		t.Fatalf("streams: [] should be rejected as empty, got %v", err)
	}
}

func TestValidateRejectsDuplicatePathAcrossDevicesStreams(t *testing.T) {
	// Path uniqueness is global across every device's streams, not just within one
	// device: two devices both serving /dup must be rejected.
	_, err := loadYAML(t, `listen: ":8554"
devices:
  - name: a
    device: "hw:1,0"
    rate: 48000
    format: s16
    streams: [{ path: /dup, mode: pcm, channels: [1] }]
  - name: b
    device: "hw:2,0"
    rate: 48000
    format: s16
    streams: [{ path: /dup, mode: pcm, channels: [1] }]
`)
	if err == nil || !strings.Contains(err.Error(), "duplicate path") {
		t.Fatalf("duplicate path across devices' streams should be rejected, got %v", err)
	}
}

func TestValidateRejectsDuplicatePathWithinDevice(t *testing.T) {
	_, err := loadYAML(t, `listen: ":8554"
devices:
  - name: a
    device: "hw:1,0"
    rate: 48000
    format: s16
    streams:
      - { path: /same, mode: pcm, channels: [1] }
      - { path: /same, mode: pcm, channels: [2] }
`)
	if err == nil || !strings.Contains(err.Error(), "duplicate path") {
		t.Fatalf("duplicate path within one device should be rejected, got %v", err)
	}
}

func TestValidateRequiresAtLeastOneStream(t *testing.T) {
	c := Config{
		Listen:  cfgListen,
		Devices: []Device{{Name: "a", Device: deviceHW1, Rate: 48000, Format: formatS16}}, // no streams
	}
	c.ApplyDefaults()
	err := c.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "devices[0].streams" {
		t.Fatalf("Validate() = %v, want a devices[0].streams error", err)
	}
}

func TestValidateStreamCountCap(t *testing.T) {
	streams := make([]Stream, 0, MaxChannels+1)
	for i := 0; i <= MaxChannels; i++ {
		streams = append(streams, Stream{Path: "/s" + string(rune('a'+i)), Mode: ModePCM, Channels: []int{1}})
	}
	c := Config{
		Listen:  cfgListen,
		Devices: []Device{{Name: "a", Device: deviceHW1, Rate: 48000, Format: formatS16, Streams: streams}},
	}
	c.ApplyDefaults()
	err := c.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "devices[0].streams" {
		t.Fatalf("Validate() = %v, want a devices[0].streams count error", err)
	}
}

func TestValidateOpusStreamNeeds48k(t *testing.T) {
	// One shared capture rate serves every stream, so an opus stream forces the
	// device rate to 48000.
	_, err := loadYAML(t, `listen: ":8554"
devices:
  - name: a
    device: "hw:1,0"
    rate: 96000
    format: s16
    streams: [{ path: /a, mode: opus, channels: [1] }]
`)
	if err == nil || !strings.Contains(err.Error(), "48000") {
		t.Fatalf("opus stream on a non-48k device should be rejected, got %v", err)
	}
}

func TestApplyDefaultsMultiStreamDoesNotDefaultPath(t *testing.T) {
	// A lone stream defaults its path to /stream; a multi-stream device cannot, so a
	// stream missing its path is left empty and rejected by validation.
	_, err := loadYAML(t, `listen: ":8554"
devices:
  - name: a
    device: "hw:1,0"
    rate: 48000
    format: s16
    streams:
      - { mode: pcm, channels: [1] }
      - { path: /b, mode: pcm, channels: [2] }
`)
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("multi-stream device with a path-less stream should be rejected, got %v", err)
	}
}

func TestSaveRoundTripsMultiStreamDevice(t *testing.T) {
	c := Config{
		Listen: cfgListen,
		Devices: []Device{{
			Name: "iface", Device: deviceHW1, Rate: 48000, Format: formatS16,
			Streams: []Stream{
				{Path: pathNorth, Mode: ModePCM, Channels: []int{1}},
				{Path: pathSouth, Mode: ModeOpus, Channels: []int{2}, Opus: Opus{Bitrate: 96000}},
			},
		}},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(p, &c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := loaded.Devices[0].Streams
	if len(got) != 2 || got[0].Path != pathNorth || got[1].Path != pathSouth || got[1].Opus.Bitrate != 96000 {
		t.Errorf("round-tripped streams = %+v, want /north and /south (opus 96000)", got)
	}
}

func TestStreamChannelUnion(t *testing.T) {
	d := Device{Streams: []Stream{
		{Channels: []int{1}},
		{Channels: []int{2, 3}},
		{Channels: []int{1, 3}},
	}}
	if got := d.StreamChannelUnion(); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("StreamChannelUnion() = %v, want [1 2 3]", got)
	}
}

func TestCloneDeepCopiesStreams(t *testing.T) {
	c := Config{Devices: []Device{{
		Name: "a", Device: deviceHW1, Rate: 48000, Format: formatS16,
		Streams: []Stream{
			{Path: "/a", Mode: ModePCM, Channels: []int{1}},
			{Path: "/b", Mode: ModePCM, Channels: []int{2}},
		},
	}}}
	clone := c.Clone()
	clone.Devices[0].Streams[1].Channels[0] = 99
	clone.Devices[0].Streams[0].Path = "/mutated"
	if c.Devices[0].Streams[1].Channels[0] != 2 {
		t.Error("Clone aliased a stream's Channels backing array")
	}
	if c.Devices[0].Streams[0].Path != "/a" {
		t.Error("Clone aliased the Streams slice; original stream path changed")
	}
}
