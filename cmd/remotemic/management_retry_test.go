//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
)

// patchedToken is a token saved through an API before its listener fails.
const patchedToken = "patched-before-the-fault-token"

// fakeServer returns a server that looks like an API serving at addr until
// stop is called with why it stopped (nil for a shutdown on ctx).
func fakeServer(addr string) (s *mgmtServer, stop func(error)) {
	s = &mgmtServer{mgmtEndpoint: mgmtEndpoint{addr: addr, certPath: "cert.pem"}, stopped: make(chan struct{})}
	return s, func(err error) {
		s.err = err
		close(s.stopped)
	}
}

// supervise runs superviseManagement on a fresh handle, as startManagementWith
// does, and returns the handle.
func supervise(ctx context.Context, srv *mgmtServer, startErr error, r mgmtRetry) *mgmt {
	m := &mgmt{done: make(chan struct{})}
	m.cur.Store(srv)
	if r.died == nil {
		r.died = func(*mgmtServer, error) {}
	}
	go superviseManagement(ctx, m, srv, startErr, r)
	return m
}

// waitServing blocks until h serves an API other than prev, and returns it.
// The real listener tests cannot run in a synctest bubble, so this polls.
func waitServing(t *testing.T, h *mgmt, prev *mgmtServer) *mgmtServer {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for {
		if s := h.serving(); s != nil && s != prev {
			return s
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("the supervisor did not bring an API up")
		}
	}
}

func TestSuperviseManagementBacksOffThenServes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		delays := []time.Duration{time.Second, 2 * time.Second}
		start := time.Now()
		var at []time.Duration
		fake, stop := fakeServer(testMgmtAddr)
		attempt := func() (*mgmtServer, error) {
			at = append(at, time.Since(start))
			if len(at) < 3 {
				return nil, errors.New("still broken")
			}
			return fake, nil
		}
		h := supervise(t.Context(), nil, errors.New("broken"), mgmtRetry{attempt: attempt, delays: delays, stable: time.Hour})
		if h.serving() != nil {
			t.Fatal("a handle whose start failed reports a serving API")
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if h.serving() != fake {
			t.Fatalf("serving = %v, want the API the third attempt brought up", h.serving())
		}
		// The first delay, then the second, then the last delay repeating.
		want := []time.Duration{time.Second, 3 * time.Second, 5 * time.Second}
		if len(at) != len(want) {
			t.Fatalf("got %d attempts, want %d", len(at), len(want))
		}
		for i := range want {
			if at[i] != want[i] {
				t.Errorf("attempt %d at %s, want %s", i+1, at[i], want[i])
			}
		}

		// Wait follows the recovered API: it blocks until that API shuts down.
		waited := make(chan struct{})
		go func() { h.Wait(); close(waited) }()
		synctest.Wait()
		select {
		case <-waited:
			t.Fatal("Wait returned while the recovered API is still serving")
		default:
		}
		stop(nil)
		<-waited
		if h.serving() != nil {
			t.Error("a shut-down API still reads as serving")
		}
	})
}

// TestSuperviseManagementLogsChangesOnly pins the log bound: a failure
// repeating the previous message is not logged again, a different one is, and
// the recovery is. Not parallel: it captures the process logger.
func TestSuperviseManagementLogsChangesOnly(t *testing.T) {
	out := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		errs := []error{errors.New("broken"), errors.New("broken"), errors.New("other")}
		fake, stop := fakeServer(testMgmtAddr)
		n := 0
		attempt := func() (*mgmtServer, error) {
			if n < len(errs) {
				n++
				return nil, errs[n-1]
			}
			return fake, nil
		}
		h := supervise(t.Context(), nil, errors.New("broken"), mgmtRetry{attempt: attempt, delays: []time.Duration{time.Second}, stable: time.Hour})
		time.Sleep(10 * time.Second)
		stop(nil)
		h.Wait()
	})
	if got := strings.Count(out.String(), "management API still unavailable"); got != 1 {
		t.Errorf("logged %d failure lines, want 1 (only the changed message); log:\n%s", got, out)
	}
	if !strings.Contains(out.String(), "management API recovered after 4 background attempt(s)") {
		t.Errorf("log lacks the recovery line; log:\n%s", out)
	}
}

func TestSuperviseManagementStopsOnCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		attempt := func() (*mgmtServer, error) {
			attempts++
			return nil, errors.New("broken")
		}
		h := supervise(ctx, nil, errors.New("broken"), mgmtRetry{attempt: attempt, delays: []time.Duration{time.Minute}, stable: time.Hour})
		time.Sleep(150 * time.Second)
		cancel()
		h.Wait()
		if attempts != 2 {
			t.Errorf("got %d attempts in 150 s at one per minute, want 2", attempts)
		}
		if h.serving() != nil {
			t.Error("a retry that never succeeded must not report a serving API")
		}
	})
}

// TestSuperviseManagementRestartsDeadAPI pins the runtime recovery: an API
// whose listener fails is handed to died, reads as not serving while the
// supervisor waits out the backoff, and a new one takes its place.
func TestSuperviseManagementRestartsDeadAPI(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first, kill := fakeServer(testMgmtAddr)
		second, stop := fakeServer(testMgmtAddr)
		var deaths []error
		died := func(s *mgmtServer, err error) {
			if s != first {
				t.Errorf("died got %v, want the API that stopped", s)
			}
			deaths = append(deaths, err)
		}
		// Atomic: the check before the first delay reads it with no
		// happens-before edge to the supervisor's later increment.
		var attempts atomic.Int32
		attempt := func() (*mgmtServer, error) {
			attempts.Add(1)
			return second, nil
		}
		h := supervise(t.Context(), first, nil, mgmtRetry{attempt: attempt, died: died, delays: []time.Duration{time.Second}, stable: time.Hour})
		if h.serving() != first {
			t.Fatal("the API that came up at start does not read as serving")
		}
		fault := errors.New("accept: broken")
		kill(fault)
		synctest.Wait()
		if h.serving() != nil {
			t.Error("a dead API still reads as serving during the backoff")
		}
		if len(deaths) != 1 || !errors.Is(deaths[0], fault) {
			t.Errorf("died saw %v, want the one serve error", deaths)
		}
		if n := attempts.Load(); n != 0 {
			t.Errorf("got %d attempts before the first delay, want 0", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if n := attempts.Load(); h.serving() != second || n != 1 {
			t.Errorf("serving = %v after %d attempt(s), want the restarted API after one", h.serving(), n)
		}
		stop(nil)
		h.Wait()
	})
}

// TestSuperviseManagementIgnoresDeathDuringShutdown pins that a listener
// failure racing shutdown (the server may report it rather than the
// cancellation) ends the supervisor instead of reporting a runtime death.
func TestSuperviseManagementIgnoresDeathDuringShutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		first, kill := fakeServer(testMgmtAddr)
		deaths := 0
		died := func(*mgmtServer, error) { deaths++ }
		attempt := func() (*mgmtServer, error) { return nil, errors.New("unexpected attempt") }
		h := supervise(ctx, first, nil, mgmtRetry{attempt: attempt, died: died, delays: []time.Duration{time.Second}, stable: time.Hour})
		cancel()
		kill(errors.New("accept: use of closed network connection"))
		h.Wait()
		if deaths != 0 {
			t.Errorf("died called %d time(s) during shutdown, want 0", deaths)
		}
	})
}

