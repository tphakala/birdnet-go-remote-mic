//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
)

const (
	testFmtS16    = "s16"
	testListenAny = "127.0.0.1:0"
	testRTSP8554  = ":8554"
	keyListen     = "listen"
	keyMgmtListen = "mgmt-listen"
	keyDiscovery  = "discovery"
	keyMgmt       = "management"
	nameScarlett  = "Scarlett"
	testCfgFile   = "config.yaml"
)

// blockingSource is a fake audio.Source whose Read blocks until Close, so a
// capture pump built on it stays alive until the appliance deliberately stops
// it. That makes reconcile lifecycle tests deterministic: a pump ends exactly
// when the test (or a reconcile) closes its source, or when the test fails it
// on demand through kill (see killDevice), never on its own.
type blockingSource struct {
	rate, channels int
	closed         chan struct{}
	kill           chan error
	once           sync.Once
	// closedErr is what Read returns once closed; nil means io.EOF. The real
	// capture returns capture.ErrClosed, which tests of the pump's error
	// selection must use.
	closedErr error
}

func newBlockingSource(rate, channels int) *blockingSource {
	return &blockingSource{rate: rate, channels: channels, closed: make(chan struct{}), kill: make(chan error, 1)}
}

func (b *blockingSource) Negotiated() (rate, channels int) { return b.rate, b.channels }

func (b *blockingSource) Read() (audio.Period, error) {
	select {
	case <-b.closed:
		if b.closedErr != nil {
			return audio.Period{}, b.closedErr
		}
		return audio.Period{}, io.EOF
	case err := <-b.kill:
		return audio.Period{}, err
	}
}

func (b *blockingSource) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// fakeOpenLog records the open and close events an appliance drives, so a test
// can assert their ordering (a hardware-swap restart must close both devices
// before it reopens either). It also keeps each device's most recently opened
// blocking source, so killDevice can fail that device's capture.
type fakeOpenLog struct {
	mu      sync.Mutex
	events  []string
	sources map[string]*blockingSource
}

func (l *fakeOpenLog) setSource(name string, src *blockingSource) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sources == nil {
		l.sources = make(map[string]*blockingSource)
	}
	l.sources[name] = src
}

func (l *fakeOpenLog) source(name string) *blockingSource {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sources[name]
}

