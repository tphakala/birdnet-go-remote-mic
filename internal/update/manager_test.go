package update

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// fakeFetch serves a scripted sequence of check outcomes and counts calls.
type fakeFetch struct {
	mu    sync.Mutex
	calls int
	rel   *Release
	err   error
}

func (f *fakeFetch) fetch(context.Context) (*Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rel, f.err
}

func (f *fakeFetch) set(rel *Release, err error) {
	f.mu.Lock()
	f.rel, f.err = rel, err
	f.mu.Unlock()
}

func (f *fakeFetch) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func fakeRelease(version string) *Release {
	return &Release{Manifest: &releasemanifest.Manifest{
		Version:  version,
		NotesURL: "https://github.com/" + releasemanifest.Repository + "/releases/tag/" + version,
	}}
}

// logSink collects log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func noJitter(time.Duration) time.Duration { return 0 }

func on() *monitor.Settings  { return &monitor.Settings{UpdateCheck: true} }
func off() *monitor.Settings { return &monitor.Settings{UpdateCheck: false} }

func activeKeys(c *notify.Center) map[string]notify.Notification {
	active := c.Active()
	out := make(map[string]notify.Notification, len(active))
	for i := range active {
		out[active[i].Key] = active[i]
	}
	return out
}

// TestManagerChecksOnlyWhenEnabled pins that with checks off nothing is
// fetched, however long the appliance runs, and that turning them on starts
// the first check after the startup delay.
func TestManagerChecksOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetch{rel: fakeRelease(vOld)}
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: (&logSink{}).logf})
		go m.Run(t.Context())
		m.Apply(off())
		time.Sleep(72 * time.Hour)
		synctest.Wait()
		if n := f.count(); n != 0 {
			t.Fatalf("checks off: %d fetches, want 0", n)
		}
		m.Apply(on())
		time.Sleep(firstCheckDelay - time.Second)
		synctest.Wait()
		if n := f.count(); n != 0 {
			t.Fatalf("before the first delay: %d fetches, want 0", n)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := f.count(); n != 1 {
			t.Fatalf("after the first delay: %d fetches, want 1", n)
		}
		time.Sleep(checkInterval)
		synctest.Wait()
		if n := f.count(); n != 2 {
			t.Fatalf("a day later: %d fetches, want 2", n)
		}
		m.Apply(off())
		time.Sleep(72 * time.Hour)
		synctest.Wait()
		if n := f.count(); n != 2 {
			t.Fatalf("after turning checks off: %d fetches, want 2", n)
		}
	})
}

// TestManagerFailureIsQuiet pins that repeated failures of one kind log once,
// back off, raise no notification, and that recovery is logged.
func TestManagerFailureIsQuiet(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetch{err: &StatusError{URL: "u", Code: 503}}
		logs := &logSink{}
		c := notify.NewCenter()
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: logs.logf, Publisher: c})
		go m.Run(t.Context())
		m.Apply(on())
		// First check at 3 min, retries after 1 h, 2 h, 4 h, 8 h, 16 h, 24 h.
		time.Sleep(firstCheckDelay + (1+2+4+8+16+24)*time.Hour + time.Second)
		synctest.Wait()
		if n := f.count(); n != 7 {
			t.Errorf("got %d fetches, want 7 with backoff", n)
		}
		if n := logs.count("check failed"); n != 1 {
			t.Errorf("logged %d failures, want 1", n)
		}
		if snap := c.Snapshot(); len(snap.Notifications) != 0 {
			t.Errorf("failures raised notifications: %+v", snap.Notifications)
		}
		if st := m.Status(); st.LastError == "" || st.LastCheck.IsZero() {
			t.Errorf("status %+v does not record the failure", st)
		}
		f.set(nil, errors.New("dial tcp: no route to host"))
		time.Sleep(24*time.Hour + time.Second)
		synctest.Wait()
		if n := logs.count("check failed"); n != 2 {
			t.Errorf("a new kind of failure: logged %d failures, want 2", n)
		}
		f.set(fakeRelease(vOld), nil)
		time.Sleep(24*time.Hour + time.Second)
		synctest.Wait()
		if n := logs.count("succeed again"); n != 1 {
			t.Errorf("recovery logged %d times, want 1", n)
		}
		if st := m.Status(); st.LastError != "" {
			t.Errorf("LastError %q after a success", st.LastError)
		}
	})
}

