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
	"sync/atomic"
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

// devBuildVersion is a version that names no release, so a Manager never
// checks.
const devBuildVersion = "dev"

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

// TestManagerReportsNextCheck pins that the status names when the check timer
// fires while checks are on, moves it to the next interval after each check,
// and reports none the moment checks are turned off (before the loop has run
// to clear its timer) or on a build that never checks.
func TestManagerReportsNextCheck(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetch{rel: fakeRelease(vOld)}
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: (&logSink{}).logf})
		go m.Run(t.Context())
		synctest.Wait()
		if got := m.Status().NextCheck; !got.IsZero() {
			t.Fatalf("checks off: NextCheck %v, want none", got)
		}
		m.Apply(on())
		synctest.Wait()
		armed := time.Now()
		if got, want := m.Status().NextCheck, armed.Add(firstCheckDelay); !got.Equal(want) {
			t.Fatalf("after turning checks on: NextCheck %v, want %v", got, want)
		}
		time.Sleep(time.Minute)
		if got, want := m.Status().NextCheck, armed.Add(firstCheckDelay); !got.Equal(want) {
			t.Errorf("a minute later: NextCheck %v, want the same deadline %v", got, want)
		}
		time.Sleep(firstCheckDelay)
		synctest.Wait()
		if n := f.count(); n != 1 {
			t.Fatalf("%d fetches after the first delay, want 1", n)
		}
		// The check fired at armed+firstCheckDelay, and the next one is due a
		// jitter-free interval after it.
		if got, want := m.Status().NextCheck, armed.Add(firstCheckDelay).Add(checkInterval-checkJitter); !got.Equal(want) {
			t.Errorf("after the first check: NextCheck %v, want %v", got, want)
		}
		m.Apply(off())
		// No Wait: the loop has not yet run to clear its timer.
		if st := m.Status(); st.CheckEnabled || !st.NextCheck.IsZero() {
			t.Errorf("just after turning checks off: %+v, want checks off and no NextCheck", st)
		}
		synctest.Wait()
		if got := m.Status().NextCheck; !got.IsZero() {
			t.Errorf("checks off: NextCheck %v, want none", got)
		}
	})
	dev := NewManager(t.Context(), &Config{Running: devBuildVersion, Logf: (&logSink{}).logf})
	dev.Apply(on())
	dev.Run(t.Context()) // returns at once for a dev build
	if got := dev.Status().NextCheck; !got.IsZero() {
		t.Errorf("dev build: NextCheck %v, want none", got)
	}
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
	m.check(t.Context(), false)
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
	// First a newer release raises the condition, then the release seen is
	// no newer (for example the newer one was withdrawn): it must clear.
	f := &fakeFetch{rel: fakeRelease(vNew)}
	c := notify.NewCenter()
	m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Publisher: c, Logf: (&logSink{}).logf})
	m.Apply(on())
	m.check(t.Context(), false)
	if len(c.Active()) != 1 {
		t.Fatalf("precondition: conditions %+v, want the available one", c.Active())
	}
	f.set(fakeRelease(vOld), nil)
	m.check(t.Context(), false)
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
	for r, want := range map[checkResult]time.Duration{
		{}:                           checkInterval - checkJitter,
		{pending: true}:              time.Hour,
		{failures: 1}:                time.Hour,
		{failures: 2}:                2 * time.Hour,
		{failures: 5}:                16 * time.Hour,
		{failures: 6}:                checkInterval,
		{failures: 40}:               checkInterval,
		{failures: 3, pending: true}: time.Hour,
	} {
		if got := nextCheck(r, noJitter); got != want {
			t.Errorf("nextCheck(%+v) = %v, want %v", r, got, want)
		}
	}
}

// applyManager is a Manager with an available v0.3.0 and a stage function
// that writes the request file as the real stager would.
func applyManager(t *testing.T, stage func(ctx context.Context, rel *Release) error) (*Manager, *notify.Center, string) {
	t.Helper()
	return applyManagerFor(t, stage, "")
}

// applyManagerFor is applyManager for a service whose binary is serviceBin.
func applyManagerFor(t *testing.T, stage func(ctx context.Context, rel *Release) error, serviceBin string) (*Manager, *notify.Center, string) {
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
		Install:   Install{Method: MethodService, CanApply: true, ServiceBin: serviceBin},
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
	if _, err := m.StartApply(); !errors.Is(err, ErrChecksDisabled) {
		t.Errorf("checks off: got %v, want ErrChecksDisabled", err)
	}
	m.Apply(on())
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
		// The updater refuses without managing to claim the request, so it
		// is still there; the appliance clears it with the failure.
		if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(`{"outcome":"failed","from":"v0.2.0","to":"v0.3.0","installed":"v0.2.0","reason":"not newer"}`), 0o644); err != nil {
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
		if exists(filepath.Join(dir, RequestFile)) {
			t.Error("the unclaimed request was left behind after the failure")
		}
	})
}

