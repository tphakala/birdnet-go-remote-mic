//go:build unix && !aix && !solaris

package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockBinExcludesAndReleases(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "remote-mic")
	release, err := LockBin(t.Context(), bin, time.Second, nil)
	if err != nil {
		t.Fatalf("first LockBin: %v", err)
	}
	waits := 0
	if _, err := LockBin(t.Context(), bin, 250*time.Millisecond, func() { waits++ }); !errors.Is(err, ErrBinBusy) {
		t.Fatalf("second LockBin while held: got %v, want ErrBinBusy", err)
	}
	if waits != 1 {
		t.Errorf("waiting called %d times, want once", waits)
	}
	release()
	release2, err := LockBin(t.Context(), bin, time.Second, nil)
	if err != nil {
		t.Fatalf("LockBin after release: %v", err)
	}
	release2()
	if _, err := os.Stat(bin + BinLockSuffix); err != nil {
		t.Errorf("lock file removed on release: %v", err)
	}
}

func TestLockBinWaitsForTheHolder(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "remote-mic")
	release, err := LockBin(t.Context(), bin, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The holder lets go from inside the waiter's first wait, so the waiter
	// has seen the lock held before it can get it.
	waited := false
	release2, err := LockBin(t.Context(), bin, 10*time.Second, func() {
		waited = true
		release()
	})
	if err != nil {
		t.Fatalf("LockBin should get the lock once the holder releases: %v", err)
	}
	release2()
	if !waited {
		t.Error("LockBin never saw the lock held")
	}
}

func TestLockBinStopsOnCancel(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "remote-mic")
	release, err := LockBin(t.Context(), bin, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	// Cancel from the first wait, so it ends the wait it interrupts.
	if _, err := LockBin(ctx, bin, time.Minute, cancel); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

// TestLockBinRefusesALink pins that a link planted at the lock path is not
// followed.
func TestLockBinRefusesALink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "remote-mic")
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, bin+BinLockSuffix); err != nil {
		t.Fatal(err)
	}
	if _, err := LockBin(t.Context(), bin, time.Second, nil); err == nil {
		t.Error("LockBin followed a symlink at the lock path")
	}
}

// TestApplyWaitsForInstallLock pins that the updater holds off while a
// service install holds the binary's lock, then applies.
func TestApplyWaitsForInstallLock(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	release, err := LockBin(t.Context(), env.binPath, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.a.LockWait = 10 * time.Second
	// Apply logs that it is waiting from its own goroutine, the test's, once
	// it has found the lock held: check the binary there, then let go.
	waited := false
	env.a.Logf = func(format string, args ...any) {
		if strings.Contains(format, "waiting for a service install") {
			waited = true
			if got := env.installed(t); got != oldBinary {
				t.Errorf("binary replaced while install held the lock: %q", got)
			}
			release()
		}
	}
	if err := env.a.Apply(t.Context()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !waited {
		t.Error("Apply never waited for the lock")
	}
	if got := env.installed(t); got != newBinary {
		t.Errorf("installed %q, want the new binary", got)
	}
}

// TestApplyTakesTheLockBeforeRecoveringAnInterruptedInstall pins that the
// journal recovery, which rolls the binary back from .prev, runs under the
// lock too: with a `service install` holding it, nothing is touched.
func TestApplyTakesTheLockBeforeRecoveringAnInterruptedInstall(t *testing.T) {
	t.Parallel()
	env := interruptedEnv(t)
	release, err := LockBin(t.Context(), env.binPath, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	env.a.LockWait = 150 * time.Millisecond
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), ErrBinBusy.Error()) {
		t.Fatalf("Apply: got %v, want a failure naming the busy lock", err)
	}
	if got := env.installed(t); got != newBinary {
		t.Errorf("installed %q: the recovery rolled back without the lock", got)
	}
	if _, err := os.Stat(env.binPath + ".pending"); err != nil {
		t.Errorf("journal gone without the lock: %v", err)
	}
	if env.restarts != 0 {
		t.Errorf("unit restarted %d times without the lock", env.restarts)
	}
}

// TestApplyFailsWhenTheLockStaysHeld pins that a lock still held after the
// wait fails the request, touching nothing.
func TestApplyFailsWhenTheLockStaysHeld(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	release, err := LockBin(t.Context(), env.binPath, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	env.a.LockWait = 150 * time.Millisecond
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), ErrBinBusy.Error()) {
		t.Fatalf("Apply: got %v, want a failure naming the busy lock", err)
	}
	if got := env.installed(t); got != oldBinary {
		t.Errorf("binary changed without the lock: %q", got)
	}
	res := env.result(t)
	if res.Outcome != OutcomeFailed || res.Installed != vOld {
		t.Errorf("result %+v, want a failure that keeps %s", res, vOld)
	}
	env.requestGone(t)
	if env.restarts != 0 {
		t.Errorf("unit restarted %d times without the lock", env.restarts)
	}
}

