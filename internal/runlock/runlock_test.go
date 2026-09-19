//go:build unix

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
	if got := PathFor("/etc/remotemic/config.yaml"); got != "/etc/remotemic/config.yaml.lock" {
		t.Errorf("PathFor = %q, want the config path with .lock appended", got)
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