// TestStartApplyBusyWithOwnRequest pins that pressing again while this
// process's own attempt holds a live request on disk is ErrBusy, not an
// earlier attempt. The stager writes the request last, so that is mostly
// while the updater is awaited (installing), and briefly before the phase
// moves on (downloading).
func TestStartApplyBusyWithOwnRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase Phase
		block bool // hold staging open after the request is written
	}{
		{PhaseInstalling, false},
		{PhaseDownloading, true},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var dir string
				m, _, dir := applyManager(t, func(ctx context.Context, _ *Release) error {
					p := filepath.Join(dir, RequestFile)
					if err := os.WriteFile(p, []byte(`{"version":"v0.3.0"}`), 0o644); err != nil {
						return err
					}
					// The bubble's clock is not the file system's; date the
					// request by it so the on-disk check sees a live attempt.
					now := time.Now()
					if err := os.Chtimes(p, now, now); err != nil {
						return err
					}
					if tc.block {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				})
				if _, err := m.StartApply(); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if st := m.Status(); st.Phase != tc.phase {
					t.Fatalf("phase %q before the second press, want %q", st.Phase, tc.phase)
				}
				if !inFlight(dir) {
					t.Fatal("the request staging wrote is not a live attempt on disk")
				}
				_, err := m.StartApply()
				if !errors.Is(err, ErrBusy) || errors.Is(err, errEarlierAttempt) {
					t.Errorf("second StartApply: got %v, want ErrBusy for this process's own attempt", err)
				}
			})
		})
	}
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
		// Not withdrawn before the timeout.
		time.Sleep(updaterStartTimeout - resultPoll)
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseInstalling || !exists(filepath.Join(dir, RequestFile)) {
			t.Fatalf("before the timeout: status %+v, request present %t", st, exists(filepath.Join(dir, RequestFile)))
		}
		time.Sleep(3 * resultPoll)
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

// spacedBin is a service binary path that needs shell quoting.
const spacedBin = "/opt/Remote Mic/remote-mic"

// TestStartApplyUpdaterMissingNamesTheServiceBinary pins that the hint names
// the service's own binary, quoted, and never a bare command that a package
// copy earlier on PATH could answer.
func TestStartApplyUpdaterMissingNamesTheServiceBinary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, bin, want string
	}{
		{"known", spacedBin, "re-run sudo '" + spacedBin + "' service install"},
		{"unknown", "", "re-run service install from the release binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m, _, _ := applyManagerFor(t, nil, tc.bin)
				if _, err := m.StartApply(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(updaterStartTimeout + 3*resultPoll)
				synctest.Wait()
				st := m.Status()
				if st.Phase != PhaseFailed || !strings.Contains(st.PhaseMessage, tc.want) {
					t.Errorf("status %+v, want the message to contain %q", st, tc.want)
				}
				if strings.Contains(st.PhaseMessage, "sudo remote-mic ") {
					t.Errorf("message %q names a bare command", st.PhaseMessage)
				}
			})
		})
	}
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
		"http 503":  &StatusError{URL: "u", Code: 503},
		"timeout":   context.DeadlineExceeded,
		"network":   &net.OpError{Op: "dial", Err: errors.New("no route to host")},
		"signature": releasemanifest.ErrBadSignature,
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
		// The updater took it.
		if err := os.Rename(filepath.Join(dir, RequestFile), filepath.Join(dir, TakenFile)); err != nil {
			t.Fatal(err)
		}
		deadline := updaterStartTimeout + DefaultHealthTimeout + time.Minute
		time.Sleep(deadline - resultPoll)
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseInstalling {
			t.Fatalf("before the deadline: status %+v, want installing", st)
		}
		time.Sleep(3 * resultPoll)
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
		if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(`{"outcome":"rolled_back","from":"v0.2.0","to":"v0.3.0","installed":"v0.2.0","reason":"timeout"}`), 0o644); err != nil {
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

// TestStartApplySlowUpdaterNotWithdrawn pins that a request the updater has
// claimed (renamed to TakenFile) is never withdrawn as "did not start",
// however long the updater takes before it restarts the appliance.
func TestStartApplySlowUpdaterNotWithdrawn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, dir := applyManager(t, nil)
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := os.Rename(filepath.Join(dir, RequestFile), filepath.Join(dir, TakenFile)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(updaterStartTimeout + DefaultHealthTimeout)
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseInstalling {
			t.Errorf("phase %q (%s) while the updater works, want installing", st.Phase, st.PhaseMessage)
		}
	})
}

