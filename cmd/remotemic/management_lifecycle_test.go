//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// lostSignalled reports whether h's lost channel holds a wake, consuming it.
func lostSignalled(h *mgmt) bool {
	select {
	case <-h.lostC():
		return true
	default:
		return false
	}
}

// TestSuperviseManagementSignalsLost pins the run-loop wake: an API that stops
// on its own and a retry that gives up (the file disabled management) each
// wake run() to retake its exit decision, while a shutdown on ctx does not.
func TestSuperviseManagementSignalsLost(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first, kill := fakeServer(testMgmtAddr)
		second, stop := fakeServer(testMgmtAddr)
		attempts := 0
		attempt := func() (*mgmtServer, error) {
			attempts++
			if attempts == 1 {
				return second, nil
			}
			return nil, errMgmtDisabled
		}
		h := supervise(t.Context(), first, nil, mgmtRetry{attempt: attempt, delays: []time.Duration{time.Second}, stable: time.Hour})
		synctest.Wait()
		if lostSignalled(h) {
			t.Fatal("a serving API woke the run loop")
		}
		kill(errors.New("accept: broken"))
		synctest.Wait()
		if !lostSignalled(h) {
			t.Error("an API that stopped on its own did not wake the run loop")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if h.serving() != second {
			t.Fatal("the retry did not bring the second API up")
		}
		stop(errors.New("accept: broken again"))
		time.Sleep(time.Second)
		synctest.Wait()
		// Two wakes coalesce into one: the death and the give-up.
		if !lostSignalled(h) {
			t.Error("a retry that gave up did not wake the run loop")
		}
		h.Wait()
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		srv, stop := fakeServer(testMgmtAddr)
		h := supervise(ctx, srv, nil, mgmtRetry{attempt: func() (*mgmtServer, error) { return nil, errors.New("unexpected") }, delays: []time.Duration{time.Second}, stable: time.Hour})
		cancel()
		stop(nil)
		h.Wait()
		if lostSignalled(h) {
			t.Error("a shutdown on ctx woke the run loop")
		}
	})
}

// TestSuperviseManagementHaltsBeforeDrain pins the two-step death: the moment
// an API stops, it reads as not serving and halted runs (so the run lock stops
// advertising the dead address at once), while died and the next attempt wait
// for the drain, which an open /events stream holds for the full timeout.
func TestSuperviseManagementHaltsBeforeDrain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first, halt, drained := fakeDrainingServer(testMgmtAddr)
		second, stop := fakeServer(testMgmtAddr)
		var steps []string
		r := mgmtRetry{
			attempt: func() (*mgmtServer, error) {
				steps = append(steps, "attempt")
				return second, nil
			},
			halted: func(*mgmtServer, error) { steps = append(steps, "halted") },
			died:   func(*mgmtServer, error) { steps = append(steps, "died") },
			delays: []time.Duration{time.Second},
			stable: time.Hour,
		}
		h := supervise(t.Context(), first, nil, r)
		halt(errors.New("accept: broken"))
		synctest.Wait()
		if h.serving() != nil {
			t.Error("a stopped API still reads as serving during its drain")
		}
		if !equalStates(steps, "halted") {
			t.Errorf("steps before the drain ended = %v, want [halted]", steps)
		}
		time.Sleep(time.Minute) // a drain longer than the backoff
		synctest.Wait()
		if !equalStates(steps, "halted") {
			t.Errorf("steps during the drain = %v, want no attempt before it ends", steps)
		}
		drained()
		time.Sleep(time.Second)
		synctest.Wait()
		if !equalStates(steps, "halted", "died", "attempt") || h.serving() != second {
			t.Errorf("steps = %v, serving %v; want halted, died, then the attempt that serves", steps, h.serving())
		}
		stop(nil)
		h.Wait()
	})
}

// TestMgmtParamsRetryWiring pins the production supervisor wiring: both death
// steps are handled, and an API must serve for the longest delay before its
// death restarts the backoff.
func TestMgmtParamsRetryWiring(t *testing.T) {
	t.Parallel()
	p := &mgmtParams{}
	r := p.retry(t.Context(), mgmtRetryBackoff[:])
	if r.stable != mgmtRetryBackoff[len(mgmtRetryBackoff)-1] {
		t.Errorf("stable = %s, want the longest delay %s", r.stable, mgmtRetryBackoff[len(mgmtRetryBackoff)-1])
	}
	if r.attempt == nil || r.halted == nil || r.died == nil {
		t.Error("the retry is missing a hook")
	}
}

