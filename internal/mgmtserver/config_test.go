package mgmtserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

const (
	devGarden  = "garden"
	devHW1     = "hw:1,0"
	pathGarden = "/garden"
	fmtS16     = "s16"
)

// baseConfig is a minimal valid, defaulted configuration for the config tests.
func baseConfig() config.Config {
	c := config.Config{
		Listen: rtspAddr,
		Devices: []config.Device{{
			Name: devGarden, Device: devHW1, Rate: 48000, Format: fmtS16,
			Streams: []config.Stream{{
				Path: pathGarden, Mode: config.ModeOpus, Channels: []int{1},
				Opus: config.Opus{Bitrate: 96000},
			}},
		}},
	}
	c.ApplyDefaults()
	return c
}

// tempStore returns a FileConfigStore seeded with baseConfig over a temp path.
func tempStore(t *testing.T) (store *FileConfigStore, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "config.yaml")
	c := baseConfig()
	return NewFileConfigStore(path, &c), path
}

func TestGetConfigReturnsMaterializedConfig(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	got, ok := resp.(mgmtapi.GetConfig200JSONResponse)
	if !ok {
		t.Fatalf("GetConfig returned %T, want 200", resp)
	}
	if got.Listen != ":8554" {
		t.Errorf("listen = %q, want :8554", got.Listen)
	}
	// enabled flags are absent in the config yet must materialize to true.
	if got.Discovery.Enabled == nil || !*got.Discovery.Enabled {
		t.Errorf("discovery.enabled = %v, want &true", got.Discovery.Enabled)
	}
	if got.Management.Enabled == nil || !*got.Management.Enabled {
		t.Errorf("management.enabled = %v, want &true", got.Management.Enabled)
	}
	if len(got.Devices) != 1 || got.Devices[0].Name != devGarden {
		t.Fatalf("devices = %+v, want one named garden", got.Devices)
	}
	if got.Devices[0].Opus == nil || got.Devices[0].Opus.Bitrate == nil || *got.Devices[0].Opus.Bitrate != 96000 {
		t.Errorf("opus bitrate not mapped: %+v", got.Devices[0].Opus)
	}
}

func TestPatchConfigReplacesDevicesAndPersists(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	devs := []mgmtapi.DeviceConfig{{
		Name: nameAttic, Device: devAttic, Path: pathAttic,
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 192000, Channels: []int{1},
	}}
	body := &mgmtapi.ConfigPatch{Devices: &devs}

	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if !ok200.RestartRequired {
		t.Error("restartRequired = false, want true after a persisted change")
	}
	if len(ok200.Config.Devices) != 1 || ok200.Config.Devices[0].Name != nameAttic {
		t.Fatalf("response devices = %+v, want one named attic", ok200.Config.Devices)
	}

	// The file on disk must load back to the patched config.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if len(loaded.Devices) != 1 || loaded.Devices[0].Name != nameAttic || loaded.Devices[0].Streams[0].Mode != config.ModePCM {
		t.Errorf("persisted config = %+v, want one pcm device named attic", loaded.Devices)
	}
}

// An empty (present) devices array removes every device: the operator can strip
// the appliance back to no capture devices from the UI. This guards the contract
// (ConfigPatch.devices carries no minItems, matching Config.devices) against a
// future request validator that would 422 the empty replacement the handler
// already performs.
func TestPatchConfigEmptyDevicesRemovesAll(t *testing.T) {
	// tempStore seeds baseConfig, which has one device.
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	empty := []mgmtapi.DeviceConfig{}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{
		Body: &mgmtapi.ConfigPatch{Devices: &empty},
	})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if len(ok200.Config.Devices) != 0 {
		t.Errorf("response devices = %+v, want none", ok200.Config.Devices)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if len(loaded.Devices) != 0 {
		t.Errorf("persisted devices = %+v, want none", loaded.Devices)
	}
}