// TestSuperviseManagementBackoffAfterShortServe pins the backoff across
// outages: an API that dies before serving for stable resumes the backoff
// where it left off, and one that served for stable restarts it.
func TestSuperviseManagementBackoffAfterShortServe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
		start := time.Now()
		var at []time.Duration
		var stops []func(error)
		attempt := func() (*mgmtServer, error) {
			at = append(at, time.Since(start))
			s, stop := fakeServer(testMgmtAddr)
			stops = append(stops, stop)
			return s, nil
		}
		h := supervise(t.Context(), nil, errors.New("broken"), mgmtRetry{attempt: attempt, delays: delays, stable: 10 * time.Second})

		time.Sleep(time.Second) // attempt 1 at 1 s
		synctest.Wait()
		time.Sleep(time.Second)
		stops[0](errors.New("died fast")) // at 2 s, after 1 s of serving
		time.Sleep(2 * time.Second)       // attempt 2 at 4 s: the backoff resumed at delays[1]
		synctest.Wait()
		time.Sleep(10 * time.Second)
		stops[1](errors.New("died late")) // at 14 s, after 10 s of serving
		time.Sleep(time.Second)           // attempt 3 at 15 s: the backoff restarted at delays[0]
		synctest.Wait()

		want := []time.Duration{time.Second, 4 * time.Second, 15 * time.Second}
		if len(at) != len(want) {
			t.Fatalf("attempts at %v, want %v", at, want)
		}
		for i := range want {
			if at[i] != want[i] {
				t.Errorf("attempt %d at %s, want %s", i+1, at[i], want[i])
			}
		}
		stops[2](nil)
		h.Wait()
	})
}

// TestSuperviseManagementStopsWhenDisabled pins that an attempt finding the
// API disabled in the config file ends the supervisor instead of retrying.
func TestSuperviseManagementStopsWhenDisabled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		attempt := func() (*mgmtServer, error) {
			attempts++
			return nil, errMgmtDisabled
		}
		h := supervise(t.Context(), nil, errors.New("broken"), mgmtRetry{attempt: attempt, delays: []time.Duration{time.Second}, stable: time.Hour})
		h.Wait()
		if attempts != 1 || h.serving() != nil {
			t.Errorf("got %d attempts, serving %v; want one attempt and no API", attempts, h.serving())
		}
	})
}