// TestManagerAvailableNotification pins the available condition: raised once
// for a newer release, rewritten in place for a newer one still, and cleared
// when checks are turned off.
func TestManagerAvailableNotification(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{rel: fakeRelease(vNew)}
	c := notify.NewCenter()
	m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Publisher: c, Logf: (&logSink{}).logf})
	m.Apply(on())
	if _, err := m.CheckNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	n, ok := activeKeys(c)[AvailableKey]
	if !ok || !strings.Contains(n.Title, vNew) || !strings.Contains(n.Message, "releases/tag/v0.3.0") {
		t.Fatalf("available condition %+v, %t", n, ok)
	}
	st := m.Status()
	if !st.Available || st.Latest != vNew || st.NotesURL == "" {
		t.Errorf("status %+v", st)
	}

	f.set(fakeRelease("v0.4.0"), nil)
	m.check(t.Context())
	if n := activeKeys(c)[AvailableKey]; !strings.Contains(n.Title, "v0.4.0") {
		t.Errorf("condition not rewritten for v0.4.0: %+v", n)
	}
	if got := len(c.Snapshot().Notifications); got != 1 {
		t.Errorf("got %d entries, want the one condition updated in place", got)
	}

	m.Apply(off())
	if _, ok := activeKeys(c)[AvailableKey]; ok {
		t.Error("condition still active after checks were turned off")
	}
}

func TestManagerNoUpdateForSameVersion(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{rel: fakeRelease(vOld)}
	c := notify.NewCenter()
	m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Publisher: c, Logf: (&logSink{}).logf})
	m.Apply(on())
	if _, err := m.CheckNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.Available || st.Latest != vOld {
		t.Errorf("status %+v", st)
	}
	if len(c.Active()) != 0 {
		t.Errorf("conditions %+v, want none", c.Active())
	}
}

func TestManagerCheckNow(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{rel: fakeRelease(vOld)}
	m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Logf: (&logSink{}).logf})
	if _, err := m.CheckNow(t.Context()); !errors.Is(err, ErrChecksDisabled) {
		t.Errorf("checks off: got %v, want ErrChecksDisabled", err)
	}
	m.Apply(on())
	for range 3 {
		if _, err := m.CheckNow(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(); n != 1 {
		t.Errorf("three quick manual checks made %d fetches, want 1", n)
	}

	dev := NewManager(t.Context(), &Config{Running: "dev", Fetch: f.fetch, Logf: (&logSink{}).logf})
	dev.Apply(on())
	if _, err := dev.CheckNow(t.Context()); !errors.Is(err, ErrNotRelease) {
		t.Errorf("dev build: got %v, want ErrNotRelease", err)
	}
	dev.Run(t.Context()) // returns at once for a dev build
}

func TestNextCheck(t *testing.T) {
	t.Parallel()
	for failures, want := range map[int]time.Duration{
		0: checkInterval - checkJitter, 1: time.Hour, 2: 2 * time.Hour, 5: 16 * time.Hour, 6: checkInterval, 40: checkInterval,
	} {
		if got := nextCheck(failures, noJitter); got != want {
			t.Errorf("nextCheck(%d) = %v, want %v", failures, got, want)
		}
	}
}

// applyManager is a Manager with an available v0.3.0 and a stage function
// that writes the request file as the real stager would.
func applyManager(t *testing.T, stage func(ctx context.Context, rel *Release) error) (*Manager, *notify.Center, string) {
	t.Helper()
	dir := t.TempDir()
	if stage == nil {
		stage = func(context.Context, *Release) error {
			return os.WriteFile(filepath.Join(dir, RequestFile), []byte(`{"version":"v0.3.0"}`), 0o644)
		}
	}
	c := notify.NewCenter()
	m := NewManager(t.Context(), &Config{
		Running:   vOld,
		Fetch:     (&fakeFetch{rel: fakeRelease(vNew)}).fetch,
		Stage:     stage,
		Dir:       dir,
		Install:   Install{Method: MethodService, CanApply: true},
		Publisher: c,
		Logf:      (&logSink{}).logf,
	})
	m.Apply(on())
	if _, err := m.CheckNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	return m, c, dir
}

func titles(c *notify.Center) []string {
	ns := c.Snapshot().Notifications
	out := make([]string, 0, len(ns))
	for i := range ns {
		out = append(out, ns[i].Title)
	}
	return out
}

func TestStartApplyRefuses(t *testing.T) {
	t.Parallel()
	m := NewManager(t.Context(), &Config{Running: vOld, Install: Install{Method: MethodDeb}, Logf: (&logSink{}).logf})
	if _, err := m.StartApply(); !errors.Is(err, ErrCannotApply) {
		t.Errorf("deb install: got %v, want ErrCannotApply", err)
	}
	m = NewManager(t.Context(), &Config{Running: vOld, Install: Install{Method: MethodService, CanApply: true}, Logf: (&logSink{}).logf})
	if _, err := m.StartApply(); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("nothing available: got %v, want ErrNoUpdate", err)
	}
}