func TestPatchConfigEmptyDiscoveryKeepsDisabled(t *testing.T) {
	// A PATCH carrying discovery:{} (the object present, enabled absent) must
	// leave an explicitly-disabled discovery disabled. Copying the patch's nil
	// Enabled pointer straight in resets the flag to nil, and a nil discovery
	// flag defaults ON, silently re-enabling advertisement the operator turned
	// off. This mirrors the auth branch, which no-ops on an absent token.
	disabled := false
	c := baseConfig()
	c.Discovery.Enabled = &disabled
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewFileConfigStore(path, &c)
	s := New(&fakeProvider{}, WithConfigStore(store))

	body := &mgmtapi.ConfigPatch{Discovery: &mgmtapi.DiscoverySettings{}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if ok200.Config.Discovery.Enabled == nil || *ok200.Config.Discovery.Enabled {
		t.Errorf("response discovery.enabled = %v, want &false (empty discovery patch must not re-enable)", ok200.Config.Discovery.Enabled)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if loaded.DiscoveryEnabled() {
		t.Error("persisted discovery is enabled, want disabled after an empty discovery patch")
	}
}

func TestPatchConfigWithReloaderAppliesLive(t *testing.T) {
	store, _ := tempStore(t)
	var got config.Config
	called := false
	reloader := func(_ context.Context, cfg config.Config) error {
		called = true
		got = cfg
		return nil
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(reloader))

	devs := []mgmtapi.DeviceConfig{{
		Name: nameAttic, Device: devAttic, Path: pathAttic,
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 192000, Channels: []int{1},
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if ok200.RestartRequired {
		t.Error("restartRequired = true, want false when the reloader hot-applied the change")
	}
	if !called {
		t.Fatal("reloader was not invoked")
	}
	if len(got.Devices) != 1 || got.Devices[0].Name != nameAttic {
		t.Fatalf("reloader received devices = %+v, want one named attic", got.Devices)
	}
}

func TestPatchConfigReloaderErrorReportsRestartRequired(t *testing.T) {
	store, path := tempStore(t)
	reloader := func(_ context.Context, _ config.Config) error {
		return errors.New("shutting down")
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(reloader))

	devs := []mgmtapi.DeviceConfig{{
		Name: nameAttic, Device: devAttic, Path: pathAttic,
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 192000, Channels: []int{1},
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if !ok200.RestartRequired {
		t.Error("restartRequired = false, want true when the hot reload failed")
	}
	// The change must still be persisted even though it could not be hot-applied.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if len(loaded.Devices) != 1 || loaded.Devices[0].Name != nameAttic {
		t.Errorf("persisted config = %+v, want the patch persisted despite reload failure", loaded.Devices)
	}
}

// TestPatchConfigSerializesReload proves the persist-then-reload sequence runs
// under one lock: the reloader never executes concurrently, so two racing
// patches cannot persist in one order and reconcile in the other.
func TestPatchConfigSerializesReload(t *testing.T) {
	store, _ := tempStore(t)
	var inFlight, maxInFlight atomic.Int32
	reloader := func(_ context.Context, _ config.Config) error {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // widen the window a racing patch could enter
		inFlight.Add(-1)
		return nil
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(reloader))

	const n = 6
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			devs := []mgmtapi.DeviceConfig{{
				Name: "d" + strconv.Itoa(i), Device: "hw:" + strconv.Itoa(i), Path: "/d" + strconv.Itoa(i),
				Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 48000, Channels: []int{1},
			}}
			if _, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}}); err != nil {
				t.Errorf("PatchConfig: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent reloads = %d, want 1 (persist+reload must be serialized)", got)
	}
}

func TestGetConfigMaterializesDeviceEnabled(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	got := resp.(mgmtapi.GetConfig200JSONResponse)
	// The base device has no explicit enabled flag; it must materialize to true
	// so the UI sees a concrete value.
	if got.Devices[0].Enabled == nil || !*got.Devices[0].Enabled {
		t.Errorf("device enabled = %v, want &true", got.Devices[0].Enabled)
	}
}

func TestPatchConfigPersistsDisabledDevice(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	off := false
	devs := []mgmtapi.DeviceConfig{{
		Name: devGarden, Device: devHW1, Path: pathGarden,
		Mode: mgmtapi.Opus, Format: mgmtapi.DeviceConfigFormatS16, Rate: 48000, Channels: []int{1},
		Opus:    &mgmtapi.OpusSettings{Bitrate: ptr(96000)},
		Enabled: &off,
	}}
	body := &mgmtapi.ConfigPatch{Devices: &devs}

	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200 := resp.(mgmtapi.PatchConfig200JSONResponse)
	if ok200.Config.Devices[0].Enabled == nil || *ok200.Config.Devices[0].Enabled {
		t.Errorf("response device enabled = %v, want &false", ok200.Config.Devices[0].Enabled)
	}

	// The disabled flag must survive persistence and reload.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if loaded.Devices[0].IsEnabled() {
		t.Error("persisted device is enabled, want disabled")
	}
}

func TestPatchConfigOmittedEnabledStaysDefaultOn(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	// A device patched with no enabled field must persist as enabled (default on),
	// not be silently disabled. This guards the common PATCH path.
	devs := []mgmtapi.DeviceConfig{{
		Name: nameAttic, Device: devAttic, Path: pathAttic,
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 192000, Channels: []int{1},
	}}
	if _, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}}); err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if !loaded.Devices[0].IsEnabled() {
		t.Error("device with omitted enabled persisted as disabled, want default-on")
	}
}

