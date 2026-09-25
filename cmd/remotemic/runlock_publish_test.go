//go:build linux

package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
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

	publishRunLock(lock, cfgPath, &mgmtEndpoint{addr: "127.0.0.1:8443", certPath: "mgmt-cert.pem"})
	st, ok, err = runlock.ReadState(lockPath)
	if err != nil || !ok {
		t.Fatalf("ReadState = %+v, %v, %v; want a published state", st, ok, err)
	}
	if st.MgmtAddr != "127.0.0.1:8443" {
		t.Errorf("got address %q, want the recovered API's", st.MgmtAddr)
	}
	if !filepath.IsAbs(st.CertPath) || filepath.Base(st.CertPath) != "mgmt-cert.pem" {
		t.Errorf("got certificate path %q, want it made absolute", st.CertPath)
	}
}