// TestStartManagementRecoversAfterCertFailure drives the real bring-up: the
// certificate cannot be written at start (cert_dir is a regular file), then the
// fault clears and the background retry brings the API up over TLS.
func TestStartManagementRecoversAfterCertFailure(t *testing.T) {
	t.Parallel()
	certDir := filepath.Join(t.TempDir(), "certs")
	if err := os.WriteFile(certDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: certDir}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	center := notify.NewCenter()
	p := &mgmtParams{cfgPath: cfgPath, cfg: cfg, storeCfg: cfg, prov: newProvider(), center: center}
	h, ok := startManagementWith(ctx, p, []time.Duration{10 * time.Millisecond})
	if ok {
		t.Fatal("a certificate failure must report management unavailable")
	}
	// The outage is raised while the fault lasts (cert_dir is still a file, so
	// no retry can have succeeded yet).
	if act := center.Active(); len(act) != 1 || act[0].Key != mgmtDownKey {
		t.Fatalf("active = %+v, want the management-unavailable condition", act)
	}
	if err := os.Remove(certDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(certDir, 0o700); err != nil {
		t.Fatal(err)
	}

	s := waitServing(t, h, nil)
	if s.certPath != filepath.Join(certDir, testCertFile) {
		t.Errorf("got certificate path %q, want the one under cert_dir", s.certPath)
	}
	if leaf := dialLeaf(t, s.addr); leaf == nil {
		t.Fatal("the recovered API did not present a certificate")
	}
	if act := center.Active(); len(act) != 0 {
		t.Errorf("active = %+v, want the condition cleared once the API serves", act)
	}
	cancel()
	h.Wait()
}

// TestStartManagementRestartsAfterListenerDies drives a runtime listener
// fault through the real server: the dead API stops advertising its address
// in the run lock, raises the outage, and a new API serves the config (with
// its PATCHes) of the one that died.
func TestStartManagementRestartsAfterListenerDies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// A first-run config, as LoadOrDefault returns with no file: defaults
	// applied and a nil device list, which a save writes as "devices: []" and
	// a reload reads back as an empty, non-nil list.
	defaults := config.Default()
	defaults.Management.Listen = testListenAny
	defaults.Management.CertDir = dir
	cfg := &defaults
	lock, err := runlock.Acquire(runlock.PathFor(cfgPath), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	center := notify.NewCenter()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The file the retry reloads is the one the dead API saved, so nothing
	// was edited while it was down and nothing may be applied live.
	var reloads atomic.Int32
	reloader := func(context.Context, config.Config) error {
		reloads.Add(1)
		return nil
	}
	p := &mgmtParams{cfgPath: cfgPath, cfg: cfg, storeCfg: cfg, prov: newProvider(), center: center, reloader: reloader, runLock: &runLockPublisher{lock: lock, cfgPath: cfgPath}}
	h, ok := startManagementWith(ctx, p, []time.Duration{10 * time.Millisecond})
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	first := h.serving()
	if st, _, _ := runlock.ReadState(runlock.PathFor(cfgPath)); st.MgmtAddr != first.addr {
		t.Errorf("run lock advertises %q, want the serving API's %q", st.MgmtAddr, first.addr)
	}
	// A PATCH saved through the first API must survive into the next one.
	if err := first.store.Update(func(c config.Config) (config.Config, error) {
		c.Auth.Token = patchedToken
		return c, nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := first.ln.Close(); err != nil {
		t.Fatal(err)
	}
	second := waitServing(t, h, first)
	if first.wait() == nil {
		t.Error("the dead API reports a shutdown on ctx, want its serve error")
	}
	if st, _, _ := runlock.ReadState(runlock.PathFor(cfgPath)); st.MgmtAddr != second.addr {
		t.Errorf("run lock advertises %q, want the restarted API's %q", st.MgmtAddr, second.addr)
	}
	if got := second.store.Config().Auth.Token; got != patchedToken {
		t.Errorf("restarted API serves token %q, want the one patched through the dead API", got)
	}
	var onsets, configEvents int
	for _, n := range center.Snapshot().Notifications {
		if n.Key == mgmtDownKey && strings.Contains(n.Message, "stopped") {
			onsets++
		}
		if n.Category == notify.CategoryConfig {
			configEvents++
		}
	}
	if onsets != 1 {
		t.Errorf("got %d outage onsets naming the stop, want 1", onsets)
	}
	if n := reloads.Load(); n != 0 || configEvents != 0 {
		t.Errorf("got %d live reloads and %d config events, want none for the file the dead API saved itself", n, configEvents)
	}
	if act := center.Active(); len(act) != 0 {
		t.Errorf("active = %+v, want the outage cleared once the API serves again", act)
	}
	cancel()
	h.Wait()
}

// TestMgmtParamsDied pins what an API that stopped on its own leaves behind
// while the backoff runs, before any attempt can overwrite it: the run lock
// advertises no address, the store seed is the dead API's config (with its
// PATCHes), and the outage is raised.
func TestMgmtParamsDied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, testCfgFile)
	lockPath := runlock.PathFor(cfgPath)
	lock, err := runlock.Acquire(lockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release run lock: %v", err)
		}
	})
	pub := &runLockPublisher{lock: lock, cfgPath: cfgPath}
	pub.publish(&mgmtEndpoint{addr: testMgmtAddr, certPath: testCertFile})

	startup := config.Default()
	store := mgmtserver.NewFileConfigStore(cfgPath, &startup)
	if err := store.Update(func(c config.Config) (config.Config, error) {
		c.Auth.Token = patchedToken
		return c, nil
	}); err != nil {
		t.Fatal(err)
	}
	center := notify.NewCenter()
	p := &mgmtParams{cfgPath: cfgPath, storeCfg: &startup, center: center, runLock: pub}
	p.died(&mgmtServer{store: store}, errors.New("accept: broken"))

	if st, ok, err := runlock.ReadState(lockPath); err != nil || !ok || st.MgmtAddr != "" {
		t.Errorf("run lock = %+v, %v, %v; want a published state with no address", st, ok, err)
	}
	if p.storeCfg.Auth.Token != patchedToken {
		t.Errorf("store seed has token %q, want the dead API's patched one", p.storeCfg.Auth.Token)
	}
	if startup.Auth.Token != "" {
		t.Error("the re-seed wrote through the startup snapshot run() holds")
	}
	if act := center.Active(); len(act) != 1 || act[0].Key != mgmtDownKey || !strings.Contains(act[0].Message, "accept: broken") {
		t.Errorf("active = %+v, want the outage naming the serve error", act)
	}
}