func TestWireDeviceToConfigDoesNotAliasEnabled(t *testing.T) {
	// Isolate the wireDeviceToConfig mapping directly: mutating the request's
	// Enabled pointer after mapping must not change the mapped config, because the
	// flag is copied into fresh storage rather than aliased. (An end-to-end PATCH
	// test cannot prove this, since FileConfigStore.Update clones and would
	// de-alias regardless.)
	flag := false
	req := &mgmtapi.DeviceConfig{
		Name: devGarden, Device: devHW1, Path: pathGarden,
		Mode: mgmtapi.Opus, Format: mgmtapi.DeviceConfigFormatS16, Rate: 48000, Channels: []int{1},
		Enabled: &flag,
	}
	out := wireDeviceToConfig(req)
	*req.Enabled = true
	if out.Enabled == nil || *out.Enabled {
		t.Errorf("wireDeviceToConfig aliased Enabled: out.Enabled=%v after mutating the request", out.Enabled)
	}
}

func TestGetConfigMaterializesNotifications(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	got, ok := resp.(mgmtapi.GetConfig200JSONResponse)
	if !ok {
		t.Fatalf("GetConfig returned %T, want 200", resp)
	}
	n := got.Notifications
	// enabled is absent in the config yet must materialize to true.
	if n.Enabled == nil || !*n.Enabled {
		t.Errorf("notifications.enabled = %v, want &true", n.Enabled)
	}
	if n.Audio == nil || n.Audio.QuietDbfs == nil || *n.Audio.QuietDbfs != -60 {
		t.Errorf("audio.quietDbfs not materialized: %+v", n.Audio)
	}
	if n.Host == nil || n.Host.CpuPercent == nil || *n.Host.CpuPercent != 90 {
		t.Errorf("host.cpuPercent not materialized: %+v", n.Host)
	}
	if n.Host.MemFreeMiB == nil || *n.Host.MemFreeMiB != 64 {
		t.Errorf("host.memFreeMiB = %v, want &64", n.Host.MemFreeMiB)
	}
	// The per-device quiet-alert flag materializes to true as well.
	if got.Devices[0].QuietAlert == nil || !*got.Devices[0].QuietAlert {
		t.Errorf("device quietAlert = %v, want &true", got.Devices[0].QuietAlert)
	}
}

