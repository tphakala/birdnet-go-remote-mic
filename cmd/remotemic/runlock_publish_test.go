//go:build linux

package main

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
)

// testMgmtAddr and testCertFile are a management endpoint the lock and retry
// tests publish and read back.
const (
	testMgmtAddr = "127.0.0.1:8443"
	testCertFile = "mgmt-cert.pem"
)

// TestPublishRunLock pins what the token commands read: the management endpoint
// when an API serves (including one a background retry brought up late), and a
// PID-only state when none does, so they edit the file instead.
func TestPublishRunLock(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	lockPath := runlock.PathFor(cfgPath)
	lock, err := runlock.Acquire(lockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if rerr := lock.Release(); rerr != nil {
			t.Errorf("releasing the run lock: %v", rerr)
		}
	})

	pub := &runLockPublisher{lock: lock, cfgPath: cfgPath}
	pub.publish(nil)
	st, ok, err := runlock.ReadState(lockPath)
	if err != nil || !ok {
		t.Fatalf("ReadState = %+v, %v, %v; want a published state", st, ok, err)
	}
	if st.MgmtAddr != "" || st.CertPath != "" {
		t.Errorf("got %+v, want no management endpoint while no API serves", st)
	}

	pub.publish(&mgmtEndpoint{addr: testMgmtAddr, certPath: testCertFile})
	st, ok, err = runlock.ReadState(lockPath)
	if err != nil || !ok {
		t.Fatalf("ReadState = %+v, %v, %v; want a published state", st, ok, err)
	}
	if st.MgmtAddr != testMgmtAddr {
		t.Errorf("got address %q, want the recovered API's", st.MgmtAddr)
	}
	if !filepath.IsAbs(st.CertPath) || filepath.Base(st.CertPath) != testCertFile {
		t.Errorf("got certificate path %q, want it made absolute", st.CertPath)
	}
}

// recordLock is a runLockPublisher write seam that records every state it is
// asked to write and fails while fail is positive, counting it down.
type recordLock struct {
	states []runlock.State
	fail   int
}

func (r *recordLock) write(st runlock.State) error {
	r.states = append(r.states, st)
	if r.fail > 0 {
		r.fail--
		return errors.New("disk full")
	}
	return nil
}

// TestRunLockPublisherRetriesFailedWrite pins the recovery from a failed
// write, which can leave the lock empty (read as "starting up"): the publisher
// rewrites the latest state it was asked for on a bounded retry until a write
// succeeds, a newer state replaces the one being retried, and stop cancels a
// pending retry.
func TestRunLockPublisherRetriesFailedWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := &recordLock{fail: 2}
		pub := &runLockPublisher{cfgPath: testCfgFile, write: rec.write, retryDelay: time.Second}
		pub.starting()
		ep := &mgmtEndpoint{addr: testMgmtAddr, certPath: testCertFile}
		pub.publish(ep) // replaces "starting" while the first write is being retried
		if len(rec.states) != 2 {
			t.Fatalf("got %d writes, want one per request", len(rec.states))
		}
		time.Sleep(time.Second)
		synctest.Wait()
		want := runLockState(ep)
		if len(rec.states) != 3 || rec.states[2] != want {
			t.Fatalf("writes = %+v, want the endpoint rewritten by one retry", rec.states)
		}
		// The retry succeeded: nothing more is written.
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(rec.states) != 3 {
			t.Errorf("got %d writes, want none after a successful retry", len(rec.states))
		}

		// stop cancels a retry that is pending.
		rec.fail = 1
		pub.publish(nil)
		pub.stop()
		pub.publish(ep)
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(rec.states) != 4 {
			t.Errorf("got %d writes, want none after stop", len(rec.states))
		}
	})
}

// TestRunLockPublisherRaisesCondition pins that a failing run lock is visible:
// the first failure of a streak raises one condition, further failures do not
// raise it again, and the write that succeeds clears it.
func TestRunLockPublisherRaisesCondition(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := &recordLock{fail: 3}
		center := notify.NewCenter()
		pub := &runLockPublisher{cfgPath: testCfgFile, write: rec.write, retryDelay: time.Second, center: center}
		pub.publish(nil)
		if act := center.Active(); len(act) != 1 || act[0].Key != runLockKey {
			t.Fatalf("active = %+v, want the unwritable-lock condition", act)
		}
		time.Sleep(2 * time.Second) // two more failed retries
		synctest.Wait()
		onsets := 0
		for _, n := range center.Snapshot().Notifications {
			if n.Key == runLockKey && n.Kind == notify.KindOnset {
				onsets++
			}
		}
		if onsets != 1 {
			t.Errorf("got %d onsets over a failing streak, want 1", onsets)
		}
		time.Sleep(time.Second) // the retry that succeeds
		synctest.Wait()
		if act := center.Active(); len(act) != 0 {
			t.Errorf("active = %+v, want the condition cleared once the lock is written", act)
		}
		pub.stop()
	})
}

// TestRunLockPublisherNil pins that a nil publisher (tests that drive the
// retry without a lock) writes nothing and does not panic.
func TestRunLockPublisherNil(t *testing.T) {
	t.Parallel()
	var pub *runLockPublisher
	pub.starting()
	pub.publish(nil)
	pub.stop()
}

// lockStates returns what rec recorded, as the token commands would read each
// state: "starting" (no PID), "no API", or the API address.
func lockStates(rec *recordLock) []string {
	out := make([]string, 0, len(rec.states))
	for _, st := range rec.states {
		switch {
		case st.PID == 0:
			out = append(out, "starting")
		case st.MgmtAddr == "":
			out = append(out, "no API")
		default:
			out = append(out, st.MgmtAddr)
		}
	}
	return out
}

// equalStates reports whether a recorded sequence (lock states, supervisor
// steps) is exactly want.
func equalStates(got []string, want ...string) bool { return slices.Equal(got, want) }