// TestStartApplyUpdaterRefuses pins that a failed result from the root
// updater ends the attempt as failed and is reported.
func TestStartApplyUpdaterRefuses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, c, dir := applyManager(t, nil)
		st, err := m.StartApply()
		if err != nil || st.Phase != PhaseDownloading {
			t.Fatalf("StartApply: %+v, %v", st, err)
		}
		if _, err := m.StartApply(); !errors.Is(err, ErrBusy) {
			t.Errorf("second StartApply: got %v, want ErrBusy", err)
		}
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseInstalling {
			t.Fatalf("phase %q after staging, want installing", st.Phase)
		}
		// The updater takes the request and refuses.
		_ = os.Remove(filepath.Join(dir, RequestFile))
		if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(`{"outcome":"failed","from":"v0.2.0","to":"v0.3.0","reason":"not newer"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(resultPoll + time.Millisecond)
		synctest.Wait()
		st = m.Status()
		if st.Phase != PhaseFailed || st.PhaseMessage != wantNotNewer {
			t.Errorf("status %+v, want failed: not newer", st)
		}
		if got := titles(c); !slices.Contains(got, "Installing v0.3.0") || !slices.Contains(got, "Update failed") {
			t.Errorf("notifications %q", got)
		}
	})
}

// TestStartApplyUpdaterMissing pins that a request nobody takes is withdrawn
// and the attempt fails with a hint.
func TestStartApplyUpdaterMissing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, dir := applyManager(t, nil)
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(updaterStartTimeout + 2*resultPoll)
		synctest.Wait()
		st := m.Status()
		if st.Phase != PhaseFailed || !strings.Contains(st.PhaseMessage, "service install") {
			t.Errorf("status %+v", st)
		}
		if _, err := os.Stat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("request not withdrawn: %v", err)
		}
	})
}

func TestStartApplyStageFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, _ := applyManager(t, func(context.Context, *Release) error { return errors.New("sha256 mismatch") })
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseFailed || st.PhaseMessage != "sha256 mismatch" {
			t.Errorf("status %+v", st)
		}
		// A failed attempt can be retried.
		if _, err := m.StartApply(); err != nil {
			t.Errorf("retry after a failure: %v", err)
		}
		synctest.Wait()
	})
}

// TestStartApplyRecoversPanic pins that a panic while staging fails the
// attempt instead of ending the process.
func TestStartApplyRecoversPanic(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, _ := applyManager(t, func(context.Context, *Release) error { panic("boom") })
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseFailed || !strings.Contains(st.PhaseMessage, "boom") {
			t.Errorf("status %+v", st)
		}
	})
}

func TestFailureCause(t *testing.T) {
	t.Parallel()
	tests := map[string]error{
		"http 503":   &StatusError{URL: "u", Code: 503},
		"timeout":    context.DeadlineExceeded,
		"network":    &net.OpError{Op: "dial", Err: errors.New("no route to host")},
		"no release": ErrNoRelease,
		"signature":  releasemanifest.ErrBadSignature,
		// Anything else is its own cause, named by its message.
		"wrapped: other": errors.New("other"),
	}
	for want, err := range tests {
		if got := failureCause(fmt.Errorf("wrapped: %w", err)); got != want {
			t.Errorf("failureCause(%v) = %q, want %q", err, got, want)
		}
	}
}

// TestStartApplyNoResult pins that an attempt the updater took but never
// reported on (and that did not restart this process) ends as failed.
func TestStartApplyNoResult(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, dir := applyManager(t, nil)
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		_ = os.Remove(filepath.Join(dir, RequestFile)) // the updater took it
		time.Sleep(updaterStartTimeout + DefaultHealthTimeout + 2*time.Minute)
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseFailed || !strings.Contains(st.PhaseMessage, "no result") {
			t.Errorf("status %+v", st)
		}
	})
}

// TestStartApplySeesRollback pins that a rolled-back result seen by the
// running appliance is reported and leaves it idle.
func TestStartApplySeesRollback(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, c, dir := applyManager(t, nil)
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(`{"outcome":"rolled_back","from":"v0.2.0","to":"v0.3.0","reason":"timeout"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(resultPoll + time.Millisecond)
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseIdle {
			t.Errorf("phase %q, want idle", st.Phase)
		}
		if got := titles(c); !slices.Contains(got, "Update rolled back") {
			t.Errorf("notifications %q", got)
		}
	})
}