// TestStartApplyIgnoresStaleResult pins that a result addressed to another
// version (not the running one) neither ends nor fails this attempt, and is
// left for the process it belongs to.
func TestStartApplyIgnoresStaleResult(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, dir := applyManager(t, nil)
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// The new version's result, written while this process still runs.
		status := filepath.Join(dir, StatusFile)
		if err := os.WriteFile(status, []byte(`{"outcome":"updated","from":"v0.2.0","to":"v0.3.0","installed":"v0.3.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * resultPoll)
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseInstalling {
			t.Errorf("phase %q, want installing", st.Phase)
		}
		if !exists(status) {
			t.Error("a result addressed to another version was consumed")
		}
	})
}

// TestManagerOffClearsAndRefuses pins "checks off" end to end: the release
// found is forgotten, apply is refused, a check that finishes after checks
// were turned off keeps nothing and raises nothing, and a download in
// progress is stopped.
func TestManagerOffClearsAndRefuses(t *testing.T) {
	t.Parallel()
	t.Run("found release forgotten", func(t *testing.T) {
		t.Parallel()
		m, c, _ := applyManager(t, nil)
		m.Apply(off())
		if st := m.Status(); st.Available || st.Latest != "" {
			t.Errorf("status %+v after checks off, want nothing available", st)
		}
		if _, err := m.StartApply(); !errors.Is(err, ErrChecksDisabled) {
			t.Errorf("StartApply: got %v, want ErrChecksDisabled", err)
		}
		if len(c.Active()) != 0 {
			t.Errorf("conditions %+v, want none", c.Active())
		}
	})
	t.Run("turned off during a check", func(t *testing.T) {
		t.Parallel()
		c := notify.NewCenter()
		var m *Manager
		m = NewManager(t.Context(), &Config{
			Running: vOld,
			Fetch: func(context.Context) (*Release, error) {
				m.Apply(off()) // the operator turns checks off mid fetch
				return fakeRelease(vNew), nil
			},
			Publisher: c,
			Logf:      (&logSink{}).logf,
		})
		m.Apply(on())
		m.check(t.Context(), false)
		if st := m.Status(); st.Available || st.Latest != "" || !st.LastCheck.IsZero() {
			t.Errorf("status %+v, want nothing kept", st)
		}
		if len(c.Active()) != 0 {
			t.Errorf("conditions %+v, want none", c.Active())
		}
	})
	t.Run("download stopped", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			m, _, _ := applyManager(t, func(ctx context.Context, _ *Release) error {
				<-ctx.Done() // a slow download
				return ctx.Err()
			})
			if _, err := m.StartApply(); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			m.Apply(off())
			synctest.Wait()
			if st := m.Status(); st.Phase != PhaseIdle {
				t.Errorf("status %+v, want the download stopped and the attempt idle", st)
			}
		})
	})
	t.Run("turned off right after the update starts", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// A download that completes after an hour unless cancelled.
			m, c, _ := applyManager(t, func(ctx context.Context, _ *Release) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Hour):
					return nil
				}
			})
			if _, err := m.StartApply(); err != nil {
				t.Fatal(err)
			}
			m.Apply(off()) // right away, whether or not the download has begun
			time.Sleep(2 * time.Hour)
			synctest.Wait()
			if st := m.Status(); st.Phase != PhaseIdle {
				t.Errorf("status %+v, want idle", st)
			}
			if slices.Contains(titles(c), "Update failed") {
				t.Error("turning checks off was reported as a failed update")
			}
		})
	})
	t.Run("check stopped", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			var cause error
			m := NewManager(t.Context(), &Config{
				Running: vOld,
				Fetch: func(ctx context.Context) (*Release, error) {
					<-ctx.Done() // a check stuck on a slow network
					cause = context.Cause(ctx)
					return nil, ctx.Err()
				},
				Logf: (&logSink{}).logf,
			})
			m.Apply(on())
			start := time.Now()
			var wg sync.WaitGroup
			wg.Go(func() { m.check(t.Context(), false) })
			synctest.Wait()
			m.Apply(off())
			wg.Wait()
			if waited := time.Since(start); waited != 0 {
				t.Errorf("check ran %v after checks were turned off, want it stopped at once", waited)
			}
			if !errors.Is(cause, ErrChecksDisabled) {
				t.Errorf("fetch cancelled with %v, want ErrChecksDisabled", cause)
			}
			if st := m.Status(); st.LastError != "" || !st.LastCheck.IsZero() {
				t.Errorf("status %+v, want the stopped check not recorded", st)
			}
		})
	})
	t.Run("check stopped, then checks back on", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			resume := make(chan struct{})
			m := NewManager(t.Context(), &Config{
				Running: vOld,
				Fetch: func(ctx context.Context) (*Release, error) {
					<-ctx.Done()
					<-resume // returns only after checks are back on
					return nil, ctx.Err()
				},
				Logf: (&logSink{}).logf,
			})
			m.Apply(on())
			var failures int
			var wg sync.WaitGroup
			wg.Go(func() { failures = m.check(t.Context(), false).failures })
			synctest.Wait()
			m.Apply(off())
			m.Apply(on())
			close(resume)
			wg.Wait()
			if st := m.Status(); failures != 0 || st.LastError != "" || !st.LastCheck.IsZero() {
				t.Errorf("failures %d, status %+v; want the stopped check not recorded as a failure", failures, st)
			}
		})
	})
	t.Run("check with checks off fetches nothing", func(t *testing.T) {
		t.Parallel()
		f := &fakeFetch{rel: fakeRelease(vNew)}
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Logf: (&logSink{}).logf})
		// A check that was already waiting for checkMu when checks went off.
		m.check(t.Context(), true)
		if n := f.count(); n != 0 {
			t.Errorf("a check with checks off made %d fetches, want 0", n)
		}
	})
}

// TestManagerCheckNowIgnoresCallerContext pins that a manual check runs on
// the appliance's lifetime, so a client that goes away does not turn it into
// a recorded failure.
func TestManagerCheckNowIgnoresCallerContext(t *testing.T) {
	t.Parallel()
	var sawCancelled bool
	m := NewManager(t.Context(), &Config{
		Running: vOld,
		Fetch: func(ctx context.Context) (*Release, error) {
			sawCancelled = ctx.Err() != nil
			return fakeRelease(vOld), nil
		},
		Logf: (&logSink{}).logf,
	})
	m.Apply(on())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	st, err := m.CheckNow(ctx)
	if err != nil || sawCancelled || st.LastError != "" {
		t.Errorf("CheckNow with a gone caller: %+v, %v (fetch saw a cancelled context: %t)", st, err, sawCancelled)
	}
}

// TestManagerCheckBudget pins that a check gets 20 s, under the management
// server's 30 s WriteTimeout, so a manual check's answer is still written.
func TestManagerCheckBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var budget time.Duration
		m := NewManager(t.Context(), &Config{
			Running: vOld,
			Fetch: func(ctx context.Context) (*Release, error) {
				d, _ := ctx.Deadline()
				budget = time.Until(d)
				return fakeRelease(vOld), nil
			},
			Logf: (&logSink{}).logf,
		})
		m.Apply(on())
		if _, err := m.CheckNow(t.Context()); err != nil {
			t.Fatal(err)
		}
		if budget != 20*time.Second {
			t.Errorf("check budget %v, want 20s", budget)
		}
	})
}

// TestStartApplyBusyOnDisk pins that an attempt found on disk (started before
// this process, which may be the version the updater is watching) refuses a
// new one, and an abandoned one does not.
func TestStartApplyBusyOnDisk(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _, dir := applyManager(t, nil)
		taken := filepath.Join(dir, TakenFile)
		if err := os.WriteFile(taken, []byte(`{"version":"v0.3.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(taken, now, now); err != nil {
			t.Fatal(err)
		}
		_, err := m.StartApply()
		if !errors.Is(err, ErrBusy) {
			t.Errorf("claim on disk: got %v, want ErrBusy", err)
		} else if !strings.Contains(err.Error(), "earlier update attempt may still be running") {
			t.Errorf("claim on disk: message %q does not say an earlier attempt may still be running", err)
		}
		old := now.Add(-time.Hour)
		if err := os.Chtimes(taken, old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := m.StartApply(); err != nil {
			t.Errorf("abandoned claim on disk: %v", err)
		}
		synctest.Wait()
	})
}

// TestStartApplyRefusesWhileAnInterruptedUpdateIsPending pins that the
// journal beside the service binary, which the root updater recovers from
// before it claims anything, holds a new request back.
func TestStartApplyRefusesWhileAnInterruptedUpdateIsPending(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "remote-mic")
		m, _, _ := applyManagerFor(t, nil, bin)
		journal := bin + JournalSuffix
		if err := os.WriteFile(journal, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := m.StartApply()
		switch {
		case !errors.Is(err, ErrBusy):
			t.Fatalf("journal present: got %v, want ErrBusy", err)
		case errors.Is(err, errEarlierAttempt):
			t.Errorf("journal present: reported as an earlier attempt: %v", err)
		case !strings.Contains(err.Error(), journal):
			t.Errorf("journal present: message %q does not name %s", err, journal)
		case strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "update in progress"):
			// The journal also exists through a healthy update's health wait, so
			// the message must not call it an interrupted one.
			t.Errorf("journal present: message %q does not say an update is in progress", err)
		}
		if got := m.Status().Phase; got != PhaseIdle {
			t.Errorf("phase after the refusal is %q, want %q", got, PhaseIdle)
		}
		if err := os.Remove(journal); err != nil {
			t.Fatal(err)
		}
		if _, err := m.StartApply(); err != nil {
			t.Errorf("journal removed: %v", err)
		}
		synctest.Wait()
	})
}

