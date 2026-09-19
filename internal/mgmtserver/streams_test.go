package mgmtserver

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

const (
	nameIface       = "iface"
	fieldDevStreams = "devices[0].streams"
	devIface        = "hw:3,0"
	pathNorth       = "/north"
	pathSouth       = "/south"
)

// twoStreamConfig seeds a store with one device fanning out two PCM streams.
func twoStreamConfig(t *testing.T) (store *FileConfigStore, path string) {
	t.Helper()
	c := config.Config{
		Listen: rtspAddr,
		Devices: []config.Device{{
			Name: nameIface, Device: devIface, Rate: 48000, Format: fmtS16,
			Streams: []config.Stream{
				{Path: pathNorth, Mode: config.ModePCM, Channels: []int{1}},
				{Path: pathSouth, Mode: config.ModePCM, Channels: []int{2}},
			},
		}},
	}
	c.ApplyDefaults()
	path = filepath.Join(t.TempDir(), "config.yaml")
	store = NewFileConfigStore(path, &c)
	return store, path
}

func TestPatchConfigStreamsAuthoritative(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	streams := []mgmtapi.StreamConfig{
		{Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1}},
		{Path: pathSouth, Mode: mgmtapi.Pcm, Channels: []int{2}},
	}
	// The flat fields are carried by the wire type but must be IGNORED because
	// streams is present: set them to a decoy path to prove they are not persisted.
	devs := []mgmtapi.DeviceConfig{{
		Name: nameIface, Device: devIface, Rate: 48000, Format: mgmtapi.DeviceConfigFormatS16,
		Path: "/decoy", Mode: mgmtapi.Opus, Channels: []int{1},
		Streams: &streams,
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	rd := ok200.Config.Devices[0]
	// The response flat fields project the FIRST stream, not the decoy.
	if rd.Path != pathNorth || rd.Mode != mgmtapi.Pcm {
		t.Errorf("flat projection = %q/%v, want /north/pcm (first stream, not the decoy)", rd.Path, rd.Mode)
	}
	if rd.Streams == nil || len(*rd.Streams) != 2 || (*rd.Streams)[1].Path != pathSouth {
		t.Fatalf("wire streams = %+v, want two ending in /south", rd.Streams)
	}
	// Persisted with both streams and no decoy.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := loaded.Devices[0].Streams
	if len(got) != 2 || got[0].Path != pathNorth || got[1].Path != pathSouth {
		t.Errorf("persisted streams = %+v, want /north and /south", got)
	}
}

func TestPatchConfigRejectsFlatCollapseOfMultiStream(t *testing.T) {
	store, _ := twoStreamConfig(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	// A flat (streams-less) patch for the same device would collapse two streams to
	// one; a client that predates fan-out cannot drop streams it cannot see, so this
	// must be rejected rather than silently applied.
	devs := []mgmtapi.DeviceConfig{{
		Name: nameIface, Device: devIface, Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1},
		Format: mgmtapi.DeviceConfigFormatS16,
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 422 (multi-stream collapse guard)", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != fieldDevStreams {
		t.Errorf("errors = %+v, want one for devices[0].streams", vp.Errors)
	}
	// The stored device must still carry both streams.
	if got := store.Config().Devices[0].Streams; len(got) != 2 {
		t.Errorf("stored streams = %d, want 2 (the rejected patch must not collapse it)", len(got))
	}
}

func TestPatchConfigRenameDoesNotBypassCollapseGuard(t *testing.T) {
	// Renaming a multi-stream device in a streams-less (flat) patch must NOT slip a
	// collapse past the guard: it matches on the stable device id, not the name.
	store, _ := twoStreamConfig(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	devs := []mgmtapi.DeviceConfig{{
		// Same hw id, new name, no streams array; a valid Rate so the ONLY thing that
		// can 422 is the collapse guard, not an incidental field error.
		Name: "iface-renamed", Device: devIface, Rate: 48000,
		Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1}, Format: mgmtapi.DeviceConfigFormatS16,
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 422 (rename must not bypass the collapse guard)", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != fieldDevStreams {
		t.Errorf("errors = %+v, want one for devices[0].streams (the collapse guard, not a field error)", vp.Errors)
	}
	if got := store.Config().Devices[0].Streams; len(got) != 2 {
		t.Errorf("stored streams = %d, want 2 (a rename+flat patch must not collapse the device)", len(got))
	}
}

func TestPatchConfigDeviceIDChangeDoesNotBypassCollapseGuard(t *testing.T) {
	// Moving a multi-stream device to a different card (device id changes, name
	// unchanged) in a streams-less flat patch must NOT bypass the guard: it matches
	// by name as well as by device id.
	store, _ := twoStreamConfig(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	devs := []mgmtapi.DeviceConfig{{
		// Same name, new hw id, no streams array; a valid Rate so the ONLY thing that
		// can 422 is the collapse guard, not an incidental field error.
		Name: nameIface, Device: "hw:9,0", Rate: 48000,
		Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1}, Format: mgmtapi.DeviceConfigFormatS16,
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 422 (device-id change must not bypass the collapse guard)", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != fieldDevStreams {
		t.Errorf("errors = %+v, want one for devices[0].streams (the collapse guard, not a field error)", vp.Errors)
	}
	if got := store.Config().Devices[0].Streams; len(got) != 2 {
		t.Errorf("stored streams = %d, want 2 (a device-id change must not collapse the device)", len(got))
	}
}

func TestPatchConfigStreamsCanReduceCount(t *testing.T) {
	// A body that DOES carry streams is authoritative and may legitimately reduce a
	// device to a single stream (the guard only blocks the flat, streams-less path).
	store, _ := twoStreamConfig(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	one := []mgmtapi.StreamConfig{{Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1}}}
	devs := []mgmtapi.DeviceConfig{{
		Name: nameIface, Device: devIface, Rate: 48000, Format: mgmtapi.DeviceConfigFormatS16,
		Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1}, Streams: &one,
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	if _, ok := resp.(mgmtapi.PatchConfig200JSONResponse); !ok {
		t.Fatalf("PatchConfig returned %T, want 200 (explicit streams may reduce the count)", resp)
	}
	if got := store.Config().Devices[0].Streams; len(got) != 1 {
		t.Errorf("stored streams = %d, want 1", len(got))
	}
}

func TestGetConfigProjectsFirstStreamAndListsAll(t *testing.T) {
	store, _ := twoStreamConfig(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	got := resp.(mgmtapi.GetConfig200JSONResponse)
	d := got.Devices[0]
	if d.Path != pathNorth || d.Mode != mgmtapi.Pcm {
		t.Errorf("flat projection = %q/%v, want /north/pcm", d.Path, d.Mode)
	}
	if d.Streams == nil || len(*d.Streams) != 2 {
		t.Fatalf("streams = %+v, want 2", d.Streams)
	}
	if (*d.Streams)[0].Path != pathNorth || (*d.Streams)[1].Path != pathSouth {
		t.Errorf("streams paths = %v, want [/north /south]", *d.Streams)
	}
}

func TestListDevicesMapsPerStreamStatus(t *testing.T) {
	dev := DeviceStatus{
		Config: config.Device{
			Name: nameIface, Device: devIface, Rate: 48000, Format: fmtS16,
			Streams: []config.Stream{
				{Path: pathNorth, Mode: config.ModePCM, Channels: []int{1}},
				{Path: pathSouth, Mode: config.ModePCM, Channels: []int{2}},
			},
		},
		State:              StateServing,
		NegotiatedRate:     48000,
		NegotiatedChannels: 2,
		ClientConnected:    true, // any stream connected
		DroppedFrames:      7,    // summed
		Streams: []StreamStatus{
			{Path: pathNorth, ClientConnected: true, DroppedFrames: 5},
			{Path: pathSouth, ClientConnected: false, DroppedFrames: 2},
		},
	}
	s := New(&fakeProvider{devices: []DeviceStatus{dev}})
	resp, err := s.ListDevices(context.Background(), mgmtapi.ListDevicesRequestObject{})
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	list := resp.(mgmtapi.ListDevices200JSONResponse)
	d := list[0]
	if d.NegotiatedChannels == nil || *d.NegotiatedChannels != 2 {
		t.Errorf("negotiatedChannels = %v, want 2 (opened hardware count)", d.NegotiatedChannels)
	}
	if !d.ClientConnected || d.DroppedFrames != 7 {
		t.Errorf("device aggregate: connected=%v dropped=%d, want true/7", d.ClientConnected, d.DroppedFrames)
	}
	if d.Streams == nil || len(*d.Streams) != 2 {
		t.Fatalf("wire streams = %+v, want 2", d.Streams)
	}
	ws := *d.Streams
	if ws[0].Path != pathNorth || !ws[0].ClientConnected || ws[0].DroppedFrames != 5 {
		t.Errorf("stream[0] = %+v, want /north connected drops=5", ws[0])
	}
	if ws[1].Path != pathSouth || ws[1].ClientConnected || ws[1].DroppedFrames != 2 {
		t.Errorf("stream[1] = %+v, want /south not-connected drops=2", ws[1])
	}
}

// TestDeviceMapsStreamedChannels asserts streamedChannels is the union of every
// stream's selection (channels covers only the first stream), for both the
// live device view and the freshly provisioned projection.
func TestDeviceMapsStreamedChannels(t *testing.T) {
	cfg := config.Device{
		Name: nameIface, Device: devIface, Rate: 48000, Format: fmtS16,
		Streams: []config.Stream{
			{Path: pathNorth, Mode: config.ModePCM, Channels: []int{3}},
			{Path: pathSouth, Mode: config.ModePCM, Channels: []int{1, 3}},
		},
	}
	live := mapDevice(&DeviceStatus{Config: cfg, State: StateServing})
	if live.StreamedChannels == nil || !slices.Equal(*live.StreamedChannels, []int{1, 3}) {
		t.Errorf("live streamedChannels = %v, want [1 3]", live.StreamedChannels)
	}
	if !slices.Equal(live.Channels, []int{3}) {
		t.Errorf("live channels = %v, want the first stream's [3]", live.Channels)
	}
	provisioned := configDeviceToWireDevice(&cfg)
	if provisioned.StreamedChannels == nil || !slices.Equal(*provisioned.StreamedChannels, []int{1, 3}) {
		t.Errorf("provisioned streamedChannels = %v, want [1 3]", provisioned.StreamedChannels)
	}
}
