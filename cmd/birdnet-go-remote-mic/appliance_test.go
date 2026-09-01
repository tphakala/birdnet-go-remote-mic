//go:build linux

package main

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
)

const (
	testFmtS16    = "s16"
	testListenAny = "127.0.0.1:0"
)

// blockingSource is a fake audio.Source whose Read blocks until Close, so a
// capture pump built on it stays alive until the appliance deliberately stops
// it. That makes reconcile lifecycle tests deterministic: a pump ends exactly
// when the test (or a reconcile) closes its source, never on its own.
type blockingSource struct {
	rate, channels int
	closed         chan struct{}
	once           sync.Once
}

func newBlockingSource(rate, channels int) *blockingSource {
	return &blockingSource{rate: rate, channels: channels, closed: make(chan struct{})}
}

func (b *blockingSource) Negotiated() (rate, channels int) { return b.rate, b.channels }

func (b *blockingSource) Read() (audio.Period, error) {
	<-b.closed
	return audio.Period{}, io.EOF
}

func (b *blockingSource) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// fakeOpenLog records the open and close events an appliance drives, so a test
// can assert their ordering (a hardware-swap restart must close both devices
// before it reopens either).
type fakeOpenLog struct {
	mu     sync.Mutex
	events []string
}

func (l *fakeOpenLog) add(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}

func (l *fakeOpenLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// fakeOpener returns an appliance.open replacement that builds a real
// deviceRuntime (real pipeline stage, ChanSource and Track) around a blocking
// fake source, logging each open and each source close through log.
func fakeOpener(log *fakeOpenLog) func(*config.Device, *levels.Hub) (*deviceRuntime, error) {
	return func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
		log.add("open:" + dev.Name)
		streamCh := len(dev.Channels)
		src := newBlockingSource(dev.Rate, streamCh)
		frames := rtspserver.NewChanSource(64)
		return &deviceRuntime{
			dev:      *dev,
			src:      audio.NewMeteredSource(loggingClose{src, dev.Name, log}, hub.Meter(dev.Name, streamCh)),
			stage:    pipeline.NewPCM(streamCh),
			frames:   frames,
			track:    &rtspserver.Track{Path: dev.Path, PayloadType: 96, Frames: frames},
			rate:     dev.Rate,
			channels: streamCh,
		}, nil
	}
}

// loggingClose wraps a source to record its Close in the shared log.
type loggingClose struct {
	inner audio.Source
	name  string
	log   *fakeOpenLog
}

func (c loggingClose) Negotiated() (rate, channels int) { return c.inner.Negotiated() }
func (c loggingClose) Read() (audio.Period, error)      { return c.inner.Read() }
func (c loggingClose) Close() error {
	c.log.add("close:" + c.name)
	return c.inner.Close()
}

func testDevice(name, hw, path string, rate int) config.Device {
	return config.Device{Name: name, Device: hw, Path: path, Mode: config.ModePCM, Rate: rate, Channels: []int{1}, Format: testFmtS16}
}

func newTestAppliance(t *testing.T) (*appliance, *fakeOpenLog, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := &fakeOpenLog{}
	app := newAppliance(ctx, levels.NewHub(), rtspserver.New(rtspserver.Config{Listen: testListenAny}), &provider{version: "test", start: time.Now()}, auth.NewGuard(""))
	app.open = fakeOpener(log)
	return app, log, cancel
}

// drainPump waits for one pump to report and processes it, so a deliberately
// stopped device's teardown completes deterministically before the test asserts.
func drainPump(t *testing.T, app *appliance) {
	t.Helper()
	select {
	case res := <-app.pumpDone:
		app.onPumpDone(res)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a pump to report done")
	}
}

func TestApplianceReconcileStartsDevice(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})

	if app.serving() != 1 || app.alive != 1 {
		t.Fatalf("after start: serving=%d alive=%d, want 1/1", app.serving(), app.alive)
	}
	if !app.srv.HasTrack("/a") {
		t.Fatal("start did not register the RTSP track")
	}
}

func TestApplianceReconcileRestartKeepsNewTrackDespiteStalePumpDone(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})
	oldRT := app.devices["a"]

	// A rate change restarts the device on the same path: the old device is
	// stopped (superseded) and a new one started at /a.
	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 96000)}})
	newRT := app.devices["a"]
	if newRT == oldRT || !app.srv.HasTrack("/a") {
		t.Fatal("restart did not install a new track at /a")
	}

	// The old pump now reports done. The superseded guard must keep it from
	// tearing down the freshly installed track on the same path.
	drainPump(t, app)

	if !app.srv.HasTrack("/a") {
		t.Fatal("a stale superseded pumpDone tore down the restarted track")
	}
	if app.serving() != 1 || app.alive != 1 {
		t.Fatalf("after restart+drain: serving=%d alive=%d, want 1/1", app.serving(), app.alive)
	}
}

