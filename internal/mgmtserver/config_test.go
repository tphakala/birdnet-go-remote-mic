package mgmtserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
		Listen: ":8554",
		Devices: []config.Device{{
			Name: devGarden, Device: devHW1, Path: pathGarden,
			Mode: config.ModeOpus, Rate: 48000, Channels: 1, Format: fmtS16,
			Opus: config.Opus{Bitrate: 96000},
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
		Name: nameAttic, Device: "hw:2,0", Path: "/attic",
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 192000, Channels: 1,
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
	if len(loaded.Devices) != 1 || loaded.Devices[0].Name != nameAttic || loaded.Devices[0].Mode != config.ModePCM {
		t.Errorf("persisted config = %+v, want one pcm device named attic", loaded.Devices)
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
		Mode: mgmtapi.Pcm, Format: mgmtapi.DeviceConfigFormatS16, Rate: 100, Channels: 1, // rate too low
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
