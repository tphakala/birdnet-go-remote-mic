package mgmtserver

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
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
func chanPtr(v ...int) *[]int                          { return &v }

func TestChooseParams(t *testing.T) {
	t.Parallel()
	opusCapable := &AvailableDevice{SupportedRates: []int{44100, 48000, 96000}, SupportedChannels: []int{1, 2}}
	ultrasonic := &AvailableDevice{SupportedRates: []int{256000, 384000}, SupportedChannels: []int{1}}
	stereoOnly48k := &AvailableDevice{SupportedRates: []int{48000, 96000}, SupportedChannels: []int{2}}
	unprobed := &AvailableDevice{}

	tests := []struct {
		name     string
		dev      *AvailableDevice
		req      *mgmtapi.ProvisionDeviceRequest
		wantMode config.Mode
		wantRate int
		wantCh   []int
	}{
		{"auto picks opus when 48k supported", opusCapable, &mgmtapi.ProvisionDeviceRequest{}, config.ModeOpus, 48000, []int{1}},
		{"auto picks pcm at best rate for ultrasonic", ultrasonic, &mgmtapi.ProvisionDeviceRequest{}, config.ModePCM, 384000, []int{1}},
		{"auto unprobed defaults pcm 48k mono", unprobed, &mgmtapi.ProvisionDeviceRequest{}, config.ModePCM, 48000, []int{1}},
		// Every derived selection is a single channel, even on a stereo-only
		// interface (the selecting source extracts it), so auto lands on mono Opus.
		{"auto stereo-only defaults to one channel", stereoOnly48k, &mgmtapi.ProvisionDeviceRequest{}, config.ModeOpus, 48000, []int{1}},
		// But selecting a single channel on that same stereo-only device unlocks
		// Opus: the selecting source extracts one channel to a mono stream.
		{"single-channel selection unlocks opus on stereo-only", stereoOnly48k, &mgmtapi.ProvisionDeviceRequest{Channels: chanPtr(1)}, config.ModeOpus, 48000, []int{1}},
		// An explicit non-first single channel must be honored for Opus, not
		// silently rewritten to channel 1.
		{"explicit single non-first channel preserved for opus", opusCapable, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Opus), Channels: chanPtr(2)}, config.ModeOpus, 48000, []int{2}},
		{"auto single non-first channel picks opus on that channel", stereoOnly48k, &mgmtapi.ProvisionDeviceRequest{Channels: chanPtr(2)}, config.ModeOpus, 48000, []int{2}},
		{"explicit opus forces 48k mono", ultrasonic, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Opus)}, config.ModeOpus, 48000, []int{1}},
		{"explicit pcm with rate override defaults mono", opusCapable, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm), Rate: intPtr(96000)}, config.ModePCM, 96000, []int{1}},
		{"explicit pcm derives best rate", ultrasonic, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm)}, config.ModePCM, 384000, []int{1}},
		{"channel selection respected for pcm", opusCapable, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm), Channels: chanPtr(1, 2)}, config.ModePCM, 48000, []int{1, 2}},
		// Auto mode must NOT silently return Opus and discard an explicit rate the
		// operator asked for; an explicit non-48k rate means they want PCM.
		{"auto with explicit rate falls to pcm not opus", opusCapable, &mgmtapi.ProvisionDeviceRequest{Rate: intPtr(96000)}, config.ModePCM, 96000, []int{1}},
		{"auto with multi-channel selection falls to pcm not opus", opusCapable, &mgmtapi.ProvisionDeviceRequest{Channels: chanPtr(1, 2)}, config.ModePCM, 48000, []int{1, 2}},
		// An EXPLICIT selection with explicit Opus is kept as asked, not collapsed:
		// two channels is a valid stereo Opus selection, and three or more is what
		// config.Validate rejects (422) rather than the request being narrowed here.
		{"explicit opus with explicit multi-channel is not collapsed", opusCapable, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Opus), Channels: chanPtr(1, 2)}, config.ModeOpus, 48000, []int{1, 2}},
		// A derived selection is always a single channel, so explicit Opus on a
		// stereo-only device lands on one channel.
		{"explicit opus on stereo-only derives one channel", stereoOnly48k, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Opus)}, config.ModeOpus, 48000, []int{1}},
		// An explicit EMPTY channel array is a derived selection just like an
		// omitted field, so it derives a single channel instead of 422ing where
		// omitting the field would have succeeded.
		{"explicit opus with empty channel array derives one channel", stereoOnly48k, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Opus), Channels: chanPtr()}, config.ModeOpus, 48000, []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mode, rate, ch := chooseParams(tt.dev, tt.req, 1)
			if mode != tt.wantMode || rate != tt.wantRate || !slices.Equal(ch, tt.wantCh) {
				t.Errorf("chooseParams = (%s, %d, %v), want (%s, %d, %v)", mode, rate, ch, tt.wantMode, tt.wantRate, tt.wantCh)
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
	if created.Mode != mgmtapi.Pcm || created.Rate != 384000 || !slices.Equal(created.Channels, []int{1}) {
		t.Errorf("created params = (%s, %d, %v), want (pcm, 384000, [1])", created.Mode, created.Rate, created.Channels)
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
	if got := loaded.Devices[1].Device; got != devAttic {
		t.Errorf("persisted device id = %q, want the id the host reported (%q)", got, devAttic)
	}
}

// An explicit empty channels array is a derived selection, exactly like an
// omitted field: the endpoint accepts it and provisions the appliance's default
// rather than rejecting it. This guards the contract (ProvisionDeviceRequest.
// channels carries no minItems) against a future request validator that would
// otherwise 422 the empty array the handler already supports.
func TestProvisionDeviceEmptyChannelsDerivesDefault(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{384000}, SupportedChannels: []int{1}},
	}}
	s := New(prov, WithConfigStore(store), WithReloader(func(context.Context, config.Config) error { return nil }))

	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		// &[]int{} is an explicit, non-nil empty array (the JSON "channels": []
		// case), distinct from an omitted field or a nil slice.
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic, Channels: &[]int{}},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	created, ok := resp.(mgmtapi.ProvisionDevice201JSONResponse)
	if !ok {
		t.Fatalf("returned %T, want 201", resp)
	}
	// Empty array derives the same default as omitting the field: PCM 384k mono.
	if created.Mode != mgmtapi.Pcm || created.Rate != 384000 || !slices.Equal(created.Channels, []int{1}) {
		t.Errorf("created params = (%s, %d, %v), want (pcm, 384000, [1])", created.Mode, created.Rate, created.Channels)
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

func TestProvisionDeviceInvalidOverrideYields422(t *testing.T) {
	// An explicit override that fails config.Validate (a rate above the ceiling)
	// takes the post-build validation path, not the early empty-device path.
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{{ID: devAttic, FriendlyName: nameAudioMoth}}}
	s := New(prov, WithConfigStore(store))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic, Mode: modePtr(mgmtapi.Pcm), Rate: intPtr(500000)},
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