// TestStartApplyShutdownQuiet pins that the appliance shutting down during a
// download is not reported as a failed update.
func TestStartApplyShutdownQuiet(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		c := notify.NewCenter()
		m := NewManager(ctx, &Config{
			Running: vOld,
			Fetch:   (&fakeFetch{rel: fakeRelease(vNew)}).fetch,
			Stage: func(ctx context.Context, _ *Release) error {
				<-ctx.Done() // a slow download
				return ctx.Err()
			},
			Dir:       t.TempDir(),
			Install:   Install{Method: MethodService, CanApply: true},
			Publisher: c,
			Logf:      (&logSink{}).logf,
		})
		m.Apply(on())
		if _, err := m.CheckNow(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
		if st := m.Status(); st.Phase != PhaseIdle {
			t.Errorf("status %+v, want idle", st)
		}
		if slices.Contains(titles(c), "Update failed") {
			t.Error("shutting down was reported as a failed update")
		}
	})
}

// errNoManifest is what the Fetcher returns for a release without a manifest.
func errNoManifest(version string) error {
	return fmt.Errorf("release %s: %w", version, ErrNoManifest)
}

// TestManagerMissingManifestRetriesHourly pins that a latest release without
// its manifest is no failure: it is noted once, asked about again after a
// fixed hour (no doubling), and the notice goes when the release verifies.
func TestManagerMissingManifestRetriesHourly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetch{err: errNoManifest(vNew)}
		logs := &logSink{}
		c := notify.NewCenter()
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: logs.logf, Publisher: c})
		go m.Run(t.Context())
		m.Apply(on())
		time.Sleep(firstCheckDelay + time.Second)
		synctest.Wait()
		st := m.Status()
		if st.LastError != "" || !strings.Contains(st.LastNotice, vNew) || st.LastCheck.IsZero() {
			t.Fatalf("status %+v, want a notice naming v0.3.0 and no error", st)
		}
		if got := len(c.Snapshot().Notifications); got != 0 {
			t.Errorf("got %d notifications, want none", got)
		}
		if n := logs.count("no update information"); n != 1 {
			t.Errorf("logged %d notices, want 1", n)
		}
		if want := st.LastCheck.Add(time.Hour); !st.NextCheck.Equal(want) {
			t.Errorf("NextCheck %v, want %v", st.NextCheck, want)
		}
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if n := f.count(); n != 4 {
			t.Errorf("got %d fetches, want 4: one per hour, not doubling", n)
		}
		if n := logs.count("no update information"); n != 1 {
			t.Errorf("logged %d notices after repeats, want 1", n)
		}
		f.set(fakeRelease(vNew), nil)
		time.Sleep(time.Hour)
		synctest.Wait()
		st = m.Status()
		if st.LastNotice != "" || !st.Available || st.Latest != vNew {
			t.Errorf("status %+v, want the verified release and no notice", st)
		}
		if got, want := time.Until(st.NextCheck), checkInterval-checkJitter; got < want-time.Minute {
			t.Errorf("next check in %v, want about %v", got, want)
		}
	})
}

