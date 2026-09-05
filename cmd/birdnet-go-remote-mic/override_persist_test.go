//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// TestSplitServeConfig guards the issue #29 origin: splitServeConfig must land
// the serve overrides on the running config but never on the store-seed config,
// and the two must be independent. Reverting the snapshot to AFTER
// applyServeOverrides makes store carry the override and fails here.
func TestSplitServeConfig(t *testing.T) {
	loaded := config.Default()
	loaded.Listen = testRTSP8554
	running, store := splitServeConfig(&loaded, serveOverrides{
		listen:    listenAddr9,
		discovery: false,
		set:       map[string]bool{keyListen: true, keyDiscovery: true},
	})
	if running.Listen != listenAddr9 {
		t.Errorf("running.Listen = %q, want the override %q", running.Listen, listenAddr9)
	}
	if store.Listen != testRTSP8554 {
		t.Errorf("store.Listen = %q, want the pre-override %q", store.Listen, testRTSP8554)
	}
	if running.DiscoveryEnabled() {
		t.Error("running should carry the --discovery=false override")
	}
	if !store.DiscoveryEnabled() {
		t.Error("store must stay override-free (discovery default-on)")
	}
}

// TestServeOverridesNotPersistedToStore guards the store-seed contract for issue
// #29: with the store seeded from splitServeConfig's override-free config (the
// production path run() takes), a later PATCH that saves the whole config keeps
// the override flags off disk. TestStartManagementSeedsStoreFromPreOverrideConfig
// covers the same end to end through startManagement over the wire.
func TestServeOverridesNotPersistedToStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	base := config.Default()
	base.Listen = testRTSP8554
	base.Management.Listen = ":8443"
	if err := config.Save(path, &base); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.LoadOrDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the store exactly as run() does, through the production helper.
	_, storeCfg := splitServeConfig(&loaded, serveOverrides{
		listen:     listenAddr9,
		mgmtListen: mgmtAddr7,
		set:        map[string]bool{keyListen: true, keyMgmtListen: true},
	})

	store := mgmtserver.NewFileConfigStore(path, &storeCfg)
	// A PATCH that enables a device triggers a full config.Save of the store.
	if uerr := store.Update(func(c config.Config) (config.Config, error) {
		c.Devices = []config.Device{{
			Name: "garden", Device: devHW1, Path: "/garden",
			Mode: config.ModePCM, Rate: 48000, Channels: []int{1}, Format: testFmtS16,
		}}
		c.ApplyDefaults()
		return c, nil
	}); uerr != nil {
		t.Fatalf("store update: %v", uerr)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Listen != testRTSP8554 {
		t.Errorf("listen override leaked to disk: got %q, want :8554", saved.Listen)
	}
	if saved.Management.Listen != ":8443" {
		t.Errorf("mgmt-listen override leaked to disk: got %q, want :8443", saved.Management.Listen)
	}
	if len(saved.Devices) != 1 {
		t.Errorf("PATCH device did not persist: %+v", saved.Devices)
	}
}

// TestServeReloaderReappliesOverrides asserts a hot reload drives the LIVE
// pipeline with the serve overrides re-applied (issue #29): the store hands back
// an override-free config, but mDNS must keep advertising the overridden RTSP
// port and a --discovery=false override must survive the reload.
func TestServeReloaderReappliesOverrides(t *testing.T) {
	reconcileCh := make(chan reconcileReq)
	ov := serveOverrides{
		listen:    listenAddr9,
		discovery: false,
		set:       map[string]bool{keyListen: true, keyDiscovery: true},
	}
	reloader := newServeReloader(context.Background(), reconcileCh, ov)

	got := make(chan config.Config, 1)
	go func() {
		req := <-reconcileCh
		got <- req.cfg
		req.reply <- nil
	}()

	fileCfg := config.Default() // Listen testRTSP8554, discovery default-on
	if err := reloader(context.Background(), fileCfg); err != nil {
		t.Fatalf("reloader: %v", err)
	}
	live := <-got
	if live.Listen != listenAddr9 {
		t.Errorf("live reload Listen = %q, want the override %q", live.Listen, listenAddr9)
	}
	if live.DiscoveryEnabled() {
		t.Error("live reload dropped the --discovery=false override")
	}
}

// TestStartManagementSeedsStoreFromPreOverrideConfig is the end-to-end guard on
// the wiring: startManagement must seed the persistence store from storeCfg (the
// on-disk config), not the override-carrying running cfg, so a PATCH /config over
// the wire never writes the override to disk (issue #29).
func TestStartManagementSeedsStoreFromPreOverrideConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	fileCfg := config.Default()
	fileCfg.Listen = testRTSP8554
	if err := config.Save(path, &fileCfg); err != nil {
		t.Fatal(err)
	}

	running := fileCfg.Clone()
	running.Listen = listenAddr9 // an active --listen override
	running.Management.Listen = testListenAny
	running.Management.CertDir = dir
	storeCfg := fileCfg.Clone()

	ctx, cancel := context.WithCancel(context.Background())
	h, ok := startManagement(ctx, path, &running, &storeCfg, newProvider(), nil, nil, nil, auth.NewGuard(""))
	if !ok {
		t.Fatal("management did not start")
	}
	defer h.Wait()
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	body := `{"devices":[{"name":"garden","device":"hw:1,0","path":"/garden","mode":"pcm","rate":48000,"channels":[1],"format":"s16"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "https://"+h.addr+mgmtserver.BasePath+"/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH /config: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /config status = %d, want 200", resp.StatusCode)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Listen != testRTSP8554 {
		t.Errorf("override %q leaked to disk via PATCH: saved listen = %q, want :8554", listenAddr9, saved.Listen)
	}
	if len(saved.Devices) != 1 {
		t.Errorf("PATCH device not persisted: %+v", saved.Devices)
	}
}