func TestPatchConfigNotificationsPartialMerge(t *testing.T) {
	store, path := tempStore(t)
	reloaded := false
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(func(context.Context, config.Config) error {
		reloaded = true
		return nil
	}))

	// A partial block touches only host.cpuPercent; audio and every other host
	// field stay at their defaults, and no device is affected.
	body := &mgmtapi.ConfigPatch{Notifications: &mgmtapi.NotificationSettings{
		Host: &mgmtapi.HostAlertSettings{CpuPercent: ptr(95)},
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if !reloaded {
		t.Error("reloader was not invoked for a notifications change")
	}
	if ok200.RestartRequired {
		t.Error("restartRequired = true, want false (a notifications change hot-applies)")
	}
	h := ok200.Config.Notifications.Host
	if h == nil || h.CpuPercent == nil || *h.CpuPercent != 95 {
		t.Fatalf("cpuPercent = %+v, want &95", h)
	}
	if h.CpuClearPercent == nil || *h.CpuClearPercent != 75 {
		t.Errorf("cpuClearPercent = %v, want &75 (unchanged by the partial merge)", h.CpuClearPercent)
	}
	if h.DiskPercent == nil || *h.DiskPercent != 90 {
		t.Errorf("diskPercent = %v, want &90 (unchanged)", h.DiskPercent)
	}
	// The whole audio block must survive a host-only patch at its defaults.
	if a := ok200.Config.Notifications.Audio; a == nil || a.QuietDbfs == nil || *a.QuietDbfs != -60 {
		t.Errorf("audio block changed by a host-only patch: %+v", a)
	}
	if len(ok200.Config.Devices) != 1 || ok200.Config.Devices[0].Name != devGarden {
		t.Errorf("devices changed by a notifications-only patch: %+v", ok200.Config.Devices)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if loaded.Notifications.Host.CPUPercent == nil || *loaded.Notifications.Host.CPUPercent != 95 ||
		loaded.Notifications.Host.CPUClearPercent == nil || *loaded.Notifications.Host.CPUClearPercent != 75 {
		t.Errorf("persisted host = %+v, want cpu 95 / cpuClear 75", loaded.Notifications.Host)
	}
}

func TestPatchConfigNotificationsMergesEveryField(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	on := true
	body := &mgmtapi.ConfigPatch{Notifications: &mgmtapi.NotificationSettings{
		Enabled: &on,
		Audio: &mgmtapi.AudioAlertSettings{
			QuietDbfs:         ptr(-50),
			QuietSeconds:      ptr(1200),
			ZeroSeconds:       ptr(45),
			ClipPercent:       ptr(30),
			ClipWindowSeconds: ptr(15),
		},
		Host: &mgmtapi.HostAlertSettings{
			CpuPercent:       ptr(85),
			CpuClearPercent:  ptr(70),
			TempCelsius:      ptr(75),
			TempClearCelsius: ptr(70),
			DiskPercent:      ptr(88),
			DiskClearPercent: ptr(80),
			MemFreePercent:   ptr(15),
			MemFreeMiB:       ptr(128),
		},
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}

	// Pin every wire-output threshold against the distinctive inputs, not just the
	// persisted config: the monitors read the config, but the web UI reads this
	// wire response, so a wrong-source bug in notificationsToWire would ship the
	// wrong value to the UI while the persisted-config assertions below still pass.
	eq := func(name string, got *int, want int) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("wire %s = %v, want %d", name, got, want)
		}
	}
	if wa := ok200.Config.Notifications.Audio; wa == nil {
		t.Error("wire audio block is nil")
	} else {
		eq("audio.quietDbfs", wa.QuietDbfs, -50)
		eq("audio.quietSeconds", wa.QuietSeconds, 1200)
		eq("audio.zeroSeconds", wa.ZeroSeconds, 45)
		eq("audio.clipPercent", wa.ClipPercent, 30)
		eq("audio.clipWindowSeconds", wa.ClipWindowSeconds, 15)
	}
	if wh := ok200.Config.Notifications.Host; wh == nil {
		t.Error("wire host block is nil")
	} else {
		eq("host.cpuPercent", wh.CpuPercent, 85)
		eq("host.cpuClearPercent", wh.CpuClearPercent, 70)
		eq("host.tempCelsius", wh.TempCelsius, 75)
		eq("host.tempClearCelsius", wh.TempClearCelsius, 70)
		eq("host.diskPercent", wh.DiskPercent, 88)
		eq("host.diskClearPercent", wh.DiskClearPercent, 80)
		eq("host.memFreePercent", wh.MemFreePercent, 15)
		eq("host.memFreeMiB", wh.MemFreeMiB, 128)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	la := loaded.Notifications.Audio
	eq("persisted audio.quietDbfs", la.QuietDbfs, -50)
	eq("persisted audio.quietSeconds", la.QuietSeconds, 1200)
	eq("persisted audio.zeroSeconds", la.ZeroSeconds, 45)
	eq("persisted audio.clipPercent", la.ClipPercent, 30)
	eq("persisted audio.clipWindowSeconds", la.ClipWindowSeconds, 15)
	lh := loaded.Notifications.Host
	eq("persisted host.cpuPercent", lh.CPUPercent, 85)
	eq("persisted host.cpuClearPercent", lh.CPUClearPercent, 70)
	eq("persisted host.tempCelsius", lh.TempCelsius, 75)
	eq("persisted host.tempClearCelsius", lh.TempClearCelsius, 70)
	eq("persisted host.diskPercent", lh.DiskPercent, 88)
	eq("persisted host.diskClearPercent", lh.DiskClearPercent, 80)
	eq("persisted host.memFreePercent", lh.MemFreePercent, 15)
	eq("persisted host.memFreeMiB", lh.MemFreeMiB, 128)
}

func TestPatchConfigNotificationsClearNotBelowOnsetYields422(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	body := &mgmtapi.ConfigPatch{Notifications: &mgmtapi.NotificationSettings{
		Host: &mgmtapi.HostAlertSettings{CpuPercent: ptr(50), CpuClearPercent: ptr(60)},
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 422 validation problem", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 {
		t.Fatalf("errors = %+v, want one entry", vp.Errors)
	}
	if (*vp.Errors)[0].Field != "notifications.host.cpu_clear_percent" {
		t.Errorf("error field = %q, want notifications.host.cpu_clear_percent", (*vp.Errors)[0].Field)
	}
}

func TestPatchConfigRejectsExplicitZeroThreshold(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	// An explicitly supplied out-of-range 0 must be rejected with a 422, not
	// silently rewritten to the default by ApplyDefaults. cpuPercent's range is
	// 1..100, so a present 0 is invalid and its presence must survive the merge
	// and ApplyDefaults to reach Validate.
	body := &mgmtapi.ConfigPatch{Notifications: &mgmtapi.NotificationSettings{
		Host: &mgmtapi.HostAlertSettings{CpuPercent: ptr(0)},
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 422 for an explicit zero threshold", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != "notifications.host.cpu_percent" {
		t.Errorf("errors = %+v, want one for notifications.host.cpu_percent", vp.Errors)
	}
	// A rejected patch must not be persisted: the file must still not exist.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("a rejected patch wrote the config file (stat err = %v); want it absent", statErr)
	}
}

func TestPatchConfigNotificationsEnabledToggle(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	off := false
	body := &mgmtapi.ConfigPatch{Notifications: &mgmtapi.NotificationSettings{Enabled: &off}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if ok200.Config.Notifications.Enabled == nil || *ok200.Config.Notifications.Enabled {
		t.Errorf("response notifications.enabled = %v, want &false", ok200.Config.Notifications.Enabled)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if loaded.NotificationsEnabled() {
		t.Error("persisted notifications still enabled, want disabled after the toggle")
	}
}

func TestPatchConfigQuietAlertPersists(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	off := false
	devs := []mgmtapi.DeviceConfig{{
		Name: devGarden, Device: devHW1, Path: pathGarden,
		Mode: mgmtapi.Opus, Format: mgmtapi.DeviceConfigFormatS16, Rate: 48000, Channels: []int{1},
		Opus:       &mgmtapi.OpusSettings{Bitrate: ptr(96000)},
		QuietAlert: &off,
	}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if ok200.Config.Devices[0].QuietAlert == nil || *ok200.Config.Devices[0].QuietAlert {
		t.Errorf("response device quietAlert = %v, want &false", ok200.Config.Devices[0].QuietAlert)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if loaded.Devices[0].QuietAlertEnabled() {
		t.Error("persisted device quietAlert is on, want off")
	}
}

func TestWireDeviceToConfigDoesNotAliasQuietAlert(t *testing.T) {
	// Parity with the Enabled alias test: mutating the request's QuietAlert
	// pointer after mapping must not change the mapped config.
	flag := false
	req := &mgmtapi.DeviceConfig{
		Name: devGarden, Device: devHW1, Path: pathGarden,
		Mode: mgmtapi.Opus, Format: mgmtapi.DeviceConfigFormatS16, Rate: 48000, Channels: []int{1},
		QuietAlert: &flag,
	}
	out := wireDeviceToConfig(req)
	*req.QuietAlert = true
	if out.QuietAlert == nil || *out.QuietAlert {
		t.Errorf("wireDeviceToConfig aliased QuietAlert: out.QuietAlert=%v after mutating the request", out.QuietAlert)
	}
}

func TestGetConfigMaterializesDisabledDevice(t *testing.T) {
	off := false
	c := baseConfig()
	c.Devices[0].Enabled = &off
	store := NewFileConfigStore(filepath.Join(t.TempDir(), "config.yaml"), &c)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	got := resp.(mgmtapi.GetConfig200JSONResponse)
	if got.Devices[0].Enabled == nil || *got.Devices[0].Enabled {
		t.Errorf("disabled device enabled = %v, want &false", got.Devices[0].Enabled)
	}
}

func TestPatchConfigDiscoveryOnlyKeepsDevices(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	off := false
	body := &mgmtapi.ConfigPatch{Discovery: &mgmtapi.DiscoverySettings{Enabled: &off}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if ok200.Config.Discovery.Enabled == nil || *ok200.Config.Discovery.Enabled {
		t.Errorf("discovery.enabled = %v, want &false", ok200.Config.Discovery.Enabled)
	}
	if len(ok200.Config.Devices) != 1 || ok200.Config.Devices[0].Name != devGarden {
		t.Errorf("devices changed by a discovery-only patch: %+v", ok200.Config.Devices)
	}
}

func TestPatchConfigEmptyIsNoop(t *testing.T) {
	store, path := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	ok200, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 200", resp)
	}
	if ok200.RestartRequired {
		t.Error("restartRequired = true for an empty patch, want false")
	}
	// An empty patch must not write the file at all: it must still not exist.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty patch wrote the config file (stat err = %v); want it absent", err)
	}
}

func TestPatchConfigInvalidYields422(t *testing.T) {
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))

	devs := []mgmtapi.DeviceConfig{{
		Name: devGarden, Device: devHW1, Path: pathGarden,
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 100, Channels: []int{1}, // rate too low
	}}
	body := &mgmtapi.ConfigPatch{Devices: &devs}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want 422 validation problem", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 {
		t.Fatalf("errors = %+v, want one entry", vp.Errors)
	}
	if (*vp.Errors)[0].Field != "devices[0].rate" {
		t.Errorf("error field = %q, want devices[0].rate", (*vp.Errors)[0].Field)
	}
}

// failingStore validates via mutate but always fails to persist.
type failingStore struct{ cfg config.Config }

func (f *failingStore) Config() config.Config { return f.cfg.Clone() }
func (f *failingStore) Update(mutate func(config.Config) (config.Config, error)) error {
	if _, err := mutate(f.cfg.Clone()); err != nil {
		return err
	}
	return errors.New("disk full")
}

func TestPatchConfigPersistErrorYields500(t *testing.T) {
	s := New(&fakeProvider{}, WithConfigStore(&failingStore{cfg: baseConfig()}))
	off := false
	body := &mgmtapi.ConfigPatch{Discovery: &mgmtapi.DiscoverySettings{Enabled: &off}}
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{Body: body})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	p, ok := resp.(mgmtapi.PatchConfigdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want default problem", resp)
	}
	if p.StatusCode != 500 {
		t.Errorf("status = %d, want 500", p.StatusCode)
	}
}

func TestFileConfigStoreUpdateIsSerialized(t *testing.T) {
	store, _ := tempStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = store.Update(func(c config.Config) (config.Config, error) {
				c.ApplyDefaults()
				verr := c.Validate()
				return c, verr
			})
		}()
	}
	wg.Wait()
	// A concurrent GET must never observe a torn config.
	if got := store.Config(); len(got.Devices) != 1 {
		t.Errorf("devices = %d after concurrent updates, want 1", len(got.Devices))
	}
}

