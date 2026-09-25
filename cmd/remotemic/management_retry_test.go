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
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// fakeMgmt returns a handle that looks like a running API at addr, whose Wait
// returns once stop is closed.
func fakeMgmt(addr string, stop chan struct{}) *mgmt {
	return &mgmt{addr: addr, certPath: "cert.pem", done: stop}
}

func TestRetryManagementBacksOffThenDelivers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		delays := []time.Duration{time.Second, 2 * time.Second}
		start := time.Now()
		var at []time.Duration
		stop := make(chan struct{})
		attempt := func() (*mgmt, error) {
			at = append(at, time.Since(start))
			if len(at) < 3 {
				return nil, errors.New("still broken")
			}
			return fakeMgmt(testMgmtAddr, stop), nil
		}
		h := retryManagement(t.Context(), attempt, delays, errors.New("broken"))

		ep := <-h.Up()
		if ep.addr != testMgmtAddr || ep.certPath != "cert.pem" {
			t.Errorf("got endpoint %+v, want the recovered API's address and certificate", ep)
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
		close(stop)
		<-waited
	})
}

// TestRetryManagementLogsChangesOnly pins the log bound: a failure repeating the
// previous message is not logged again, a different one is, and the recovery
// is. Not parallel: it captures the process logger.
func TestRetryManagementLogsChangesOnly(t *testing.T) {
	out := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		errs := []error{errors.New("broken"), errors.New("broken"), errors.New("other")}
		stop := make(chan struct{})
		close(stop)
		n := 0
		attempt := func() (*mgmt, error) {
			if n < len(errs) {
				n++
				return nil, errs[n-1]
			}
			return fakeMgmt(testMgmtAddr, stop), nil
		}
		h := retryManagement(t.Context(), attempt, []time.Duration{time.Second}, errors.New("broken"))
		<-h.Up()
		h.Wait()
	})
	if got := strings.Count(out.String(), "management API still disabled"); got != 1 {
		t.Errorf("logged %d failure lines, want 1 (only the changed message); log:\n%s", got, out)
	}
	if !strings.Contains(out.String(), "management API recovered after 4 background attempt(s)") {
		t.Errorf("log lacks the recovery line; log:\n%s", out)
	}
}

// TestPendingRecovery pins the non-blocking take run() uses before deciding it
// has no diagnostic surface: an endpoint a retry delivered but the run loop has
// not received is returned once, and a nil or idle handle reports nothing.
func TestPendingRecovery(t *testing.T) {
	t.Parallel()
	if _, ok := pendingRecovery(nil); ok {
		t.Error("a nil handle reported a pending recovery")
	}
	up := make(chan mgmtEndpoint, 1)
	h := &mgmt{up: up}
	if _, ok := pendingRecovery(h); ok {
		t.Error("an idle retry handle reported a pending recovery")
	}
	up <- mgmtEndpoint{addr: testMgmtAddr, certPath: testCertFile}
	ep, ok := pendingRecovery(h)
	if !ok || ep.addr != testMgmtAddr {
		t.Fatalf("got %+v, %v; want the buffered endpoint", ep, ok)
	}
	if _, ok := pendingRecovery(h); ok {
		t.Error("the endpoint was reported twice")
	}
}

// TestAdoptPending pins that a pending endpoint reaches adopt exactly once and
// that nothing pending calls nothing.
func TestAdoptPending(t *testing.T) {
	t.Parallel()
	up := make(chan mgmtEndpoint, 1)
	h := &mgmt{up: up}
	var got []mgmtEndpoint
	adopt := func(ep mgmtEndpoint) { got = append(got, ep) }
	adoptPending(h, adopt)
	adoptPending(nil, adopt)
	if len(got) != 0 {
		t.Fatalf("adopted %v with nothing pending", got)
	}
	up <- mgmtEndpoint{addr: testMgmtAddr}
	adoptPending(h, adopt)
	adoptPending(h, adopt)
	if len(got) != 1 || got[0].addr != testMgmtAddr {
		t.Errorf("adopted %v, want the one pending endpoint once", got)
	}
}

func TestRetryManagementStopsOnCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		attempt := func() (*mgmt, error) {
			attempts++
			return nil, errors.New("broken")
		}
		h := retryManagement(ctx, attempt, []time.Duration{time.Minute}, errors.New("broken"))
		time.Sleep(150 * time.Second)
		cancel()
		h.Wait()
		if attempts != 2 {
			t.Errorf("got %d attempts in 150 s at one per minute, want 2", attempts)
		}
		select {
		case <-h.Up():
			t.Error("a retry that never succeeded must not deliver an endpoint")
		default:
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

	var ep mgmtEndpoint
	select {
	case ep = <-h.Up():
	case <-time.After(10 * time.Second):
		t.Fatal("the background retry did not bring the API up after the fault cleared")
	}
	if ep.certPath != filepath.Join(certDir, testCertFile) {
		t.Errorf("got certificate path %q, want the one under cert_dir", ep.certPath)
	}
	if leaf := dialLeaf(t, ep.addr); leaf == nil {
		t.Fatal("the recovered API did not present a certificate")
	}
	if act := center.Active(); len(act) != 0 {
		t.Errorf("active = %+v, want the condition cleared once the API serves", act)
	}
	cancel()
	h.Wait()
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
	h.Wait()

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
	h2.Wait()
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
	h.Wait()
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
		h.Wait()
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
		h.Wait()
		t.Fatal("a config file that no longer loads must fail the attempt")
	}
}
