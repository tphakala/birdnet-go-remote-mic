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
	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()
	release2, err := LockBin(t.Context(), bin, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("LockBin should get the lock once the holder releases: %v", err)
	}
	release2()
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
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := LockBin(ctx, bin, time.Minute, nil); !errors.Is(err, context.Canceled) {
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
	go func() {
		time.Sleep(200 * time.Millisecond)
		if got := env.installed(t); got != oldBinary {
			t.Errorf("binary replaced while install held the lock: %q", got)
		}
		release()
	}()
	if err := env.a.Apply(t.Context()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := env.installed(t); got != newBinary {
		t.Errorf("installed %q, want the new binary", got)
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