// TestRecoverManagementFollowsEditedFile pins that an attempt binds and reads
// the certificate where the reloaded file says, with the serve flags still
// overriding it, so editing the file fixes a port in use or a bad cert_dir.
func TestRecoverManagementFollowsEditedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	editedDir := filepath.Join(dir, "edited-certs")
	if err := os.Mkdir(editedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: filepath.Join(dir, "startup-certs")}}
	edited := startup.Clone()
	edited.Management.CertDir = editedDir
	edited.Management.Listen = "127.0.0.1:1" // overridden by the flag below
	if err := config.Save(cfgPath, &edited); err != nil {
		t.Fatal(err)
	}
	ov := serveOverrides{mgmtListen: testListenAny, set: map[string]bool{"mgmt-listen": true}}
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, overrides: ov, prov: newProvider()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement: %v", err)
	}
	if want := filepath.Join(editedDir, testCertFile); s.certPath != want || p.prov.certPath != want {
		t.Errorf("certificate at %q (provider %q), want the edited cert_dir's %q", s.certPath, p.prov.certPath, want)
	}
	if leaf := dialLeaf(t, s.addr); leaf == nil {
		t.Error("the API did not serve on the overriding --mgmt-listen address")
	}
	if p.storeCfg.Management.Listen != edited.Management.Listen {
		t.Errorf("store seeded with listen %q, want the file's %q (overrides stay out of the store)", p.storeCfg.Management.Listen, edited.Management.Listen)
	}
	cancel()
	_ = s.wait()
}

// TestRecoverManagementStopsWhenFileDisablesManagement pins that a file now
// disabling management fails the attempt with errMgmtDisabled, which ends the
// retry, and resolves the outage rather than leaving it raised forever.
func TestRecoverManagementStopsWhenFileDisablesManagement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	edited := startup.Clone()
	edited.Management.Enabled = new(false)
	if err := config.Save(cfgPath, &edited); err != nil {
		t.Fatal(err)
	}
	center := notify.NewCenter()
	center.Onset(notify.Notification{Key: mgmtDownKey, Severity: notify.SeverityError, Category: notify.CategorySystem, Title: "down"})
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider(), center: center}
	if _, err := recoverManagement(t.Context(), p); !errors.Is(err, errMgmtDisabled) {
		t.Fatalf("got %v, want errMgmtDisabled", err)
	}
	if act := center.Active(); len(act) != 0 {
		t.Errorf("active = %+v, want the outage resolved", act)
	}

	// The --management flag still wins over the file.
	p.overrides = serveOverrides{management: true, set: map[string]bool{keyMgmt: true}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement with --management: %v", err)
	}
	cancel()
	_ = s.wait()
}