func TestProvisionDeviceNotImplementedWithoutStore(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	d, ok := resp.(mgmtapi.ProvisionDevicedefaultApplicationProblemPlusJSONResponse)
	if !ok || d.StatusCode != http.StatusNotImplemented {
		t.Fatalf("returned %T (status %v), want 501", resp, ok)
	}
}

func TestDeleteDeviceNotImplementedWithoutStore(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.DeleteDevice(context.Background(), mgmtapi.DeleteDeviceRequestObject{Name: devGarden})
	if err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	d, ok := resp.(mgmtapi.DeleteDevicedefaultApplicationProblemPlusJSONResponse)
	if !ok || d.StatusCode != http.StatusNotImplemented {
		t.Fatalf("returned %T (status %v), want 501", resp, ok)
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

func TestProvisionDeviceOpusStereoYields201(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameScarlett, SupportedRates: []int{48000}, SupportedChannels: []int{1, 2}},
	}}
	s := New(prov, WithConfigStore(store))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic, Mode: modePtr(mgmtapi.Opus), Channels: chanPtr(1, 2)},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	created, ok := resp.(mgmtapi.ProvisionDevice201JSONResponse)
	if !ok {
		t.Fatalf("returned %T, want 201 for a two-channel (stereo) Opus selection", resp)
	}
	if created.Mode != mgmtapi.Opus || created.Rate != 48000 || !slices.Equal(created.Channels, []int{1, 2}) {
		t.Errorf("created params = (%s, %d, %v), want (opus, 48000, [1 2])", created.Mode, created.Rate, created.Channels)
	}
}

