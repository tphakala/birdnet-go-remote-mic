package mgmtserver

import (
	"context"
	"regexp"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

var hexPath = regexp.MustCompile(`^/[0-9a-f]{16}$`)

const (
	nameScarlett  = "Scarlett 2i2 USB"
	nameAudioMoth = "AudioMoth"
	slugAudioMoth = "audiomoth"
)

func TestSlug(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		nameScarlett:     "scarlett-2i2-usb",
		"  AudioMoth  ":  slugAudioMoth,
		"hw:1,0":         "hw-1-0",
		"USB   Audio!!!": "usb-audio",
		"---":            "",
		"":               "",
		"ÄÖÅ mic":        "mic",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveNameUniqueSuffix(t *testing.T) {
	t.Parallel()
	taken := map[string]bool{"scarlett-2i2-usb": true, "scarlett-2i2-usb-2": true}
	got := deriveName(nameScarlett, "hw:1,0", taken)
	if got != "scarlett-2i2-usb-3" {
		t.Errorf("deriveName collision = %q, want scarlett-2i2-usb-3", got)
	}
	// Empty friendly name falls back to the device id slug; no channel-mode suffix.
	if got := deriveName("", devAttic, map[string]bool{}); got != "hw-2-0" {
		t.Errorf("deriveName id fallback = %q, want hw-2-0", got)
	}
	// A name never carries a -stereo/-mono suffix.
	if got := deriveName(nameAudioMoth, devAttic, map[string]bool{}); got != slugAudioMoth {
		t.Errorf("deriveName = %q, want audiomoth (no channel suffix)", got)
	}
}

func TestRandomPathFormatAndUniqueness(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		p := randomPath(seen)
		if !hexPath.MatchString(p) {
			t.Fatalf("randomPath = %q, want /<16 hex>", p)
		}
		if seen[p] {
			t.Fatalf("randomPath returned a taken path %q", p)
		}
		seen[p] = true
	}
	// A pre-populated collision is retried, never returned.
	taken := map[string]bool{"/0000000000000000": true}
	if got := randomPath(taken); taken[got] {
		t.Errorf("randomPath returned a taken path %q", got)
	}
}

func modePtr(m mgmtapi.StreamMode) *mgmtapi.StreamMode { return &m }
func intPtr(v int) *int                                { return &v }

func TestChooseParams(t *testing.T) {
	t.Parallel()
	opusCapable := &AvailableDevice{SupportedRates: []int{44100, 48000, 96000}, SupportedChannels: []int{1, 2}}
	ultrasonic := &AvailableDevice{SupportedRates: []int{256000, 384000}, SupportedChannels: []int{1}}
	unprobed := &AvailableDevice{}

	tests := []struct {
		name     string
		dev      *AvailableDevice
		req      *mgmtapi.ProvisionDeviceRequest
		wantMode config.Mode
		wantRate int
		wantCh   int
	}{
		{"auto picks opus when 48k mono supported", opusCapable, &mgmtapi.ProvisionDeviceRequest{}, config.ModeOpus, 48000, 1},
		{"auto picks pcm at best rate for ultrasonic", ultrasonic, &mgmtapi.ProvisionDeviceRequest{}, config.ModePCM, 384000, 1},
		{"auto unprobed defaults pcm 48k mono", unprobed, &mgmtapi.ProvisionDeviceRequest{}, config.ModePCM, 48000, 1},
		{"explicit opus forces 48k mono", ultrasonic, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Opus)}, config.ModeOpus, 48000, 1},
		{"explicit pcm with rate override defaults mono", opusCapable, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm), Rate: intPtr(96000)}, config.ModePCM, 96000, 1},
		{"explicit pcm derives best rate", ultrasonic, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm)}, config.ModePCM, 384000, 1},
		{"channel override respected for pcm", ultrasonic, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm), Channels: intPtr(2)}, config.ModePCM, 384000, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mode, rate, ch := chooseParams(tt.dev, tt.req)
			if mode != tt.wantMode || rate != tt.wantRate || ch != tt.wantCh {
				t.Errorf("chooseParams = (%s, %d, %d), want (%s, %d, %d)", mode, rate, ch, tt.wantMode, tt.wantRate, tt.wantCh)
			}
		})
	}
}