// TestManagerNoInformationTransitions walks sequences of check outcomes and
// pins what each leaves behind: the kept or forgotten release, the condition,
// the failure count, the log lines, and that a check records a failure or a
// notice, never both.
func TestManagerNoInformationTransitions(t *testing.T) {
	t.Parallel()
	fail := &StatusError{URL: "u", Code: 503}
	type step struct {
		rel       *Release
		err       error
		want      checkResult
		latest    string
		available bool
		lastError bool
		notice    string // substring of LastNotice, "" for none
	}
	tests := []struct {
		name  string
		steps []step
		check func(t *testing.T, m *Manager, c *notify.Center, logs *logSink)
	}{
		{
			name: "missing manifest keeps the offer",
			steps: []step{
				{rel: fakeRelease(vNew), latest: vNew, available: true},
				{err: errNoManifest("v0.4.0"), want: checkResult{pending: true}, latest: vNew, available: true, notice: "v0.4.0"},
				{rel: fakeRelease("v0.4.0"), latest: "v0.4.0", available: true},
			},
			check: func(t *testing.T, _ *Manager, c *notify.Center, _ *logSink) {
				t.Helper()
				n := activeKeys(c)[AvailableKey]
				if !strings.Contains(n.Title, "v0.4.0") || len(c.Snapshot().Notifications) != 1 {
					t.Errorf("condition %+v, want one entry rewritten for v0.4.0", n)
				}
			},
		},
		{
			name: "no release forgets the offer",
			steps: []step{
				{rel: fakeRelease(vNew), latest: vNew, available: true},
				{err: ErrNoRelease, notice: "no published release"},
			},
			check: func(t *testing.T, _ *Manager, c *notify.Center, _ *logSink) {
				t.Helper()
				if _, ok := activeKeys(c)[AvailableKey]; ok {
					t.Error("condition still active after the release was withdrawn")
				}
			},
		},
		{
			name: "a notice after a verified release is logged again",
			steps: []step{
				{err: errNoManifest(vNew), want: checkResult{pending: true}, notice: vNew},
				{rel: fakeRelease(vNew), latest: vNew, available: true},
				{err: errNoManifest(vNew), want: checkResult{pending: true}, latest: vNew, available: true, notice: vNew},
			},
			check: func(t *testing.T, _ *Manager, _ *notify.Center, logs *logSink) {
				t.Helper()
				if n := logs.count("no update information"); n != 2 {
					t.Errorf("logged %d notices, want 2: one per gap", n)
				}
			},
		},
		{
			name: "failures and notices alternate",
			steps: []step{
				{err: fail, want: checkResult{failures: 1}, lastError: true},
				{err: fail, want: checkResult{failures: 2}, lastError: true},
				{err: errNoManifest(vNew), want: checkResult{pending: true}, notice: vNew},
				{err: fail, want: checkResult{failures: 1}, lastError: true},
				{err: errNoManifest(vNew), want: checkResult{pending: true}, notice: vNew},
				{err: fail, want: checkResult{failures: 1}, lastError: true},
			},
			check: func(t *testing.T, _ *Manager, _ *notify.Center, logs *logSink) {
				t.Helper()
				if n := logs.count("check failed"); n != 1 {
					t.Errorf("logged %d failures, want 1", n)
				}
				if n := logs.count("no update information"); n != 1 {
					t.Errorf("logged %d notices, want 1", n)
				}
				if n := logs.count("succeed again"); n != 0 {
					t.Errorf("logged %d recoveries, want 0", n)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetch{}
			logs := &logSink{}
			c := notify.NewCenter()
			m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Publisher: c, Logf: logs.logf})
			m.Apply(on())
			for i, s := range tt.steps {
				f.set(s.rel, s.err)
				if got := m.check(t.Context(), false); got != s.want {
					t.Errorf("step %d: result %+v, want %+v", i, got, s.want)
				}
				st := m.Status()
				if st.Latest != s.latest || st.Available != s.available {
					t.Errorf("step %d: latest %q available %t, want %q %t", i, st.Latest, st.Available, s.latest, s.available)
				}
				if (st.LastError != "") != s.lastError {
					t.Errorf("step %d: LastError %q, want set %t", i, st.LastError, s.lastError)
				}
				if (st.LastNotice == "") != (s.notice == "") || !strings.Contains(st.LastNotice, s.notice) {
					t.Errorf("step %d: LastNotice %q, want one containing %q", i, st.LastNotice, s.notice)
				}
				if st.LastError != "" && st.LastNotice != "" {
					t.Errorf("step %d: both LastError %q and LastNotice %q set", i, st.LastError, st.LastNotice)
				}
			}
			if tt.check != nil {
				tt.check(t, m, c, logs)
			}
		})
	}
}