// TestStartManagementPublishesFirstState pins the startup token window: run()
// publishes nothing before management starts, and startManagement publishes
// exactly one state (the endpoint, or no API after a failed start), so the
// lock reads as starting through the certificate check and the listen, and a
// token command cannot edit the file the API is about to be seeded from.
func TestStartManagementPublishesFirstState(t *testing.T) {
	t.Parallel()
	t.Run("serves", func(t *testing.T) {
		t.Parallel()
		rec := &recordLock{}
		cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		p := &mgmtParams{cfgPath: testCfgFile, cfg: cfg, storeCfg: cfg, prov: newProvider(), runLock: &runLockPublisher{write: rec.write}}
		h, ok := startManagementWith(ctx, p, []time.Duration{time.Hour})
		if !ok {
			t.Fatal("management should have started on an ephemeral port")
		}
		if got := lockStates(rec); !equalStates(got, h.serving().addr) {
			t.Errorf("run lock writes = %v, want only the serving endpoint", got)
		}
		cancel()
		h.Wait()
	})
	t.Run("fails", func(t *testing.T) {
		t.Parallel()
		certDir := filepath.Join(t.TempDir(), "certs")
		if err := os.WriteFile(certDir, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := &recordLock{}
		cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: certDir}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		p := &mgmtParams{cfgPath: testCfgFile, cfg: cfg, storeCfg: cfg, prov: newProvider(), runLock: &runLockPublisher{write: rec.write}}
		h, ok := startManagementWith(ctx, p, []time.Duration{time.Hour})
		if ok {
			t.Fatal("a certificate failure must report management unavailable")
		}
		if got := lockStates(rec); !equalStates(got, "no API") {
			t.Errorf("run lock writes = %v, want only the no-API state", got)
		}
		cancel()
		h.Wait()
	})
}

// TestRecoverManagementMarksStartingUnchangedFile pins the starting mark on the
// path TestRecoverManagementPublishesRunLock cannot see (its reloader only runs
// for a changed file): an attempt whose file is unchanged still marks the lock
// as starting before it serves.
func TestRecoverManagementMarksStartingUnchangedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := &recordLock{}
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// No file at cfgPath: the attempt keeps the config it has, so nothing is
	// reloaded or applied.
	p := &mgmtParams{cfgPath: filepath.Join(dir, testCfgFile), cfg: cfg, storeCfg: cfg, prov: newProvider(), center: notify.NewCenter(), runLock: &runLockPublisher{write: rec.write}}
	s, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement: %v", err)
	}
	if got := lockStates(rec); !equalStates(got, "starting", s.addr) {
		t.Errorf("run lock writes = %v, want starting then the endpoint", got)
	}
	cancel()
	_ = s.wait()
}

// TestRecoverManagementEditedTokenEvents pins that applying a file edited while
// the API was down records what a PATCH would: the access-control event when
// the token changed, and the end of a pending restart-required condition.
func TestRecoverManagementEditedTokenEvents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, testCfgFile)
	startup := config.Config{Management: config.Management{Listen: testListenAny, CertDir: dir}}
	edited := startup.Clone()
	edited.Auth.Token = editedToken
	if err := config.Save(cfgPath, &edited); err != nil {
		t.Fatal(err)
	}
	guard := auth.NewGuard("")
	// The serve reloader reconciles, which sets the guard from the config.
	reloader := func(_ context.Context, c config.Config) error {
		guard.Set(c.Auth.Token)
		return nil
	}
	center := notify.NewCenter()
	// A key a failed hot reload raised earlier; the constant is unexported.
	center.Onset(notify.Notification{Severity: notify.SeverityWarning, Category: notify.CategoryConfig, Key: "config:restart", Title: "Restart required"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &mgmtParams{cfgPath: cfgPath, cfg: &startup, storeCfg: &startup, prov: newProvider(), reloader: reloader, guard: guard, center: center}
	s, err := recoverManagement(ctx, p)
	if err != nil {
		t.Fatalf("recoverManagement: %v", err)
	}
	want := mgmtserver.AuthChangedNotification(false, true)
	var authEvents int
	for _, n := range center.Snapshot().Notifications {
		if n.Title == want.Title && n.Message == want.Message {
			authEvents++
		}
	}
	if authEvents != 1 {
		t.Errorf("got %d access-control events, want 1 for the token the file enabled", authEvents)
	}
	if act := center.Active(); len(act) != 0 {
		t.Errorf("active = %+v, want the restart-required condition cleared by the live apply", act)
	}
	cancel()
	_ = s.wait()
}

// TestServeManagementDrainBoundsStuckHandler drives the drain fallback: a
// handler that never returns (an /events stream that ignores its request)
// keeps Shutdown from finishing, so Close cuts the connection, and the wait for
// handlers is bounded too, so the API still stops.
func TestServeManagementDrainBoundsStuckHandler(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	events := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-release
	})
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &mgmtParams{cfgPath: testCfgFile, cfg: cfg, storeCfg: cfg, prov: newProvider(), events: events, drainTimeout: 50 * time.Millisecond}
	p.useConfig(cfg)
	s, err := serveManagement(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	go func() {
		req, rerr := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+s.addr+mgmtserver.BasePath+"/events", http.NoBody)
		if rerr != nil {
			return
		}
		if resp, derr := client.Do(req); derr == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- s.wait() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("API reported %v after ctx was cancelled, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stuck handler kept the API from stopping")
	}
}