func TestProvisionDeviceOpusThreeChannelYields422(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameScarlett, SupportedRates: []int{48000}, SupportedChannels: []int{1, 2, 3, 4}},
	}}
	s := New(prov, WithConfigStore(store))
	before := len(store.Config().Devices)
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic, Mode: modePtr(mgmtapi.Opus), Channels: chanPtr(1, 2, 3)},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	got, ok := resp.(mgmtapi.ProvisionDevice422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("returned %T, want 422 for a three-channel Opus selection", resp)
	}
	if got.Errors == nil || len(*got.Errors) != 1 || !strings.HasSuffix((*got.Errors)[0].Field, ".channels") {
		t.Errorf("errors = %+v, want one entry on the channels field", got.Errors)
	}
	// A rejected provisioning must not persist a device.
	if len(store.Config().Devices) != before {
		t.Errorf("device count = %d after a rejected provision, want %d (nothing persisted)", len(store.Config().Devices), before)
	}
}

// TestRandomPathRetriesOnCollision forces the entropy source to return a taken
// path first, proving the retry loop runs and returns the next, distinct path.
// It must stay sequential (no t.Parallel): it swaps the package-level randRead.
func TestRandomPathRetriesOnCollision(t *testing.T) {
	calls := 0
	prev := randRead
	randRead = func(b []byte) (int, error) {
		calls++
		fill := byte(0)
		if calls > 1 {
			fill = 0xab
		}
		for i := range b {
			b[i] = fill
		}
		return len(b), nil
	}
	defer func() { randRead = prev }()

	taken := map[string]bool{"/0000000000000000": true}
	got := randomPath(taken)
	if got != "/abababababababab" {
		t.Errorf("randomPath = %q, want the second draw /abababababababab", got)
	}
	if calls != 2 {
		t.Errorf("entropy source called %d times, want 2 (one collision, one retry)", calls)
	}
}

func TestDeriveNameFallsBackToDevice(t *testing.T) {
	t.Parallel()
	// Neither the friendly name nor the id slugs to anything: the last-resort
	// base is "device", uniqued like any other base.
	if got := deriveName("", "---", map[string]bool{}); got != "device" {
		t.Errorf("deriveName = %q, want device", got)
	}
	if got := deriveName("", "---", map[string]bool{"device": true}); got != "device-2" {
		t.Errorf("deriveName with device taken = %q, want device-2", got)
	}
}

// TestChooseParamsPreferredChannel asserts a derived selection takes the
// preferred (loudest) channel, and an explicit selection ignores it.
func TestChooseParamsPreferredChannel(t *testing.T) {
	t.Parallel()
	dev := &AvailableDevice{SupportedRates: []int{48000}, SupportedChannels: []int{4}}
	if _, _, ch := chooseParams(dev, &mgmtapi.ProvisionDeviceRequest{}, 3); !slices.Equal(ch, []int{3}) {
		t.Errorf("derived selection = %v, want [3]", ch)
	}
	if _, _, ch := chooseParams(dev, &mgmtapi.ProvisionDeviceRequest{Channels: chanPtr(2)}, 3); !slices.Equal(ch, []int{2}) {
		t.Errorf("explicit selection = %v, want [2]", ch)
	}
}