// TestManagerNoticeClearedByOff pins that turning checks off forgets a
// notice, so a throttled manual check after turning them back on cannot show
// an old one.
func TestManagerNoticeClearedByOff(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{err: errNoManifest(vNew)}
	m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Publisher: notify.NewCenter(), Logf: (&logSink{}).logf})
	m.Apply(on())
	m.check(t.Context(), false)
	if m.Status().LastNotice == "" {
		t.Fatal("precondition: no notice recorded")
	}
	m.Apply(off())
	if got := m.Status().LastNotice; got != "" {
		t.Errorf("LastNotice %q after checks were turned off, want none", got)
	}
	m.Apply(on())
	st, err := m.CheckNow(t.Context())
	if err != nil || st.LastNotice != "" || f.count() != 1 {
		t.Errorf("CheckNow: status %+v, err %v, %d fetches; want the throttled check to replay no notice", st, err, f.count())
	}
}

// TestManagerCheckNowNoInformation pins that a manual check that finds no
// update information is not an error: the notice rides in the status.
func TestManagerCheckNowNoInformation(t *testing.T) {
	t.Parallel()
	f := &fakeFetch{err: errNoManifest(vNew)}
	m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Publisher: notify.NewCenter(), Logf: (&logSink{}).logf})
	m.Apply(on())
	st, err := m.CheckNow(t.Context())
	if err != nil || st.LastError != "" || !strings.Contains(st.LastNotice, vNew) {
		t.Errorf("CheckNow: status %+v, err %v; want a notice and no error", st, err)
	}
}

// TestManagerMissingManifestAfterStop pins that a fetch stopped by turning
// checks off records nothing, even when it returns a missing manifest after
// checks are back on.
func TestManagerMissingManifestAfterStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		resume := make(chan struct{})
		logs := &logSink{}
		m := NewManager(t.Context(), &Config{
			Running: vOld,
			Fetch: func(ctx context.Context) (*Release, error) {
				<-ctx.Done()
				<-resume
				return nil, errNoManifest(vNew)
			},
			Logf: logs.logf,
		})
		m.Apply(on())
		var r checkResult
		var wg sync.WaitGroup
		wg.Go(func() { r = m.check(t.Context(), false) })
		synctest.Wait()
		m.Apply(off())
		m.Apply(on())
		close(resume)
		wg.Wait()
		if st := m.Status(); r.pending || st.LastNotice != "" || !st.LastCheck.IsZero() || logs.count("no update information") != 0 {
			t.Errorf("result %+v, status %+v; want the stopped check not recorded", r, st)
		}
	})
}

// fetchLog is a fakeFetch whose first call can block until the manager
// cancels it (checks turned off), as a slow request does.
type fetchLog struct{ fakeFetch }

func (f *fetchLog) blockFirst(ctx context.Context) (*Release, error) {
	if f.count() == 0 {
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	return f.fetch(ctx)
}

// TestManagerTurnOffRestartsSchedule pins that turning checks off starts the
// check schedule over from the startup delay once they are back on, however
// the toggle lines up with a check in flight or with the loop being idle.
func TestManagerTurnOffRestartsSchedule(t *testing.T) {
	t.Parallel()
	type scenario struct {
		name string
		// failFirst is the number of failed checks before the run under test.
		failFirst int
		run       func(t *testing.T, m *Manager, f *fetchLog)
	}
	// duringFetch waits for the first check to be in flight.
	duringFetch := func(t *testing.T, m *Manager, f *fetchLog) {
		t.Helper()
		m.Apply(on())
		time.Sleep(firstCheckDelay)
		synctest.Wait()
		if n := f.count(); n < 1 {
			t.Fatalf("no check in flight after the startup delay: %d fetches", n)
		}
	}
	scenarios := []scenario{
		{"off then on during a fetch", 0, func(t *testing.T, m *Manager, f *fetchLog) {
			t.Helper()
			duringFetch(t, m, f)
			m.Apply(off())
			m.Apply(on())
			synctest.Wait()
		}},
		{"off then on during a fetch after failures", 2, func(t *testing.T, m *Manager, f *fetchLog) {
			t.Helper()
			duringFetch(t, m, f)
			m.Apply(off())
			m.Apply(on())
			synctest.Wait()
		}},
		{"off then on while idle on a day timer", 0, func(t *testing.T, m *Manager, f *fetchLog) {
			t.Helper()
			f.set(fakeRelease(vNew), nil)
			m.Apply(on())
			time.Sleep(firstCheckDelay)
			synctest.Wait()
			if !m.Status().Available {
				t.Fatal("the first check found no release")
			}
			// No Wait between the two: Run cannot run in between.
			m.Apply(off())
			m.Apply(on())
			synctest.Wait()
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := &fetchLog{}
				f.set(nil, &StatusError{URL: "u", Code: 503})
				fetch := f.blockFirst
				if strings.Contains(sc.name, "idle") {
					fetch = f.fetch
				}
				c := notify.NewCenter()
				m := NewManager(t.Context(), &Config{Running: vOld, Fetch: fetch, Jitter: noJitter, Logf: (&logSink{}).logf, Publisher: c})
				m.failures = sc.failFirst
				go m.Run(t.Context())
				sc.run(t, m, f)
				restarted := time.Now()
				if got, want := m.Status().NextCheck, restarted.Add(firstCheckDelay); !got.Equal(want) {
					t.Fatalf("NextCheck %v, want %v (the startup delay from the toggle)", got, want)
				}
				before := f.count()
				time.Sleep(firstCheckDelay - time.Second)
				synctest.Wait()
				if n := f.count(); n != before {
					t.Fatalf("%d fetches before the startup delay, want %d", n, before)
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if n := f.count(); n != before+1 {
					t.Fatalf("%d fetches after the startup delay, want %d", n, before+1)
				}
			})
		})
	}
}