func TestListAvailableDevices(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{384000}, SupportedChannels: []int{1}},
	}}
	s := New(prov, WithConfigStore(store))

	resp, err := s.ListAvailableDevices(context.Background(), mgmtapi.ListAvailableDevicesRequestObject{})
	if err != nil {
		t.Fatalf("ListAvailableDevices: %v", err)
	}
	got, ok := resp.(mgmtapi.ListAvailableDevices200JSONResponse)
	if !ok {
		t.Fatalf("returned %T, want 200", resp)
	}
	if len(got) != 1 || got[0].Device != devAttic || got[0].State != mgmtapi.Available {
		t.Fatalf("available = %+v, want one available hw:2,0", got)
	}
	if got[0].FriendlyName == nil || *got[0].FriendlyName != nameAudioMoth {
		t.Errorf("friendlyName = %v, want AudioMoth", got[0].FriendlyName)
	}
	if got[0].SupportedRates == nil || (*got[0].SupportedRates)[0] != 384000 {
		t.Errorf("supportedRates = %v, want [384000]", got[0].SupportedRates)
	}
}

func TestProvisionDeviceHappyPath(t *testing.T) {
	store, path := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{384000}, SupportedChannels: []int{1}},
	}}
	var reloaded bool
	reloader := func(_ context.Context, cfg config.Config) error {
		reloaded = true
		if len(cfg.Devices) != 2 {
			t.Errorf("reloader saw %d devices, want 2", len(cfg.Devices))
		}
		return nil
	}
	s := New(prov, WithConfigStore(store), WithReloader(reloader))

	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	created, ok := resp.(mgmtapi.ProvisionDevice201JSONResponse)
	if !ok {
		t.Fatalf("returned %T, want 201", resp)
	}
	if created.Device != devAttic || created.Name != slugAudioMoth {
		t.Errorf("created = %+v, want name audiomoth on hw:2,0", created)
	}
	// AudioMoth is 384k-only mono: no 48k, so PCM at 384k mono, never Opus.
	if created.Mode != mgmtapi.Pcm || created.Rate != 384000 || created.Channels != 1 {
		t.Errorf("created params = (%s, %d, %d), want (pcm, 384000, 1)", created.Mode, created.Rate, created.Channels)
	}
	if !hexPath.MatchString(created.Path) {
		t.Errorf("created path = %q, want /<16 hex>", created.Path)
	}
	if !reloaded {
		t.Error("reloader was not invoked")
	}

	// The new device persisted to disk alongside the seeded one.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if len(loaded.Devices) != 2 {
		t.Fatalf("persisted %d devices, want 2: %+v", len(loaded.Devices), loaded.Devices)
	}
}

func TestProvisionDeviceUnknownYields404(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: "hw:9,0"},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice404ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 404", resp)
	}
}

func TestProvisionDeviceMissingBodyYields422(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: "   "},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice422ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 422", resp)
	}
}

func TestProvisionDeviceAlreadyConfiguredYields409(t *testing.T) {
	// The provider advertises hw:1,0 as available, but the config already lists it
	// (an inconsistent state the persist-time guard must still catch as a conflict).
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{{ID: devHW1, FriendlyName: "Garden"}}}
	s := New(prov, WithConfigStore(store))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devHW1},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 409", resp)
	}
}

func TestDeleteDeviceHappyPath(t *testing.T) {
	store, path := tempStore(t)
	var reloaded bool
	reloader := func(_ context.Context, cfg config.Config) error {
		reloaded = true
		if len(cfg.Devices) != 0 {
			t.Errorf("reloader saw %d devices, want 0", len(cfg.Devices))
		}
		return nil
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(reloader))

	resp, err := s.DeleteDevice(context.Background(), mgmtapi.DeleteDeviceRequestObject{Name: devGarden})
	if err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.DeleteDevice204Response); !ok {
		t.Fatalf("returned %T, want 204", resp)
	}
	if !reloaded {
		t.Error("reloader was not invoked")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if len(loaded.Devices) != 0 {
		t.Errorf("persisted %d devices, want 0", len(loaded.Devices))
	}
}

func TestDeleteDeviceUnknownYields404(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.DeleteDevice(context.Background(), mgmtapi.DeleteDeviceRequestObject{Name: "nope"})
	if err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.DeleteDevice404ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 404", resp)
	}
	// The seeded device must be untouched (a failed delete does not persist).
	if got := store.Config(); len(got.Devices) != 1 {
		t.Errorf("config has %d devices, want 1 (unchanged)", len(got.Devices))
	}
}
