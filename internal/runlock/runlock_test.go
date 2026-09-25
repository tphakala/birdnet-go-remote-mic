//go:build unix && !aix && !solaris

package runlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// flock locks belong to the open file description, so two TryAcquire calls in
// one process contend exactly like two processes would.

func TestTryAcquireExcludesSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.lock")
	l, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	if _, err := TryAcquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("second TryAcquire err = %v, want ErrHeld", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after Release: %v", err)
	}
	_ = l2.Release()
}

func TestAcquireWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.lock")
	l, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = l.Release()
	}()
	l2, err := Acquire(path, 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire did not get the lock after release: %v", err)
	}
	_ = l2.Release()
}

func TestAcquireGivesUpWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.lock")
	l, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer func() {
		if err := l.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()
	const wait = 150 * time.Millisecond
	start := time.Now()
	if _, err := Acquire(path, wait); !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire err = %v, want ErrHeld", err)
	}
	if elapsed := time.Since(start); elapsed < wait {
		t.Fatalf("Acquire gave up after %v, want it to wait at least %v", elapsed, wait)
	}
}

// TestParseStateEdges asserts that empty, undecodable, and pid-zero content all
// read as "nothing published", while a well-formed record parses.
func TestParseStateEdges(t *testing.T) {
	for _, tc := range []struct {
		name, in string
	}{
		{"empty", ""},
		{"garbage", "not json at all"},
		{"pid zero", `{"pid":0,"mgmtAddr":"127.0.0.1:9443"}`},
	} {
		if _, ok := parseState([]byte(tc.in)); ok {
			t.Errorf("%s: parseState ok=true, want not published", tc.name)
		}
	}
	if s, ok := parseState([]byte(`{"pid":7,"mgmtAddr":"x"}`)); !ok || s.PID != 7 {
		t.Errorf("valid record: ok=%v pid=%d, want ok with pid 7", ok, s.PID)
	}
}

// TestReadStateMissingFileErrors asserts a missing lock file is an error, not a
// silent "nothing published".
func TestReadStateMissingFileErrors(t *testing.T) {
	if _, ok, err := ReadState(filepath.Join(t.TempDir(), "absent.lock")); err == nil || ok {
		t.Fatalf("ReadState on a missing file: ok=%v err=%v, want an error", ok, err)
	}
}

func TestPublishReadStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.lock")
	l, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer func() { _ = l.Release() }()
	if _, ok, err := ReadState(path); err != nil || ok {
		t.Fatalf("before Publish: ok=%v err=%v, want not published", ok, err)
	}
	want := State{PID: 42, MgmtAddr: "[::]:8443", CertPath: "/x/mgmt-cert.pem"}
	if err := l.Publish(want); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// A shorter second publish must not leave a tail of the first.
	want = State{PID: 42}
	if err := l.Publish(want); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got, ok, err := ReadState(path)
	if err != nil || !ok {
		t.Fatalf("ReadState: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("ReadState = %+v, want %+v", got, want)
	}
}

func TestReleaseEmptiesButKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.lock")
	l, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if err := l.Publish(State{PID: 7}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("lock file removed on release: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("lock file size after release = %d, want 0", info.Size())
	}
}

// TestTryAcquireClearsStaleState asserts that acquiring a lock whose previous
// holder crashed (leaving published state, since Release never ran) empties the
// file, so a token command reading it during the new holder's startup sees
// "not published" rather than the dead appliance's address.
func TestTryAcquireClearsStaleState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.lock")
	if err := os.WriteFile(path, []byte(`{"pid":1234,"mgmtAddr":"127.0.0.1:9443"}`), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}
	l, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer func() {
		if err := l.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()
	if _, ok, err := ReadState(path); err != nil || ok {
		t.Fatalf("ReadState after re-acquiring a crashed lock: ok=%v err=%v, want not published", ok, err)
	}
}