// TestManagerTurnOffRestartsOfferAfterToggle pins that a release found before
// a quick off and on is found again after the startup delay, with its
// notification back.
func TestManagerTurnOffRestartsOfferAfterToggle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetch{rel: fakeRelease(vNew)}
		c := notify.NewCenter()
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: (&logSink{}).logf, Publisher: c})
		go m.Run(t.Context())
		m.Apply(on())
		time.Sleep(firstCheckDelay)
		synctest.Wait()
		if !m.Status().Available {
			t.Fatal("the first check found no release")
		}
		m.Apply(off())
		m.Apply(on())
		synctest.Wait()
		time.Sleep(firstCheckDelay)
		synctest.Wait()
		if !m.Status().Available {
			t.Error("the offer is not back after the startup delay")
		}
		if _, ok := activeKeys(c)[AvailableKey]; !ok {
			t.Error("the available notification is not back after the startup delay")
		}
	})
}

// TestManagerNextCheckAfterSuccessUnchanged is the control: without a
// turn-off the next check stays a day after a success.
func TestManagerNextCheckAfterSuccessUnchanged(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetch{rel: fakeRelease(vOld)}
		m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: (&logSink{}).logf})
		go m.Run(t.Context())
		m.Apply(on())
		time.Sleep(firstCheckDelay)
		synctest.Wait()
		if got, want := m.Status().NextCheck, time.Now().Add(checkInterval-checkJitter); !got.Equal(want) {
			t.Fatalf("NextCheck %v, want %v", got, want)
		}
	})
}

// TestManagerTurnOffRestartsLogDedupe pins that a notice or a failure kind is
// logged once per enabled period: turning checks off and on logs it again.
func TestManagerTurnOffRestartsLogDedupe(t *testing.T) {
	t.Parallel()
	type outcome int
	const (
		notice outcome = iota
		failure
		success
		toggle
	)
	tests := []struct {
		name                               string
		steps                              []outcome
		notices, failures, recoveredLogged int
	}{
		{"notice, notice, toggle, notice", []outcome{notice, notice, toggle, notice}, 2, 0, 0},
		{"failure, failure, toggle, failure", []outcome{failure, failure, toggle, failure}, 0, 2, 0},
		{"notice, toggle, failure, notice", []outcome{notice, toggle, failure, notice}, 2, 1, 0},
		{"failure, toggle, success", []outcome{failure, toggle, success}, 0, 1, 0},
		{"control: notice three times", []outcome{notice, notice, notice}, 1, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetch{}
			logs := &logSink{}
			m := NewManager(t.Context(), &Config{Running: vOld, Fetch: f.fetch, Jitter: noJitter, Logf: logs.logf, Publisher: notify.NewCenter()})
			m.Apply(on())
			for _, s := range tt.steps {
				switch s {
				case toggle:
					m.Apply(off())
					m.Apply(on())
					continue
				case notice:
					f.set(nil, errNoManifest(vNew))
				case failure:
					f.set(nil, &StatusError{URL: "u", Code: 503})
				case success:
					f.set(fakeRelease(vOld), nil)
				}
				m.check(t.Context(), false)
			}
			if n := logs.count("no update information"); n != tt.notices {
				t.Errorf("logged %d notices, want %d", n, tt.notices)
			}
			if n := logs.count("check failed"); n != tt.failures {
				t.Errorf("logged %d failures, want %d", n, tt.failures)
			}
			if n := logs.count("succeed again"); n != tt.recoveredLogged {
				t.Errorf("logged %d recoveries, want %d", n, tt.recoveredLogged)
			}
		})
	}
}

// installProbe stands in for Config.Detect: it returns the Install the test
// last set and counts the reads.
type installProbe struct {
	cur   atomic.Pointer[Install]
	calls atomic.Int32
}

func newInstallProbe(in Install) *installProbe {
	p := &installProbe{}
	p.cur.Store(&in)
	return p
}

func (p *installProbe) set(in Install) { p.cur.Store(&in) }

func (p *installProbe) detect() Install {
	p.calls.Add(1)
	return *p.cur.Load()
}

