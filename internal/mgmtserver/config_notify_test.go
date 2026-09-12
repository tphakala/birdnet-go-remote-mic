package mgmtserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// testAuthToken2 is a second valid token (16 unreserved chars), for a rotation
// that differs from testAuthToken.
const testAuthToken2 = "z4Wq8nB1cV6xM0jH"

// findNotification returns the first entry in snap matching category, kind and
// title, or nil. The emitter tests wire a real *notify.Center as the notifier and
// read it back, so they exercise the same idempotency the production wiring does.
func findNotification(snap notify.Snapshot, category notify.Category, kind notify.Kind, title string) *notify.Notification {
	for i := range snap.Notifications {
		n := &snap.Notifications[i]
		if n.Category == category && n.Kind == kind && n.Title == title {
			return n
		}
	}
	return nil
}

// patchDiscovery sends a discovery-only patch, which reaches the reloader without
// touching the auth token.
func patchDiscovery(t *testing.T, s *Server, enabled bool) {
	t.Helper()
	if _, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{
		Body: &mgmtapi.ConfigPatch{Discovery: &mgmtapi.DiscoverySettings{Enabled: ptr(enabled)}},
	}); err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
}

func TestPatchConfigReloadFailureEmitsReloadFailedAndRestartRequired(t *testing.T) {
	store, _ := tokenStore(t, "")
	center := notify.NewCenter()
	failing := func(context.Context, config.Config) error { return errors.New("shutting down") }
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(failing), WithNotifier(center))

	patchDiscovery(t, s, false)

	snap := center.Snapshot()
	if findNotification(snap, notify.CategoryConfig, notify.KindEvent, "Config reload failed") == nil {
		t.Error("a failed reload must publish a config reload-failed event")
	}
	restart := findNotification(snap, notify.CategoryConfig, notify.KindOnset, "Restart required")
	if restart == nil {
		t.Fatal("a failed reload must raise a restart-required onset")
	}
	if restart.Severity != notify.SeverityWarning {
		t.Errorf("restart-required severity = %q, want warning", restart.Severity)
	}
	if restart.Key != configRestartKey {
		t.Errorf("restart-required key = %q, want %q", restart.Key, configRestartKey)
	}
	if got := center.Active(); len(got) != 1 || got[0].Key != configRestartKey {
		t.Errorf("active = %+v, want the restart condition only", got)
	}
}

func TestPatchConfigNoReloaderEmitsRestartRequiredOnly(t *testing.T) {
	store, _ := tokenStore(t, "")
	center := notify.NewCenter()
	// No reloader: the change is persisted but cannot be hot-applied, so a restart
	// is required, but there is no reload failure to report.
	s := New(&fakeProvider{}, WithConfigStore(store), WithNotifier(center))

	patchDiscovery(t, s, false)

	snap := center.Snapshot()
	if findNotification(snap, notify.CategoryConfig, notify.KindEvent, "Config reload failed") != nil {
		t.Error("without a reloader there is no reload failure to report")
	}
	if findNotification(snap, notify.CategoryConfig, notify.KindOnset, "Restart required") == nil {
		t.Error("a persisted change with no reloader must raise a restart-required onset")
	}
}

func TestPatchConfigReloadCancellationEmitsNothing(t *testing.T) {
	store, _ := tokenStore(t, "")
	center := notify.NewCenter()
	// A canceled request context is an indeterminate outcome (the PATCH client
	// disconnected): the run loop may have applied the change, so no reload-failed
	// event or restart-required condition should be raised.
	canceled := func(context.Context, config.Config) error { return context.Canceled }
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(canceled), WithNotifier(center))

	patchDiscovery(t, s, false)

	snap := center.Snapshot()
	if findNotification(snap, notify.CategoryConfig, notify.KindEvent, "Config reload failed") != nil {
		t.Error("a canceled request must not publish a reload-failed event")
	}
	if findNotification(snap, notify.CategoryConfig, notify.KindOnset, "Restart required") != nil {
		t.Error("a canceled request must not raise a restart-required onset")
	}
	if got := len(center.Active()); got != 0 {
		t.Errorf("active conditions = %d, want 0", got)
	}
}

func TestPatchConfigSuccessfulReloadResolvesRestartRequired(t *testing.T) {
	store, _ := tokenStore(t, "")
	center := notify.NewCenter()
	failNext := true
	reloader := func(context.Context, config.Config) error {
		if failNext {
			return errors.New("shutting down")
		}
		return nil
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(reloader), WithNotifier(center))

	// First patch cannot hot-apply: the restart-required condition is raised.
	patchDiscovery(t, s, false)
	if findNotification(center.Snapshot(), notify.CategoryConfig, notify.KindOnset, "Restart required") == nil {
		t.Fatal("a failed reload must raise a restart-required onset")
	}
	if got := len(center.Active()); got != 1 {
		t.Fatalf("active conditions = %d, want 1", got)
	}

	// A later patch hot-applies successfully, so the running pipeline matches the
	// persisted config again and the restart-required condition is retracted.
	failNext = false
	patchDiscovery(t, s, true)
	if got := len(center.Active()); got != 0 {
		t.Fatalf("after a successful reload, active = %d, want the restart condition resolved", got)
	}
	ns := center.Snapshot().Notifications
	if len(ns) == 0 || ns[len(ns)-1].Kind != notify.KindClear {
		t.Errorf("expected the last entry to clear the restart condition, got %+v", ns)
	}
}