// TestPatchedDevicesMatchesByNameFirst pins the collapse guard's match order:
// when two flat entries swap their device ids, each is checked against its own
// existing entry (by name), so the rejection names the multi-stream device at
// its own index instead of the entry that took over its id.
func TestPatchedDevicesMatchesByNameFirst(t *testing.T) {
	cur := []config.Device{
		{Name: "multi", Device: "usb:a", Streams: []config.Stream{{Path: "/m1"}, {Path: "/m2"}}},
		{Name: "single", Device: "usb:b", Streams: []config.Stream{{Path: "/s"}}},
	}
	wire := []mgmtapi.DeviceConfig{
		{Name: "multi", Device: "usb:b", Path: "/m1", Mode: mgmtapi.Pcm, Rate: 48000, Channels: []int{1}},
		{Name: "single", Device: "usb:a", Path: "/s", Mode: mgmtapi.Pcm, Rate: 48000, Channels: []int{1}},
	}
	_, err := patchedDevices(cur, wire)
	var verr *config.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("patchedDevices error = %v, want a collapse ValidationError", err)
	}
	if verr.Field != "devices[0].streams" || !strings.Contains(verr.Reason, `"multi"`) {
		t.Errorf("collapse error = %s: %s, want devices[0].streams naming multi", verr.Field, verr.Reason)
	}
}