// TestApplyRevalidatesTheInstalledBinaryAfterWaiting pins that an updater that
// waited for the lock does not act on what it saw before: a newer binary put
// there by `service install` in the meantime is left alone, not replaced by
// the staged release this older updater was started for.
func TestApplyRevalidatesTheInstalledBinaryAfterWaiting(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	release, err := LockBin(t.Context(), env.binPath, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.a.LockWait = 10 * time.Second
	const newer = "remote-mic v0.4.0\nnewer"
	env.a.Logf = func(format string, args ...any) {
		if strings.Contains(format, "waiting for a service install") {
			if err := os.WriteFile(env.binPath, []byte(newer), 0o755); err != nil {
				t.Error(err)
			}
			release()
		}
	}
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "changed since this updater started") {
		t.Fatalf("Apply: got %v, want a failure naming the change", err)
	}
	if got := env.installed(t); got != newer {
		t.Errorf("installed %q, want the newer binary left alone", got)
	}
	if _, err := os.Stat(env.binPath + ".prev"); !os.IsNotExist(err) {
		t.Errorf("rollback copy written for an update that did not run: %v", err)
	}
	res := env.result(t)
	if res.Outcome != OutcomeFailed {
		t.Errorf("result %+v, want a failure", res)
	}
	env.requestGone(t)
	if env.restarts != 0 {
		t.Errorf("unit restarted %d times", env.restarts)
	}
}

// TestApplyRechecksTheBinaryAfterWaiting pins the other two ways the recheck
// under the lock refuses: the binary or its directory is no longer root-only,
// or the installed binary no longer runs. Nothing is installed either way.
func TestApplyRechecksTheBinaryAfterWaiting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(env *applyEnv)
		want   string
	}{
		{
			name: "no longer root-only",
			change: func(env *applyEnv) {
				env.a.Owner = func(os.FileInfo) (uint32, uint32, bool) { return 1000, 1000, true }
			},
			want: "refusing to update",
		},
		{
			name: "installed binary fails to run",
			change: func(env *applyEnv) {
				run := env.a.Version
				env.a.Version = func(ctx context.Context, bin string) (string, error) {
					if bin == env.binPath {
						return "", errors.New("exec format error")
					}
					return run(ctx, bin)
				}
			},
			want: "exec format error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newApplyEnv(t)
			release, err := LockBin(t.Context(), env.binPath, time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			env.a.LockWait = 10 * time.Second
			env.a.Logf = func(format string, args ...any) {
				if strings.Contains(format, "waiting for a service install") {
					tt.change(env)
					release()
				}
			}
			if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply: got %v, want a failure containing %q", err, tt.want)
			}
			if got := env.installed(t); got != oldBinary {
				t.Errorf("installed %q, want the binary left alone", got)
			}
			if env.restarts != 0 {
				t.Errorf("unit restarted %d times", env.restarts)
			}
		})
	}
}

// TestApplyRevalidatesWithoutHavingWaited pins that the recheck does not depend
// on the lock having been busy: an install that finished before the updater
// asked for the lock leaves a newer binary just the same.
func TestApplyRevalidatesWithoutHavingWaited(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	const newer = "remote-mic v0.4.0\nnewer"
	if err := os.WriteFile(env.binPath, []byte(newer), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "changed since this updater started") {
		t.Fatalf("Apply: got %v, want a failure naming the change", err)
	}
	if got := env.installed(t); got != newer {
		t.Errorf("installed %q, want the newer binary left alone", got)
	}
	if env.restarts != 0 {
		t.Errorf("unit restarted %d times", env.restarts)
	}
}

// TestApplyCancelledWhileRecheckingTheInstalledBinary pins that a stop while
// the installed binary is being checked under the lock ends the run as
// stopped, not as a changed binary.
func TestApplyCancelledWhileRecheckingTheInstalledBinary(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	env.a.Version = func(context.Context, string) (string, error) {
		cancel()
		return "", errors.New("signal: killed")
	}
	if err := env.a.Apply(ctx); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Apply: got %v, want the run reported as stopped", err)
	}
	if got := env.installed(t); got != oldBinary {
		t.Errorf("installed %q, want it untouched", got)
	}
}