func TestPatchConfigSuccessfulReloadEmitsNoRestart(t *testing.T) {
	store, _ := tokenStore(t, "")
	center := notify.NewCenter()
	ok := func(context.Context, config.Config) error { return nil }
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(ok), WithNotifier(center))

	patchDiscovery(t, s, false)

	snap := center.Snapshot()
	if findNotification(snap, notify.CategoryConfig, notify.KindOnset, "Restart required") != nil {
		t.Error("a successful hot reload must not raise a restart-required onset")
	}
	if findNotification(snap, notify.CategoryConfig, notify.KindEvent, "Config reload failed") != nil {
		t.Error("a successful hot reload must not report a reload failure")
	}
	if got := len(center.Active()); got != 0 {
		t.Errorf("active conditions = %d, want 0", got)
	}
}

func TestPatchConfigAuthEnableEmitsAuthChanged(t *testing.T) {
	store, _ := tokenStore(t, "")
	guard := auth.NewGuard("")
	center := notify.NewCenter()
	ok := func(context.Context, config.Config) error { return nil }
	s := New(&fakeProvider{}, WithConfigStore(store), WithAuth(guard), WithReloader(ok), WithNotifier(center))

	patchAuth(t, s, ptr(testAuthToken))

	n := findNotification(center.Snapshot(), notify.CategoryConfig, notify.KindEvent, "Access control changed")
	if n == nil {
		t.Fatal("enabling the token must publish an auth-changed event")
	}
	if n.Severity != notify.SeverityInfo {
		t.Errorf("auth-changed severity = %q, want info", n.Severity)
	}
	if !strings.Contains(n.Message, "enabled") {
		t.Errorf("auth-changed message = %q, want it to mention the token was enabled", n.Message)
	}
}

func TestPatchConfigAuthRotateEmitsAuthChanged(t *testing.T) {
	store, _ := tokenStore(t, testAuthToken)
	guard := auth.NewGuard(testAuthToken)
	center := notify.NewCenter()
	ok := func(context.Context, config.Config) error { return nil }
	s := New(&fakeProvider{}, WithConfigStore(store), WithAuth(guard), WithReloader(ok), WithNotifier(center))

	patchAuth(t, s, ptr(testAuthToken2))

	n := findNotification(center.Snapshot(), notify.CategoryConfig, notify.KindEvent, "Access control changed")
	if n == nil {
		t.Fatal("rotating the token must publish an auth-changed event")
	}
	if !strings.Contains(n.Message, "rotated") {
		t.Errorf("auth-changed message = %q, want it to mention the token was rotated", n.Message)
	}
}

func TestPatchConfigAuthDisableEmitsAuthChanged(t *testing.T) {
	store, _ := tokenStore(t, testAuthToken)
	guard := auth.NewGuard(testAuthToken)
	center := notify.NewCenter()
	ok := func(context.Context, config.Config) error { return nil }
	s := New(&fakeProvider{}, WithConfigStore(store), WithAuth(guard), WithReloader(ok), WithNotifier(center))

	patchAuth(t, s, ptr(""))

	n := findNotification(center.Snapshot(), notify.CategoryConfig, notify.KindEvent, "Access control changed")
	if n == nil {
		t.Fatal("disabling the token must publish an auth-changed event")
	}
	if !strings.Contains(n.Message, "disabled") {
		t.Errorf("auth-changed message = %q, want it to mention the token was disabled", n.Message)
	}
}

func TestPatchConfigUnchangedTokenEmitsNoAuthChanged(t *testing.T) {
	store, _ := tokenStore(t, testAuthToken)
	guard := auth.NewGuard(testAuthToken)
	center := notify.NewCenter()
	ok := func(context.Context, config.Config) error { return nil }
	s := New(&fakeProvider{}, WithConfigStore(store), WithAuth(guard), WithReloader(ok), WithNotifier(center))

	// Re-applying the same token does not advance the guard generation.
	genBefore := guard.Generation()
	patchAuth(t, s, ptr(testAuthToken))

	if got := guard.Generation(); got != genBefore {
		t.Errorf("generation advanced from %d to %d on an unchanged token", genBefore, got)
	}
	if findNotification(center.Snapshot(), notify.CategoryConfig, notify.KindEvent, "Access control changed") != nil {
		t.Error("re-applying the same token must not publish an auth-changed event")
	}
}

func TestPostSystemRestartEmitsRestartRequested(t *testing.T) {
	center := notify.NewCenter()
	s := New(&fakeProvider{}, WithRestart(func() {}), WithNotifier(center))

	resp, err := s.PostSystemRestart(context.Background(), mgmtapi.PostSystemRestartRequestObject{})
	if err != nil {
		t.Fatalf("PostSystemRestart: %v", err)
	}
	if _, ok := resp.(mgmtapi.PostSystemRestart202JSONResponse); !ok {
		t.Fatalf("response type %T, want 202", resp)
	}
	if findNotification(center.Snapshot(), notify.CategorySystem, notify.KindEvent, "Restart requested") == nil {
		t.Error("a restart request must publish a restart-requested event")
	}
}