func TestRecoverManagementAppliesFileEditedWhileDown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	// The token CLI edited the file while no API was published.
	edited := startup.Clone()
	edited.Auth.Token = "edited-while-down-token"
	if err := config.Save(cfgPath, &edited); err != nil {
		t.Fatal(err)
	}
	onDisk, err := config.LoadOrDefault(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var applied []config.Config
	reloader := func(_ context.Context, c config.Config) error {
		applied = append(applied, c)
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	center := notify.NewCenter()
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider(), reloader: reloader, center: center}
	p.certPath = filepath.Join(dir, testCertFile)
	p.keyPath = filepath.Join(dir, "mgmt-key.pem")

	h, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement: %v", err)
	}
	if len(applied) != 1 || applied[0].Auth.Token != edited.Auth.Token {
		t.Fatalf("got reloads %+v, want one applying the edited token", applied)
	}
	// Like a PATCH, the apply leaves a config event in the history.
	var events int
	for _, n := range center.Snapshot().Notifications {
		if n.Category == notify.CategoryConfig && n.Kind == notify.KindEvent {
			events++
		}
	}
	if events != 1 {
		t.Errorf("got %d config events, want 1 for the applied file", events)
	}
	if p.storeCfg.Auth.Token != edited.Auth.Token {
		t.Errorf("store seeded with token %q, want the edited file's", p.storeCfg.Auth.Token)
	}
	if startup.Auth.Token != "" {
		t.Error("the startup snapshot run() holds must not be written through")
	}
	// The API that came up serves the edited file, not the startup snapshot: a
	// later web UI save would otherwise write the stale token back.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	if body := getBody(t, ctx, client, h.addr, "/api/v1/config"); !strings.Contains(body, edited.Auth.Token) {
		t.Errorf("GET /config = %s, want the edited token", body)
	}
	cancel()
	_ = h.wait()

	// A second attempt with the file unchanged applies nothing. The store holds a
	// Clone of what was loaded, as run() seeds it (splitServeConfig), so this
	// compares the way production does.
	seeded := onDisk.Clone()
	p.storeCfg = &seeded
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	h2, err := recoverManagement(ctx2, p)
	if err != nil {
		t.Fatalf("recoverManagement: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("got %d reloads, want none for an unchanged file", len(applied)-1)
	}
	cancel2()
	_ = h2.wait()
}

// TestRecoverManagementKeepsSnapshotWhenFileMissing pins the missing-file case: a
// config file gone at retry time (an unmounted volume) must not read as the
// device-less default and be applied live, which would tear down every stream.
// The attempt serves with the startup snapshot and applies nothing.
func TestRecoverManagementKeepsSnapshotWhenFileMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml") // never written
	startup := config.Config{
		Management: config.Management{Listen: testListenAny, CertDir: dir},
		Devices: []config.Device{{
			Name: "porch", Device: "hw:1,0", Rate: 48000, Format: "s16",
			Streams: []config.Stream{{Path: "/porch", Mode: config.ModePCM, Channels: []int{1}}},
		}},
	}
	var applied int
	reloader := func(context.Context, config.Config) error {
		applied++
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider(), reloader: reloader}
	p.certPath = filepath.Join(dir, testCertFile)
	p.keyPath = filepath.Join(dir, "mgmt-key.pem")

	h, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement with the file missing: %v, want the API up on the startup snapshot", err)
	}
	if applied != 0 {
		t.Errorf("got %d reloads, want none for a missing file", applied)
	}
	if p.storeCfg != &startup || len(p.storeCfg.Devices) != 1 {
		t.Errorf("store seeded with %+v, want the startup snapshot", p.storeCfg)
	}
	cancel()
	_ = h.wait()
}

// TestRecoverManagementFailedServeKeepsCondition pins that the unavailable
// condition clears only once the API actually serves: an attempt that loads the
// config but cannot bind leaves it raised.
func TestRecoverManagementFailedServeKeepsCondition(t *testing.T) {
	t.Parallel()
	occupied, err := net.Listen("tcp", testListenAny)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := occupied.Close(); cerr != nil {
			t.Errorf("closing occupied listener: %v", cerr)
		}
	})
	dir := t.TempDir()
	startup := config.Config{Management: config.Management{Listen: occupied.Addr().String(), CertDir: dir}}
	center := notify.NewCenter()
	center.Onset(notify.Notification{Key: mgmtDownKey, Severity: notify.SeverityError, Category: notify.CategorySystem, Title: "down"})
	p := &mgmtParams{cfgPath: filepath.Join(dir, "config.yaml"), cfg: &startup, storeCfg: &startup, prov: newProvider(), center: center}
	p.certPath = filepath.Join(dir, testCertFile)
	p.keyPath = filepath.Join(dir, "mgmt-key.pem")
	if _, err := recoverManagement(t.Context(), p); err == nil {
		t.Fatal("recoverManagement on a busy port succeeded, want a bind failure")
	}
	if act := center.Active(); len(act) != 1 || act[0].Key != mgmtDownKey {
		t.Errorf("active = %+v, want the condition still raised after a failed attempt", act)
	}
}