// TestInstallRedetectedOnRequest pins that the installation is read again
// on request and never on a timer: Status reuses a reading for
// installRecheck, StartApply always reads afresh and refuses what the
// current installation cannot do.
func TestInstallRedetectedOnRequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		canApply := Install{Method: MethodService, CanApply: true, ServiceBin: serviceBin}
		noUpdater := Install{Method: MethodService, ServiceBin: serviceBin, Hint: "to enable one-button updates, re-run service install"}
		probe := newInstallProbe(canApply)
		dir := t.TempDir()
		staged := 0
		logs := &logSink{}
		m := NewManager(t.Context(), &Config{
			Running: vOld,
			Fetch:   (&fakeFetch{rel: fakeRelease(vNew)}).fetch,
			Stage: func(context.Context, *Release) error {
				staged++
				return os.WriteFile(filepath.Join(dir, RequestFile), []byte(`{"version":"v0.3.0"}`), 0o644)
			},
			Dir:       dir,
			Install:   canApply,
			Detect:    probe.detect,
			Publisher: notify.NewCenter(),
			Logf:      logs.logf,
		})
		m.Apply(on())
		if _, err := m.CheckNow(t.Context()); err != nil {
			t.Fatal(err)
		}
		wantStatus := func(step string, wantCan bool, wantCalls int32) {
			t.Helper()
			if got := m.Status().Install.CanApply; got != wantCan {
				t.Errorf("%s: CanApply %t, want %t", step, got, wantCan)
			}
			if got := probe.calls.Load(); got != wantCalls {
				t.Errorf("%s: %d installation reads, want %d", step, got, wantCalls)
			}
		}

		wantStatus("startup value", true, 0)
		probe.set(noUpdater)
		wantStatus("inside the window", true, 0)

		st, err := m.StartApply()
		if !errors.Is(err, ErrCannotApply) || st.Install.CanApply {
			t.Fatalf("StartApply after the updater went away: %+v, %v; want ErrCannotApply with CanApply false", st, err)
		}
		if staged != 0 {
			t.Errorf("a refused press staged %d times, want 0", staged)
		}
		wantStatus("after the refused press", false, 1)

		probe.set(canApply)
		time.Sleep(installRecheck)
		wantStatus("after the window", true, 2)
		wantStatus("inside the new window", true, 2)
		wantStatus("inside the new window again", true, 2)

		st, err = m.StartApply()
		if err != nil || st.Phase != PhaseDownloading {
			t.Fatalf("StartApply with the updater back: %+v, %v", st, err)
		}
		if got := probe.calls.Load(); got != 3 {
			t.Errorf("%d installation reads after the accepted press, want 3", got)
		}
		synctest.Wait()
		if staged != 1 {
			t.Errorf("staged %d times, want 1", staged)
		}
		if got := logs.count("no longer available"); got != 1 {
			t.Errorf("%d lines for the updater going away, want 1", got)
		}
		if got := logs.count("is now available"); got != 1 {
			t.Errorf("%d lines for the updater coming back, want 1", got)
		}
	})
}

// TestStartApplyKeepsTheInstallItStartedWith pins that an attempt words its
// withdraw advice from the installation its press read, not a later one.
func TestStartApplyKeepsTheInstallItStartedWith(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		at := func(bin string) Install {
			return Install{Method: MethodService, CanApply: true, ServiceBin: bin}
		}
		startup, started, later := at("/opt/startup/remote-mic"), at("/opt/started/remote-mic"), at("/opt/later/remote-mic")
		probe := newInstallProbe(started)
		dir := t.TempDir()
		m := NewManager(t.Context(), &Config{
			Running: vOld,
			Fetch:   (&fakeFetch{rel: fakeRelease(vNew)}).fetch,
			Stage: func(context.Context, *Release) error {
				return os.WriteFile(filepath.Join(dir, RequestFile), []byte(`{"version":"v0.3.0"}`), 0o644)
			},
			Dir:       dir,
			Install:   startup,
			Detect:    probe.detect,
			Publisher: notify.NewCenter(),
			Logf:      (&logSink{}).logf,
		})
		m.Apply(on())
		if _, err := m.CheckNow(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := m.StartApply(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// A later reading, as a status poll would make once the window ends.
		probe.set(later)
		time.Sleep(installRecheck + resultPoll)
		if got := m.Status().Install.ServiceBin; got != later.ServiceBin {
			t.Fatalf("status names %q after the window, want %q", got, later.ServiceBin)
		}
		time.Sleep(updaterStartTimeout + 3*resultPoll)
		synctest.Wait()
		st := m.Status()
		if st.Phase != PhaseFailed || !strings.Contains(st.PhaseMessage, started.RerunInstall()) {
			t.Errorf("status %+v, want a failure that says %q", st, started.RerunInstall())
		}
	})
}

// TestInstallWithoutDetectIsStatic pins that a Manager configured without
// Detect keeps reporting the configured Install, whatever the time.
func TestInstallWithoutDetectIsStatic(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		want := Install{Method: MethodService, CanApply: true, ServiceBin: serviceBin}
		m := NewManager(t.Context(), &Config{Running: vOld, Install: want, Logf: (&logSink{}).logf})
		time.Sleep(2 * installRecheck)
		if got := m.Status().Install; got != want {
			t.Errorf("Status().Install = %+v, want %+v", got, want)
		}
	})
}