func TestApplianceReconcileDisableStopsDevice(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()

	app.reconcile(&config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}})

	off := false
	disabled := testDevice("a", "hw:0", "/a", 48000)
	disabled.Enabled = &off
	app.reconcile(&config.Config{Devices: []config.Device{disabled}})
	drainPump(t, app)

	if app.serving() != 0 || app.alive != 0 {
		t.Fatalf("after disable: serving=%d alive=%d, want 0/0", app.serving(), app.alive)
	}
	if app.srv.HasTrack("/a") {
		t.Fatal("disable did not retire the RTSP track")
	}
	rt, ok := app.devices["a"]
	if !ok || rt.currentState() != mgmtserver.StateDisabled {
		t.Fatalf("disabled device record = %+v (ok=%v), want a disabled record", rt, ok)
	}
}

func TestApplianceRestartStopsAllBeforeStartingAny(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()

	// Two devices on two cards.
	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("a", "hw:0", "/a", 48000),
		testDevice("b", "hw:1", "/b", 48000),
	}})

	// Swap their hardware cards: both restart. If the reconcile interleaved
	// stop+start per device, opening a's new card (hw:1) would collide with b,
	// which still holds it. Assert every close happens before every open.
	log.mu.Lock()
	log.events = nil // ignore the initial opens
	log.mu.Unlock()

	app.reconcile(&config.Config{Devices: []config.Device{
		testDevice("a", "hw:1", "/a", 48000),
		testDevice("b", "hw:0", "/b", 48000),
	}})

	events := log.snapshot()
	lastClose, firstOpen := -1, len(events)
	for i, e := range events {
		switch {
		case len(e) >= 6 && e[:6] == "close:":
			if i > lastClose {
				lastClose = i
			}
		case len(e) >= 5 && e[:5] == "open:":
			if i < firstOpen {
				firstOpen = i
			}
		}
	}
	if lastClose == -1 || firstOpen == len(events) || lastClose >= firstOpen {
		t.Fatalf("restart did not stop all before starting any: events=%v", events)
	}

	drainPump(t, app) // the two superseded old pumps
	drainPump(t, app)
}

func TestRememberCapsRetainsLastKnown(t *testing.T) {
	a := &appliance{capsCache: map[string]deviceCaps{}}

	// First probe records the caps.
	r, c := a.rememberCaps("hw:1,0", []int{48000, 96000}, []int{1, 2})
	if len(r) != 2 || len(c) != 2 {
		t.Fatalf("first probe = %v %v, want the probed values", r, c)
	}

	// A transient empty probe (card-swap window) keeps the last-known caps.
	r, c = a.rememberCaps("hw:1,0", nil, nil)
	if len(r) != 2 || r[0] != 48000 || len(c) != 2 {
		t.Errorf("empty re-probe = %v %v, want the retained [48000 96000] [1 2]", r, c)
	}

	// A later non-empty probe replaces the cache.
	r, c = a.rememberCaps("hw:1,0", []int{384000}, []int{1})
	if len(r) != 1 || r[0] != 384000 || len(c) != 1 || c[0] != 1 {
		t.Errorf("updated probe = %v %v, want [384000] [1]", r, c)
	}

	// An unknown device with an empty probe stays empty (nothing to retain).
	if r, c := a.rememberCaps("hw:9,0", nil, nil); r != nil || c != nil {
		t.Errorf("unknown empty probe = %v %v, want nil nil", r, c)
	}
}

const testAuthToken = "k7Qm3vX9pL2wR8nT"

func TestApplianceReconcileAppliesAuthToken(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	cfg := config.Config{Listen: testListenAny}
	cfg.Auth.Token = testAuthToken
	app.reconcile(&cfg)
	if !app.guard.Enabled() {
		t.Error("reconcile with a token must enable the guard")
	}
	if !app.prov.Status().AuthRequired {
		t.Error("reconcile with a token must report authRequired")
	}
	cfg.Auth.Token = ""
	app.reconcile(&cfg)
	if app.guard.Enabled() {
		t.Error("reconcile with an empty token must disable the guard")
	}
	if app.prov.Status().AuthRequired {
		t.Error("reconcile with an empty token must report open access")
	}
}

func TestApplianceAuthToggleRebuildsAnnouncement(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	cfg := config.Config{Listen: testListenAny, Devices: []config.Device{testDevice("garden", "hw:1,0", "/garden", 48000)}}
	app.reconcile(&cfg)
	defer app.closeAll()
	before := app.announceGen
	// Same devices, same discovery flag: only the token changes. The TXT auth
	// hint must follow it, so the advertisement is rebuilt.
	cfg.Auth.Token = testAuthToken
	app.reconcile(&cfg)
	if app.announceGen == before {
		t.Error("enabling auth must rebuild the mDNS advertisement (TXT auth hint)")
	}
	before = app.announceGen
	// An unrelated reconcile with nothing changed must not rebuild.
	app.reconcile(&cfg)
	if app.announceGen != before {
		t.Error("a no-op reconcile must not rebuild the advertisement")
	}
}