func TestLoudestChannel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		levels []float64
		want   int
	}{
		{"empty", nil, 1},
		{"clear winner", []float64{-60, -30, -55}, 2},
		{"exact tie picks lowest", []float64{-40, -40}, 1},
		{"within tie window picks lowest", []float64{-40.6, -40}, 1},
		{"outside tie window picks loudest", []float64{-42, -40}, 2},
		{"all silent picks channel 1", []float64{-95, -85, -99}, 1},
		{"single channel", []float64{-20}, 1},
		{"one dB below is still a tie for the lowest", []float64{-41, -40}, 1},
		{"loudest at the silence floor picks channel 1", []float64{-80, -80.5}, 1},
		// A NaN channel is skipped, so it never wins and a real channel decides the
		// result. The RMS feed cannot produce NaN; these pin the explicit policy.
		{"NaN channel is skipped, the real channel wins", []float64{math.NaN(), -20}, 2},
		{"NaN on the loudest channel is skipped", []float64{-20, math.NaN()}, 1},
		{"all NaN picks channel 1", []float64{math.NaN(), math.NaN()}, 1},
	} {
		if got := loudestChannel(tc.levels); got != tc.want {
			t.Errorf("%s: loudestChannel(%v) = %d, want %d", tc.name, tc.levels, got, tc.want)
		}
	}
}

// TestProvisionDeviceDefaultsToLoudestChannel asserts provisioning without a
// channel selection probes the device across its width at the rate it will
// use, and provisions the loudest channel.
func TestProvisionDeviceDefaultsToLoudestChannel(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{2, 4}},
	}}
	var gotRate, gotWidth int
	probe := func(_ context.Context, _ string, rate, channels int) ([]float64, error) {
		gotRate, gotWidth = rate, channels
		return []float64{-70, -65, -30, -31}, nil
	}
	s := New(prov, WithConfigStore(store), WithChannelProbe(probe),
		WithReloader(func(context.Context, config.Config) error { return nil }))
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
	if !slices.Equal(created.Channels, []int{3}) {
		t.Errorf("channels = %v, want [3] (the loudest; 4 is within the tie window but higher)", created.Channels)
	}
	if gotRate != 48000 || gotWidth != 4 {
		t.Errorf("probe opened at %d Hz x %d ch, want 48000 Hz x 4 ch", gotRate, gotWidth)
	}
}

// TestProvisionDeviceProbeFallbacks asserts a failing probe falls back to
// channel 1 and an explicit selection skips the probe entirely.
func TestProvisionDeviceProbeFallbacks(t *testing.T) {
	newServer := func(t *testing.T, probe ChannelProbe) *Server {
		t.Helper()
		store, _ := tempStore(t)
		prov := &fakeProvider{available: []AvailableDevice{
			{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{2}},
		}}
		return New(prov, WithConfigStore(store), WithChannelProbe(probe),
			WithReloader(func(context.Context, config.Config) error { return nil }))
	}
	provision := func(t *testing.T, s *Server, req *mgmtapi.ProvisionDeviceRequest) []int {
		t.Helper()
		resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{Body: req})
		if err != nil {
			t.Fatalf("ProvisionDevice: %v", err)
		}
		created, ok := resp.(mgmtapi.ProvisionDevice201JSONResponse)
		if !ok {
			t.Fatalf("returned %T, want 201", resp)
		}
		return created.Channels
	}

	failing := func(context.Context, string, int, int) ([]float64, error) { return nil, errors.New("device busy") }
	if ch := provision(t, newServer(t, failing), &mgmtapi.ProvisionDeviceRequest{Device: devAttic}); !slices.Equal(ch, []int{1}) {
		t.Errorf("failed probe: channels = %v, want [1]", ch)
	}

	called := false
	spy := func(context.Context, string, int, int) ([]float64, error) {
		called = true
		return []float64{-90, -10}, nil
	}
	ch := provision(t, newServer(t, spy), &mgmtapi.ProvisionDeviceRequest{Device: devAttic, Channels: chanPtr(1)})
	if called {
		t.Error("probe ran although the request named its channels")
	}
	if !slices.Equal(ch, []int{1}) {
		t.Errorf("explicit selection: channels = %v, want [1]", ch)
	}
}

// TestProvisionDeviceAlreadyConfiguredSkipsProbe asserts an already-configured
// device is refused with a 409 before the channel probe runs, so provisioning
// never opens a device that is already serving.
func TestProvisionDeviceAlreadyConfiguredSkipsProbe(t *testing.T) {
	store, _ := tempStore(t) // baseConfig already lists devHW1
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devHW1, FriendlyName: "Garden", SupportedRates: []int{48000}, SupportedChannels: []int{2}},
	}}
	called := false
	probe := func(context.Context, string, int, int) ([]float64, error) {
		called = true
		return []float64{-90, -10}, nil
	}
	s := New(prov, WithConfigStore(store), WithChannelProbe(probe))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devHW1},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 409", resp)
	}
	if called {
		t.Error("channel probe ran for an already-configured device")
	}
}