// killDevice fails the named device's running capture with err and processes
// the pump's ending, so a test drives the real pump-death path (the fan-out read
// fails, the pump reports it, onPumpDone handles it) rather than stopping the
// device and faking the result.
func killDevice(t *testing.T, app *appliance, log *fakeOpenLog, name string, err error) {
	t.Helper()
	src := log.source(name)
	if src == nil {
		t.Fatalf("no blocking source was opened for %q", name)
	}
	select {
	case src.kill <- err:
	default:
		t.Fatalf("%q's source already has a failure queued; its pump is gone", name)
	}
	select {
	case res := <-app.pumpDone:
		app.onPumpDone(res)
	case <-time.After(5 * time.Second):
		t.Fatalf("%q's pump did not report after its capture failed", name)
	}
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
// deviceRuntime (real fan-out, per-stream pipeline stages, ChanSources and
// Tracks) around a blocking fake source, logging each open and each source close
// through log. It mirrors production openDevice: one metered base capture fanned
// out into one runtime per configured stream.
func fakeOpener(log *fakeOpenLog) func(*config.Device, *levels.Hub) (*deviceRuntime, error) {
	return fakeOpenerWith(log, func(rate, channels int) audio.Source { return newBlockingSource(rate, channels) })
}

// fakeOpenerWith is fakeOpener around the capture source newSrc builds, so a test
// can open a device whose capture fails after the open succeeds.
func fakeOpenerWith(log *fakeOpenLog, newSrc func(rate, channels int) audio.Source) func(*config.Device, *levels.Hub) (*deviceRuntime, error) {
	return func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
		log.add("open:" + dev.Name + "@" + dev.Device)
		// Mirror production ResolveOpenChannels: open at >= the highest selected
		// channel (the union is sorted-unique, so its last element is the max), not
		// len(union), so a non-contiguous selection like [1,3] opens 3 channels and a
		// stream selecting channel 3 does not read past the base buffer.
		openCh := 1
		if u := dev.StreamChannelUnion(); len(u) > 0 {
			openCh = u[len(u)-1]
		}
		src := newSrc(dev.Rate, openCh)
		if bs, ok := src.(*blockingSource); ok {
			log.setSource(dev.Name, bs)
		}
		metered := audio.NewMeteredSource(loggingClose{src, dev.Name, log}, hub.Meter(dev.Name, openCh))
		streams := make([]*streamRuntime, 0, len(dev.Streams))
		for i := range dev.Streams {
			s := dev.Streams[i]
			frames := rtspserver.NewChanSource(64)
			streams = append(streams, &streamRuntime{
				stream: s,
				stage:  pipeline.NewPCM(),
				frames: frames,
				track:  &rtspserver.Track{Path: s.Path, PayloadType: 96, Frames: frames},
			})
		}
		fanout, consumers := audio.NewFanout(metered, dev.Name, fanoutStreams(streams))
		for i := range streams {
			streams[i].src = audio.NewSelectingSource(consumers[i], openCh, streams[i].stream.Channels)
		}
		return &deviceRuntime{
			dev:      *dev,
			src:      metered,
			fanout:   fanout,
			streams:  streams,
			rate:     dev.Rate,
			channels: openCh,
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
	return config.Device{
		Name: name, Device: hw, Rate: rate, Format: testFmtS16,
		Streams: []config.Stream{{Path: path, Mode: config.ModePCM, Channels: []int{1}}},
	}
}

func newTestAppliance(t *testing.T) (*appliance, *fakeOpenLog, context.CancelFunc) {
	t.Helper()
	// Stub the mDNS responder so reconcile tests that set a Listen address do not
	// multicast a fake service onto the host's real LAN. announceGen is bumped by
	// restartAnnounce before startAnnounce runs, so a no-op stub still lets the
	// rebuild-count assertions work. Swapping a package var means these tests must
	// stay sequential (they are; none calls t.Parallel).
	prevAnnounce := startAnnounce
	startAnnounce = func(context.Context, string, []*deviceRuntime, bool) {}
	t.Cleanup(func() { startAnnounce = prevAnnounce })

	ctx, cancel := context.WithCancel(t.Context())
	log := &fakeOpenLog{}
	// One guard is shared by the RTSP server and the appliance, exactly as
	// main.go wires them, so a token applied by reconcile enforces on the stream
	// too. Passing it only to newAppliance (as before) left the RTSP server with
	// a nil guard, so the auth assertions could not see whether the stream was
	// actually protected.
	guard := auth.NewGuard("")
	srv := rtspserver.New(rtspserver.Config{Listen: testListenAny, Auth: guard})
	// A real Center is the notifier so the emission tests assert against its
	// Snapshot and Active state, exercising the same idempotency the production
	// wiring relies on. Tests read it back via app.notifier.(*notify.Center).
	app := newAppliance(ctx, levels.NewHub(), srv, &provider{version: "test", start: time.Now()}, guard, notify.NewCenter())
	app.open = fakeOpener(log)
	// Every id resolves to present hardware at an address equal to the id, so the
	// lifecycle tests do not read the host's sysfs; the identity tests replace
	// this with a fakeHost.
	app.resolve = func(id string) (audio.Hardware, error) {
		return audio.Hardware{ID: id, HWAddr: id, IDStable: true}, nil
	}
	// A test that hits an open failure arms a real retry timer; stop it so it
	// does not outlive the test.
	t.Cleanup(app.stopRetries)
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

// recordedGate is the gate the pump handed one stream's stage.
type recordedGate struct {
	path string
	gate pipeline.Gate
}

// gateRecorder is a pipeline.Stage that hands the gate it was given to
// the test, tagged with its stream's path, then drains its source until the
// device stops.
type gateRecorder struct {
	path  string
	gates chan<- recordedGate
}

func (g gateRecorder) Run(src audio.Source, gate pipeline.Gate, _ func(pipeline.Frame) error) error {
	g.gates <- recordedGate{path: g.path, gate: gate}
	for {
		if _, err := src.Read(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// TestAppliancePumpGatesStageOnFeed pins the production wiring of the encode
// gate: the pump must hand each stage its own stream feed's play session, so a
// stream encodes exactly while a client plays it and each new client starts a
// new session. A nil gate would bring back
// encoding for no client, a gate that never opens would stream nothing to a
// playing client, and a sibling's gate would encode one stream on another's
// client; the stage-level tests cannot see any of these, because they pass the
// gate themselves. The device has two streams so the last case is visible.
func TestAppliancePumpGatesStageOnFeed(t *testing.T) {
	app, log, cancel := newTestAppliance(t)
	defer cancel()
	defer app.closeAll()
	dev := testDevice("a", "hw:0", "/a", 48000)
	dev.Streams = append(dev.Streams, config.Stream{Path: "/a2", Mode: config.ModePCM, Channels: []int{1}})
	// One slot per stream, so no stage blocks handing over its gate.
	gates := make(chan recordedGate, len(dev.Streams))
	open := fakeOpener(log)
	app.open = func(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
		rt, err := open(dev, hub)
		if err != nil {
			return nil, err
		}
		for _, sr := range rt.streams {
			sr.stage = gateRecorder{path: sr.stream.Path, gates: gates}
		}
		return rt, nil
	}

	app.reconcile(&config.Config{Devices: []config.Device{dev}})

	got := make(map[string]pipeline.Gate)
	for range dev.Streams {
		select {
		case r := <-gates:
			if r.gate == nil {
				t.Fatalf("the pump passed stream %s a nil gate: its stage would encode with no client playing", r.path)
			}
			got[r.path] = r.gate
		case <-time.After(2 * time.Second):
			t.Fatalf("the pump ran %d of %d stream stages", len(got), len(dev.Streams))
		}
	}
	feeds := make(map[string]*rtspserver.ChanSource)
	for _, sr := range app.devices["a"].streams {
		feeds[sr.stream.Path] = sr.frames
	}
	// check asserts every stream's gate against the one path that should be open
	// ("" for none).
	check := func(when, open string) {
		t.Helper()
		for path, gate := range got {
			if on, _ := gate(); on != (path == open) {
				t.Errorf("%s: gate of %s = %v, want %v", when, path, on, path == open)
			}
		}
	}
	check("before any client plays", "")
	for _, path := range []string{"/a", "/a2"} {
		feeds[path].SetActive(true)
		check("while only "+path+" plays", path)
		_, first := got[path]()
		feeds[path].SetActive(false)
		check("after "+path+" stops", "")
		// The gate carries the feed's play session, so the stage sees the next
		// client as a new session (its encoder reset) even across a quick replay.
		feeds[path].SetActive(true)
		if _, next := got[path](); next == first {
			t.Errorf("gate of %s kept session %d across a new PLAY, want a new session", path, first)
		}
		feeds[path].SetActive(false)
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

	// Both must be serving after the swap. When a opens its swapped-in card, the
	// other device's OLD runtime still sits in a.devices holding that same address
	// but marked superseded; hardwareOwner must ignore a superseded runtime, or
	// one of the two would be refused as a duplicate of the card it is taking over.
	for _, name := range []string{"a", "b"} {
		if st := app.devices[name].currentState(); st != mgmtserver.StateServing {
			t.Errorf("%s state = %s after the swap, want serving (hardwareOwner must ignore the superseded old runtime)", name, st)
		}
	}

	drainPump(t, app) // the two superseded old pumps
	drainPump(t, app)
}

func TestRememberCapsRetainsLastKnown(t *testing.T) {
	a := &appliance{capsCache: map[string]deviceCaps{}}

	// First probe records the caps.
	r, c := a.rememberCaps(devHW1, []int{48000, 96000}, []int{1, 2})
	if len(r) != 2 || len(c) != 2 {
		t.Fatalf("first probe = %v %v, want the probed values", r, c)
	}

	// A transient empty probe (card-swap window) keeps the last-known caps.
	r, c = a.rememberCaps(devHW1, nil, nil)
	if len(r) != 2 || r[0] != 48000 || len(c) != 2 {
		t.Errorf("empty re-probe = %v %v, want the retained [48000 96000] [1 2]", r, c)
	}

	// A later non-empty probe replaces the cache.
	r, c = a.rememberCaps(devHW1, []int{384000}, []int{1})
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
	if !app.prov.authRequired() {
		t.Error("reconcile with a token must report authRequired")
	}
	cfg.Auth.Token = ""
	app.reconcile(&cfg)
	if app.guard.Enabled() {
		t.Error("reconcile with an empty token must disable the guard")
	}
	if app.prov.authRequired() {
		t.Error("reconcile with an empty token must report open access")
	}
}

// recordingMonitors is a fake monitor.Monitors that records each Apply so a
// reconcile test can assert the settings it re-armed the monitors with.
type recordingMonitors struct {
	calls []monitor.Settings
}

func (r *recordingMonitors) Apply(s *monitor.Settings) { r.calls = append(r.calls, *s) }

func TestApplianceReconcileAppliesMonitorSettings(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	rec := &recordingMonitors{}
	app.monitors = rec

	cfg := config.Config{Devices: []config.Device{testDevice("a", "hw:0", "/a", 48000)}}
	cfg.ApplyDefaults()
	app.reconcile(&cfg)
	if len(rec.calls) != 1 {
		t.Fatalf("Apply calls after first reconcile = %d, want 1", len(rec.calls))
	}
	rt := app.devices["a"]

	// Change only a notification threshold. Replace the pointer (rather than
	// deref-assigning) so the first call's recorded Settings, which shares the old
	// pointer, is not aliased. Apply is re-armed with the new value and no device
	// is restarted, because the reload plan is empty.
	newCPU := 95
	cfg.Notifications.Host.CPUPercent = &newCPU
	app.reconcile(&cfg)
	if len(rec.calls) != 2 {
		t.Fatalf("Apply calls after a threshold change = %d, want 2", len(rec.calls))
	}
	last := rec.calls[1]
	if last.Host.CPUPercent == nil || *last.Host.CPUPercent != 95 {
		t.Errorf("re-armed Host.CPUPercent = %v, want 95", last.Host.CPUPercent)
	}
	if !last.QuietAlert["a"] {
		t.Error("re-armed QuietAlert[a] = false, want true (default on)")
	}
	if app.devices["a"] != rt {
		t.Error("a notification-only change restarted the device, want no restart")
	}
	if app.serving() != 1 {
		t.Errorf("serving = %d after a threshold change, want 1 (unchanged)", app.serving())
	}
}

func TestApplianceAuthToggleRebuildsAnnouncement(t *testing.T) {
	app, _, cancel := newTestAppliance(t)
	defer cancel()
	cfg := config.Config{Listen: testListenAny, Devices: []config.Device{testDevice("garden", devHW1, "/garden", 48000)}}
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