// TestRecoverManagementReloadErrorFailsAttempt pins the reloader-error branch: a
// file the running appliance rejects fails the attempt, so the API does not
// come up seeded with a config that does not match what is running.
func TestRecoverManagementReloadErrorFailsAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	edited := startup.Clone()
	edited.Auth.Token = "rejected-by-reload-token"
	if err := config.Save(cfgPath, &edited); err != nil {
		t.Fatal(err)
	}
	reloader := func(context.Context, config.Config) error { return errors.New("reconcile refused") }
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider(), reloader: reloader}
	p.certPath = filepath.Join(dir, testCertFile)
	p.keyPath = filepath.Join(dir, "mgmt-key.pem")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h, err := recoverManagement(ctx, p)
	if err == nil {
		cancel()
		_ = h.wait()
		t.Fatal("recoverManagement succeeded although the reload failed")
	}
	if p.storeCfg != &startup {
		t.Error("a failed reload must leave the store seed at the startup snapshot")
	}
}

func TestRecoverManagementFailsOnUnloadableConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: [not, a, string\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider()}
	// Valid certificate paths, so the attempt fails only for the config.
	p.certPath = filepath.Join(dir, testCertFile)
	p.keyPath = filepath.Join(dir, "mgmt-key.pem")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h, err := recoverManagement(ctx, p)
	if err != nil && !strings.Contains(err.Error(), "cannot reload config") {
		t.Errorf("got error %v, want the config reload failure", err)
	}
	if err == nil {
		cancel()
		_ = h.wait()
		t.Fatal("a config file that no longer loads must fail the attempt")
	}
}

// TestRecoverManagementPublishesRunLock pins the token CLI race fix: while a
// background attempt reloads the config file and brings the API up, the run
// lock reads as starting, so a token command asks the operator to retry
// instead of editing a file the attempt may already have read. A successful
// attempt publishes its endpoint itself, and a failed one leaves the lock
// with no endpoint, so the token commands edit the file again.
func TestRecoverManagementPublishesRunLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	lockPath := runlock.PathFor(cfgPath)
	lock, err := runlock.Acquire(lockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	pub := &runLockPublisher{lock: lock, cfgPath: cfgPath}
	pub.publish(nil)

	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	edited := startup.Clone()
	edited.Auth.Token = "edited-while-down-token"
	if err := config.Save(cfgPath, &edited); err != nil {
		t.Fatal(err)
	}
	// The reloader runs mid-attempt, after the reload: the lock must read as
	// starting there.
	var midAttempt []bool
	reloader := func(context.Context, config.Config) error {
		_, ok, rerr := runlock.ReadState(lockPath)
		if rerr != nil {
			t.Errorf("ReadState mid-attempt: %v", rerr)
		}
		midAttempt = append(midAttempt, ok)
		return nil
	}
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider(), reloader: reloader, center: notify.NewCenter(), runLock: pub}
	p.certPath = filepath.Join(dir, testCertFile)
	p.keyPath = filepath.Join(dir, "mgmt-key.pem")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement: %v", err)
	}
	if len(midAttempt) != 1 || midAttempt[0] {
		t.Errorf("lock published mid-attempt = %v, want [false] (starting)", midAttempt)
	}
	st, ok, err := runlock.ReadState(lockPath)
	if err != nil || !ok || st.MgmtAddr != h.addr {
		t.Errorf("after the attempt: ReadState = %+v, %v, %v; want the API's address %q", st, ok, err, h.addr)
	}
	cancel()
	_ = h.wait()

	// A failed attempt (the file no longer loads) leaves no endpoint.
	if err := os.WriteFile(cfgPath, []byte("listen: [not, a, string\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverManagement(t.Context(), p); err == nil {
		t.Fatal("a config file that no longer loads must fail the attempt")
	}
	st, ok, err = runlock.ReadState(lockPath)
	if err != nil || !ok || st.MgmtAddr != "" {
		t.Errorf("after a failed attempt: ReadState = %+v, %v, %v; want a published state with no endpoint", st, ok, err)
	}
}