// TestProvisionDeviceCancelledDuringProbeDoesNotPersist asserts a request whose
// context is cancelled during the probe is answered with a 503 and persists
// nothing, so a client that walked away leaves no half-provisioned device.
func TestProvisionDeviceCancelledDuringProbeDoesNotPersist(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{2}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	probe := func(context.Context, string, int, int) ([]float64, error) {
		cancel() // the client disconnects during the probe
		return []float64{-90, -10}, nil
	}
	before := len(store.Config().Devices)
	s := New(prov, WithConfigStore(store), WithChannelProbe(probe),
		WithReloader(func(context.Context, config.Config) error { return nil }))
	resp, err := s.ProvisionDevice(ctx, mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	d, ok := resp.(mgmtapi.ProvisionDevicedefaultApplicationProblemPlusJSONResponse)
	if !ok || d.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("returned %T, want a 503", resp)
	}
	if got := len(store.Config().Devices); got != before {
		t.Errorf("persisted %d devices after a cancelled request, want %d", got, before)
	}
}

// TestProvisionDeviceCancelledWhileWaitingForLockDoesNotPersist asserts a
// request cancelled while it waits for patchMu (held by another request's
// persist-and-reload) is not persisted once it gets the lock.
func TestProvisionDeviceCancelledWhileWaitingForLockDoesNotPersist(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{1}},
	}}
	s := New(prov, WithConfigStore(store), WithReloader(func(context.Context, config.Config) error { return nil }))
	before := len(store.Config().Devices)

	s.patchMu.Lock() // another request is mid persist-and-reload
	ctx, cancel := context.WithCancel(context.Background())
	type out struct {
		resp mgmtapi.ProvisionDeviceResponseObject
		err  error
	}
	done := make(chan out, 1)
	go func() {
		resp, err := s.ProvisionDevice(ctx, mgmtapi.ProvisionDeviceRequestObject{
			Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic},
		})
		done <- out{resp, err}
	}()
	cancel() // the client disconnects while the request waits for the lock
	s.patchMu.Unlock()

	r := <-done
	if r.err != nil {
		t.Fatalf("ProvisionDevice: %v", r.err)
	}
	d, ok := r.resp.(mgmtapi.ProvisionDevicedefaultApplicationProblemPlusJSONResponse)
	if !ok || d.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("returned %T, want a 503", r.resp)
	}
	if got := len(store.Config().Devices); got != before {
		t.Errorf("persisted %d devices after a cancelled request, want %d", got, before)
	}
}