// TestInflightIdle pins the handler count the drain waits on: idle is closed
// with no handler running and stays open until the last one returns.
func TestInflightIdle(t *testing.T) {
	t.Parallel()
	var f inflight
	select {
	case <-f.idle():
	default:
		t.Fatal("idle is not closed with no handler running")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	h := f.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(nil, nil)
	}()
	<-entered
	idle := f.idle()
	select {
	case <-idle:
		t.Fatal("idle closed while a handler runs")
	default:
	}
	close(release)
	<-done
	select {
	case <-idle:
	case <-time.After(10 * time.Second):
		t.Fatal("idle did not close once the last handler returned")
	}
}

// TestPrepareCertificateMetadataFallback drives the metadata fallback: a
// certificate whose metadata cannot be described still serves TLS, while the
// certificate endpoints stay unmounted.
func TestPrepareCertificateMetadataFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prov := newProvider()
	prov.describe = func(*tls.Certificate) (mgmtcert.Info, error) { return mgmtcert.Info{}, errors.New("unparseable") }
	cfg := &config.Config{Management: config.Management{CertDir: dir}}
	p := &mgmtParams{cfgPath: filepath.Join(dir, testCfgFile), cfg: cfg, prov: prov}
	p.useConfig(cfg)
	mounted, err := p.prepareCertificate()
	if err != nil {
		t.Fatalf("prepareCertificate: %v", err)
	}
	if mounted {
		t.Error("certificate endpoints mounted without metadata")
	}
	if c, err := prov.tlsCertificate(nil); err != nil || c == nil {
		t.Errorf("tlsCertificate = %v, %v; want the certificate served anyway", c, err)
	}
	if info := prov.Certificate(); info.Subject != "" {
		t.Errorf("Certificate() = %+v, want no metadata", info)
	}
}

func TestStartupExit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                                string
		serving                             int
		api, allDisabled, mgmtEnabled, exit bool
	}{
		{name: "a device serves", serving: 1, mgmtEnabled: true},
		{name: "only the API serves", api: true, mgmtEnabled: true},
		{name: "all disabled", allDisabled: true, exit: true},
		{name: "API failed to start", mgmtEnabled: true, exit: true},
		{name: "management disabled", exit: true},
	}
	for _, tt := range tests {
		err := startupExit(tt.serving, tt.api, tt.allDisabled, tt.mgmtEnabled)
		if (err != nil) != tt.exit {
			t.Errorf("%s: got %v, want exit %v", tt.name, err, tt.exit)
		}
	}
}

func TestRunExit(t *testing.T) {
	t.Parallel()
	pumpErr := errors.New("device gone")
	tests := []struct {
		name          string
		alive         int
		api, lost     bool
		lastPumpErr   error
		exit, wantErr bool
	}{
		{name: "a pump is alive", alive: 1},
		{name: "the API serves", api: true},
		{name: "last pump ended cleanly", exit: true},
		{name: "last pump failed", lastPumpErr: pumpErr, exit: true, wantErr: true},
		{name: "API lost, a pump alive", alive: 1, lost: true},
		{name: "API lost after the last pump", lost: true, exit: true, wantErr: true},
		{name: "API lost after a pump failed", lost: true, lastPumpErr: pumpErr, exit: true, wantErr: true},
	}
	for _, tt := range tests {
		exit, err := runExit(tt.alive, tt.api, tt.lastPumpErr, tt.lost)
		if exit != tt.exit || (err != nil) != tt.wantErr {
			t.Errorf("%s: got exit %v, err %v; want exit %v, error %v", tt.name, exit, err, tt.exit, tt.wantErr)
		}
		if tt.lastPumpErr != nil && err != nil && !errors.Is(err, tt.lastPumpErr) {
			t.Errorf("%s: %v does not wrap the last pump error", tt.name, err)
		}
	}
}
