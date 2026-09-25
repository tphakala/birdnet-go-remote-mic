//go:build linux

package main

import (
	"path/filepath"
	"testing"
	"time"

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
	t.Cleanup(func() { _ = lock.Release() })

	publishRunLock(lock, cfgPath, nil)
	st, ok, err := runlock.ReadState(lockPath)
	if err != nil || !ok {
		t.Fatalf("ReadState = %+v, %v, %v; want a published state", st, ok, err)
	}
	if st.MgmtAddr != "" || st.CertPath != "" {
		t.Errorf("got %+v, want no management endpoint while no API serves", st)
	}

	publishRunLock(lock, cfgPath, &mgmtEndpoint{addr: testMgmtAddr, certPath: testCertFile})
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