// TestPreferredChannelProbeParams asserts the probe runs at the rate and width
// provisioning will actually use, and is skipped when there is nothing to choose.
func TestPreferredChannelProbeParams(t *testing.T) {
	newServer := func(probe ChannelProbe) *Server {
		store, _ := tempStore(t)
		return New(&fakeProvider{}, WithConfigStore(store), WithChannelProbe(probe))
	}

	t.Run("opus request probes at 48k not the requested rate", func(t *testing.T) {
		var gotRate int
		s := newServer(func(_ context.Context, _ string, rate, _ int) ([]float64, error) {
			gotRate = rate
			return []float64{-90, -10}, nil
		})
		d := &AvailableDevice{SupportedRates: []int{44100, 48000}, SupportedChannels: []int{2}}
		s.preferredChannel(context.Background(), d, &mgmtapi.ProvisionDeviceRequest{
			Mode: modePtr(mgmtapi.Opus), Rate: intPtr(44100),
		})
		if gotRate != 48000 {
			t.Errorf("probe rate = %d, want 48000 (the rate opus provisioning uses)", gotRate)
		}
	})

	t.Run("implausible rate skips the probe", func(t *testing.T) {
		called := false
		s := newServer(func(context.Context, string, int, int) ([]float64, error) {
			called = true
			return []float64{-90, -10}, nil
		})
		d := &AvailableDevice{SupportedRates: []int{48000}, SupportedChannels: []int{2}}
		got := s.preferredChannel(context.Background(), d, &mgmtapi.ProvisionDeviceRequest{
			Mode: modePtr(mgmtapi.Pcm), Rate: intPtr(1000),
		})
		if called || got != 1 {
			t.Errorf("probe called=%v, channel=%d; want the probe skipped and channel 1 for a 1000 Hz request", called, got)
		}
	})

	t.Run("empty channels array still probes", func(t *testing.T) {
		// Through ProvisionDevice, not preferredChannel directly: the decision to
		// probe for an explicit empty array is made by ProvisionDevice's check.
		called := false
		store, _ := tempStore(t)
		prov := &fakeProvider{available: []AvailableDevice{
			{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{2}},
		}}
		s := New(prov, WithConfigStore(store), WithReloader(func(context.Context, config.Config) error { return nil }),
			WithChannelProbe(func(context.Context, string, int, int) ([]float64, error) {
				called = true
				return []float64{-90, -10}, nil
			}))
		resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
			Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic, Channels: chanPtr()},
		})
		if err != nil {
			t.Fatalf("ProvisionDevice: %v", err)
		}
		created, ok := resp.(mgmtapi.ProvisionDevice201JSONResponse)
		if !ok {
			t.Fatalf("returned %T, want 201", resp)
		}
		if !called || !slices.Equal(created.Channels, []int{2}) {
			t.Errorf("probe called=%v, channels=%v; want the probe run and the loudest channel [2]", called, created.Channels)
		}
	})

	t.Run("steps down when the widest count fails at the rate", func(t *testing.T) {
		var tried []int
		s := newServer(func(_ context.Context, _ string, _, channels int) ([]float64, error) {
			tried = append(tried, channels)
			if channels > 2 {
				return nil, errors.New("invalid channel count at this rate")
			}
			return []float64{-90, -10}, nil
		})
		d := &AvailableDevice{SupportedRates: []int{192000}, SupportedChannels: []int{1, 2, 4, 8}}
		got := s.preferredChannel(context.Background(), d, &mgmtapi.ProvisionDeviceRequest{Mode: modePtr(mgmtapi.Pcm)})
		if got != 2 || !slices.Equal(tried, []int{8, 4, 2}) {
			t.Errorf("channel %d after widths %v; want channel 2 after trying [8 4 2]", got, tried)
		}
	})

	t.Run("single-channel device skips the probe", func(t *testing.T) {
		called := false
		s := newServer(func(context.Context, string, int, int) ([]float64, error) {
			called = true
			return nil, nil
		})
		d := &AvailableDevice{SupportedRates: []int{48000}, SupportedChannels: []int{1}}
		if got := s.preferredChannel(context.Background(), d, &mgmtapi.ProvisionDeviceRequest{}); called || got != 1 {
			t.Errorf("probe called=%v, channel=%d; want no probe and channel 1", called, got)
		}
	})

	t.Run("width caps at the config maximum", func(t *testing.T) {
		var gotWidth int
		s := newServer(func(_ context.Context, _ string, _, channels int) ([]float64, error) {
			gotWidth = channels
			return make([]float64, channels), nil
		})
		d := &AvailableDevice{SupportedRates: []int{48000}, SupportedChannels: []int{16}}
		s.preferredChannel(context.Background(), d, &mgmtapi.ProvisionDeviceRequest{})
		if gotWidth != config.MaxChannels {
			t.Errorf("probe width = %d, want %d (config maximum)", gotWidth, config.MaxChannels)
		}
	})

	t.Run("single-channel device is not probed", func(t *testing.T) {
		called := false
		s := newServer(func(context.Context, string, int, int) ([]float64, error) {
			called = true
			return []float64{-10}, nil
		})
		d := &AvailableDevice{SupportedRates: []int{48000}, SupportedChannels: []int{1}}
		if got := s.preferredChannel(context.Background(), d, &mgmtapi.ProvisionDeviceRequest{}); got != 1 {
			t.Errorf("preferredChannel = %d, want 1", got)
		}
		if called {
			t.Error("probe ran for a single-channel device")
		}
	})
}