func TestNilLockIsNoop(t *testing.T) {
	var l *Lock
	if err := l.Publish(State{PID: 1}); err != nil {
		t.Fatalf("nil Publish: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
}

func TestPathFor(t *testing.T) {
	dir := t.TempDir()

	// A path whose file and directory both fail to resolve falls back to the
	// literal path with .lock appended.
	missing := filepath.Join(dir, "nope", "config.yaml")
	if got := PathFor(missing); got != missing+".lock" {
		t.Errorf("PathFor(missing) = %q, want the literal path with .lock appended", got)
	}

	// A config reached through a symlink and through its target resolve to the
	// same lock path, so the appliance and a token command that name it
	// differently do not each take their own lock and both believe no appliance
	// is running. The expected side is EvalSymlinks-resolved too, because the temp
	// root itself may be a symlink (e.g. /var -> /private/var on macOS).
	target := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(target, []byte("listen: :8554\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config-link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	want := resolvedTarget + ".lock"
	if got := PathFor(link); got != want {
		t.Errorf("PathFor(symlink) = %q, want the target's lock %q", got, want)
	}
	if got := PathFor(target); got != want {
		t.Errorf("PathFor(target) = %q, want %q", got, want)
	}

	// A config that does not exist yet, reached through a symlinked PARENT
	// directory, still maps to one lock: PathFor resolves the directory even
	// though the leaf will only be created later.
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	viaReal := PathFor(filepath.Join(realDir, "new.yaml"))
	viaLink := PathFor(filepath.Join(linkDir, "new.yaml"))
	if viaReal != viaLink {
		t.Errorf("first-run parent symlink: PathFor(real) = %q != PathFor(link) = %q", viaReal, viaLink)
	}

	// A dangling final symlink (its target not created yet) derives the target's
	// lock, so the link and its future target converge rather than taking
	// separate locks. This must hold for a relative target and for an absolute
	// target whose directory is itself a symlink (realDir/linkDir from above).
	relDangling := filepath.Join(dir, "rel-dangling.yaml")
	if err := os.Symlink("future.yaml", relDangling); err != nil {
		t.Fatal(err)
	}
	if got, wantConv := PathFor(relDangling), PathFor(filepath.Join(dir, "future.yaml")); got != wantConv {
		t.Errorf("relative dangling: PathFor(link) = %q != PathFor(target) = %q", got, wantConv)
	}

	absTarget := filepath.Join(linkDir, "abs-future.yaml") // linkDir -> realDir; abs-future.yaml never created
	absDangling := filepath.Join(dir, "abs-dangling.yaml")
	if err := os.Symlink(absTarget, absDangling); err != nil {
		t.Fatal(err)
	}
	if got, wantConv := PathFor(absDangling), PathFor(absTarget); got != wantConv {
		t.Errorf("absolute dangling: PathFor(link) = %q != PathFor(target) = %q", got, wantConv)
	}
}

// TestTryAcquireOpenError asserts a lock file that cannot be created is a plain
// error, not ErrHeld, so serve can tell "cannot lock" from "already running".
func TestTryAcquireOpenError(t *testing.T) {
	_, err := TryAcquire(filepath.Join(t.TempDir(), "missing-dir", "config.yaml.lock"))
	if err == nil || errors.Is(err, ErrHeld) {
		t.Fatalf("TryAcquire in a missing directory: err = %v, want a non-ErrHeld error", err)
	}
}

// TestLockEditsSerializes asserts two edit-lock holders serialize: while one
// holds the lock a no-wait attempt gets ErrHeld and a waiting attempt stays
// blocked, and the waiter gets the lock once the first holder releases.
func TestLockEditsSerializes(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	release, err := LockEdits(cfgPath, 0)
	if err != nil {
		t.Fatalf("first LockEdits: %v", err)
	}
	if _, err := LockEdits(cfgPath, 0); !errors.Is(err, ErrHeld) {
		t.Fatalf("no-wait LockEdits while held: err = %v, want ErrHeld", err)
	}

	type result struct {
		release func() error
		err     error
	}
	got := make(chan result, 1)
	go func() {
		r, err := LockEdits(cfgPath, time.Minute)
		got <- result{r, err}
	}()
	select {
	case r := <-got:
		if r.release != nil {
			_ = r.release()
		}
		t.Fatalf("second LockEdits returned while the first held the lock: err = %v", r.err)
	default:
	}

	if err := release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("second LockEdits after release: %v", r.err)
		}
		if err := r.release(); err != nil {
			t.Errorf("second release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second LockEdits did not get the lock within 10s of the first release")
	}
}

// TestLockEditsWaitsForRelease asserts a waiting LockEdits keeps retrying while
// the lock is held and gets it once the holder releases, rather than giving up
// on its first attempt.
func TestLockEditsWaitsForRelease(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	release, err := LockEdits(cfgPath, 0)
	if err != nil {
		t.Fatalf("first LockEdits: %v", err)
	}
	const delay = 3 * retryInterval
	released := make(chan error, 1)
	start := time.Now()
	time.AfterFunc(delay, func() { released <- release() })

	release2, err := LockEdits(cfgPath, time.Minute)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waiting LockEdits: %v, want the lock after the holder released", err)
	}
	t.Cleanup(func() {
		if err := release2(); err != nil {
			t.Errorf("second release: %v", err)
		}
	})
	if err := <-released; err != nil {
		t.Errorf("first release: %v", err)
	}
	if elapsed < delay {
		t.Fatalf("waiting LockEdits returned after %v, want at least %v (the holder's release)", elapsed, delay)
	}
}

// TestLockEditsGivesUpWhileHeld asserts a positive wait that expires while the
// lock is still held returns ErrHeld, after waiting it out.
func TestLockEditsGivesUpWhileHeld(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	release, err := LockEdits(cfgPath, 0)
	if err != nil {
		t.Fatalf("first LockEdits: %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})
	const wait = 3 * retryInterval
	start := time.Now()
	if _, err := LockEdits(cfgPath, wait); !errors.Is(err, ErrHeld) {
		t.Fatalf("LockEdits while held: err = %v, want ErrHeld", err)
	}
	if elapsed := time.Since(start); elapsed < wait {
		t.Fatalf("LockEdits gave up after %v, want it to wait at least %v", elapsed, wait)
	}
}

// TestEditPathForConverges asserts a relative spelling and a spelling through a
// symlinked directory map to the same edit lock as the absolute path, both for
// an existing config and for one not created yet. Not parallel: it changes the
// working directory.
func TestEditPathForConverges(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(realDir, "config.yaml")
	if err := os.WriteFile(existing, []byte("listen: :8554\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(realDir)
	for _, base := range []string{"config.yaml", "not-yet.yaml"} {
		want := EditPathFor(filepath.Join(realDir, base))
		if got := EditPathFor(base); got != want {
			t.Errorf("%s relative: EditPathFor = %q, want %q", base, got, want)
		}
		if got := EditPathFor(filepath.Join(linkDir, base)); got != want {
			t.Errorf("%s via symlinked dir: EditPathFor = %q, want %q", base, got, want)
		}
	}
}

// TestLockEditsIndependentOfRunLock asserts the edit lock never contends with
// the run lock an appliance holds for its whole life, lives at EditPathFor with
// mode 0600, and survives release (it is never unlinked).
func TestLockEditsIndependentOfRunLock(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	run, err := TryAcquire(PathFor(cfgPath))
	if err != nil {
		t.Fatalf("TryAcquire run lock: %v", err)
	}
	defer func() { _ = run.Release() }()

	release, err := LockEdits(cfgPath, 0)
	if err != nil {
		t.Fatalf("LockEdits under a held run lock: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	editPath := EditPathFor(cfgPath)
	if want := PathFor(cfgPath) + ".edit"; editPath != want {
		t.Fatalf("EditPathFor = %q, want %q", editPath, want)
	}
	fi, err := os.Stat(editPath)
	if err != nil {
		t.Fatalf("edit lock file after release: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("edit lock mode = %o, want 600", got)
	}
}

// TestLockEditsOpenError asserts an edit lock file that cannot be created is a
// plain error, not ErrHeld.
func TestLockEditsOpenError(t *testing.T) {
	t.Parallel()
	_, err := LockEdits(filepath.Join(t.TempDir(), "missing-dir", "config.yaml"), 0)
	if err == nil || errors.Is(err, ErrHeld) {
		t.Fatalf("LockEdits in a missing directory: err = %v, want a non-ErrHeld error", err)
	}
}

// TestPublishAfterReleaseErrors asserts Publish reports a failed write rather
// than silently dropping the state.
func TestPublishAfterReleaseErrors(t *testing.T) {
	l, err := TryAcquire(filepath.Join(t.TempDir(), "config.yaml.lock"))
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l.Publish(State{PID: 1}); err == nil {
		t.Fatal("Publish on a released lock returned nil, want an error")
	}
}