const (
	addrHW4     = "hw:4,0"
	idStableUSB = "usb:1235:8218:s=S1:if=0,0"
)

// TestProvisionDeviceConfiguredUnderAnotherIDYields409 pins that a device the
// host lists but the available view hides (the config owns it under another id,
// such as a card index that resolves to it) is refused as already configured,
// without the disruptive probe, rather than provisioned as a second entry.
func TestProvisionDeviceConfiguredUnderAnotherIDYields409(t *testing.T) {
	store, _ := tempStore(t)
	prov := &fakeProvider{detected: []AvailableDevice{{ID: idStableUSB, HWAddr: devHW1, IDStable: true}}}
	called := false
	probe := func(context.Context, string, int, int) ([]float64, error) {
		called = true
		return nil, nil
	}
	s := New(prov, WithConfigStore(store), WithChannelProbe(probe))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: idStableUSB},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	got, ok := resp.(mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("returned %T, want 409", resp)
	}
	if got.Detail == nil || !strings.Contains(*got.Detail, "another entry") {
		detail := "<nil>"
		if got.Detail != nil {
			detail = *got.Detail
		}
		t.Errorf("409 detail = %q, want it to say the device is set up as another entry", detail)
	}
	if called {
		t.Error("channel probe ran for a device the config already owns")
	}
}

// TestProvisionDeviceRechecksAliasUnderLock pins the alias check repeated under
// patchMu: a device the config comes to own under another id while this
// request probes (a concurrent PATCH adding an alias, simulated from the probe)
// is refused with a 409 instead of being persisted as a second entry.
func TestProvisionDeviceRechecksAliasUnderLock(t *testing.T) {
	store, _ := tempStore(t)
	dev := AvailableDevice{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{2}}
	prov := &fakeProvider{available: []AvailableDevice{dev}}
	probe := func(context.Context, string, int, int) ([]float64, error) {
		// The concurrent alias lands: the device is still detected but no longer
		// available.
		prov.available, prov.detected = nil, []AvailableDevice{dev}
		return []float64{-90, -10}, nil
	}
	before := len(store.Config().Devices)
	s := New(prov, WithConfigStore(store), WithChannelProbe(probe),
		WithReloader(func(context.Context, config.Config) error { return nil }))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 409", resp)
	}
	if got := len(store.Config().Devices); got != before {
		t.Errorf("persisted %d devices, want %d (the aliased device must not be added)", got, before)
	}
}

// TestProvisionDeviceUnpluggedDuringProbeYields404 pins that a device that
// leaves the host while provisioning probes it is reported as gone (404), not
// as configured under another id (409): the available view hides both, and
// only the detected view tells them apart.
func TestProvisionDeviceUnpluggedDuringProbeYields404(t *testing.T) {
	store, _ := tempStore(t)
	dev := AvailableDevice{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{48000}, SupportedChannels: []int{2}}
	prov := &fakeProvider{available: []AvailableDevice{dev}}
	probe := func(context.Context, string, int, int) ([]float64, error) {
		prov.available, prov.detected = nil, nil // unplugged mid-probe
		return []float64{-90, -10}, nil
	}
	before := len(store.Config().Devices)
	s := New(prov, WithConfigStore(store), WithChannelProbe(probe),
		WithReloader(func(context.Context, config.Config) error { return nil }))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: devAttic},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice404ApplicationProblemPlusJSONResponse); !ok {
		t.Fatalf("returned %T, want 404", resp)
	}
	if got := len(store.Config().Devices); got != before {
		t.Errorf("persisted %d devices, want %d", got, before)
	}
}

// TestProbeWidths pins the probe's width ladder: widest first, only counts of
// two or more, capped at the config maximum and deduplicated, and the cap alone
// when the device reported no counts.
func TestProbeWidths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		supported []int
		want      []int
	}{
		{"unknown counts probe at the cap", nil, []int{config.MaxChannels}},
		{"single channel probes nothing", []int{1}, nil},
		{"widest first, mono dropped", []int{1, 2, 4, 8}, []int{8, 4, 2}},
		{"over-cap counts collapse to one cap", []int{2, 16, 32}, []int{config.MaxChannels, 2}},
		{"duplicates removed", []int{4, 2, 4}, []int{4, 2}},
	} {
		if got := probeWidths(tc.supported); !slices.Equal(got, tc.want) {
			t.Errorf("%s: probeWidths(%v) = %v, want %v", tc.name, tc.supported, got, tc.want)
		}
	}
}

// TestPreferredChannelStopsWhenBudgetSpent pins that the step-down stops once
// the probe budget (or the request) is gone, instead of trying every narrower
// width against a dead context.
func TestPreferredChannelStopsWhenBudgetSpent(t *testing.T) {
	t.Parallel()
	var tried []int
	probe := func(ctx context.Context, _ string, _, channels int) ([]float64, error) {
		tried = append(tried, channels)
		return nil, ctx.Err()
	}
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store), WithChannelProbe(probe))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := &AvailableDevice{SupportedRates: []int{48000}, SupportedChannels: []int{2, 4, 8}}
	if got := s.preferredChannel(ctx, d, &mgmtapi.ProvisionDeviceRequest{}); got != 1 || !slices.Equal(tried, []int{8}) {
		t.Errorf("channel %d after widths %v; want channel 1 after trying only [8]", got, tried)
	}
}

// TestProvisionDeviceTrimsID pins that surrounding whitespace in the requested
// id is dropped before lookup and persistence, so it cannot create an entry
// whose id differs from the host's by whitespace.
func TestProvisionDeviceTrimsID(t *testing.T) {
	store, path := tempStore(t)
	prov := &fakeProvider{available: []AvailableDevice{
		{ID: devAttic, FriendlyName: nameAudioMoth, SupportedRates: []int{384000}, SupportedChannels: []int{1}},
	}}
	s := New(prov, WithConfigStore(store))
	resp, err := s.ProvisionDevice(context.Background(), mgmtapi.ProvisionDeviceRequestObject{
		Body: &mgmtapi.ProvisionDeviceRequest{Device: " " + devAttic + "\t"},
	})
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if _, ok := resp.(mgmtapi.ProvisionDevice201JSONResponse); !ok {
		t.Fatalf("returned %T, want 201", resp)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := loaded.Devices[len(loaded.Devices)-1].Device; got != devAttic {
		t.Errorf("persisted device id = %q, want %q", got, devAttic)
	}
}

// TestAvailableDeviceCarriesIdentity pins the wire mapping of the current
// address and the stable flag for an available device.
func TestAvailableDeviceCarriesIdentity(t *testing.T) {
	got := mapAvailableDevice(&AvailableDevice{ID: idStableUSB, HWAddr: addrHW4, IDStable: true})
	if got.HwAddr == nil || *got.HwAddr != addrHW4 || got.IdStable == nil || !*got.IdStable {
		t.Errorf("mapped = %+v, want hwAddr hw:4,0 and idStable true", got)
	}
	got = mapAvailableDevice(&AvailableDevice{ID: devHW1})
	if got.HwAddr != nil || got.IdStable == nil || *got.IdStable {
		t.Errorf("mapped fallback = %+v, want no hwAddr and idStable false", got)
	}
}

// TestDeviceStatusCarriesIdentity pins the wire mapping of the resolved
// address and the stable flag for a configured device.
func TestDeviceStatusCarriesIdentity(t *testing.T) {
	d := servingOpus()
	d.HWAddr = addrHW4
	d.IDStable = true
	got := mapDevice(&d)
	if got.HwAddr == nil || *got.HwAddr != addrHW4 || got.IdStable == nil || !*got.IdStable {
		t.Errorf("mapped = %+v, want hwAddr hw:4,0 and idStable true", got)
	}
	d.HWAddr, d.IDStable = "", false
	got = mapDevice(&d)
	if got.HwAddr != nil || got.IdStable == nil || *got.IdStable {
		t.Errorf("mapped absent device = %+v, want no hwAddr and idStable false", got)
	}
}
