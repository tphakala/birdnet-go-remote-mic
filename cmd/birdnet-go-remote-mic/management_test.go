//go:build linux

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

func servingRecord(name, path string) *deviceRuntime {
	return &deviceRuntime{
		dev: config.Device{
			Name: name, Device: "hw:1,0", Path: path,
			Mode: config.ModeOpus, Rate: 48000, Channels: 1, Format: "s16",
		},
		state:    mgmtserver.StateServing,
		rate:     48000,
		channels: 1,
	}
}

func skippedRecord(name, path, errMsg string) *deviceRuntime {
	return &deviceRuntime{
		dev: config.Device{
			Name: name, Device: "hw:2,0", Path: path,
			Mode: config.ModePCM, Rate: 192000, Channels: 1, Format: "s16",
		},
		state: mgmtserver.StateSkipped,
		err:   errMsg,
	}
}

func newProvider() *provider {
	return &provider{version: "v1.0.0", start: time.Now(), rtspListen: ":8554", discovery: true}
}

func TestProviderStatusDegradedBeforeSetDevices(t *testing.T) {
	p := newProvider()
	st := p.Status()
	if st.DevicesTotal != 0 || st.DevicesServing != 0 {
		t.Errorf("before setDevices want 0/0, got serving=%d total=%d", st.DevicesServing, st.DevicesTotal)
	}
	if got := p.Devices(); len(got) != 0 {
		t.Errorf("Devices() before setDevices = %d, want 0", len(got))
	}
	if _, ok := p.Device("garden"); ok {
		t.Error("Device lookup before setDevices should miss")
	}
}

func TestProviderStatusCountsServing(t *testing.T) {
	p := newProvider()
	p.setDevices([]*deviceRuntime{
		servingRecord("garden", "/garden"),
		skippedRecord("attic", "/attic", "open capture: device busy"),
	})
	st := p.Status()
	if st.DevicesTotal != 2 || st.DevicesServing != 1 {
		t.Errorf("want serving=1 total=2, got serving=%d total=%d", st.DevicesServing, st.DevicesTotal)
	}
}

func TestProviderDeviceLookup(t *testing.T) {
	p := newProvider()
	p.setDevices([]*deviceRuntime{
		servingRecord("garden", "/garden"),
		skippedRecord("attic", "/attic", "open capture: device busy"),
	})

	d, ok := p.Device("attic")
	if !ok {
		t.Fatal("Device(attic) not found")
	}
	if d.State != mgmtserver.StateSkipped || d.Error != "open capture: device busy" {
		t.Errorf("attic mapped wrong: state=%q err=%q", d.State, d.Error)
	}
	if _, ok := p.Device("ghost"); ok {
		t.Error("Device(ghost) should not be found")
	}
}

func TestClosedMgmtWaitReturns(t *testing.T) {
	// A nil handle (management disabled) and a closed handle (cert failure) must
	// both make Wait return immediately so shutdown never blocks on them.
	var nilHandle *mgmt
	nilHandle.Wait()
	closedMgmt().Wait()
}

func TestStartManagementCertFailureReportsUnavailable(t *testing.T) {
	// Point cert_dir at a regular file so certificate persistence cannot succeed.
	// A configured-but-dead API must report ok=false so run() does not treat it as
	// a live diagnostic surface.
	badDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Management: config.Management{Listen: "127.0.0.1:0", CertDir: badDir}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil)
	if ok {
		t.Error("a certificate failure must report management unavailable")
	}
	h.Wait() // the returned handle must not block shutdown
}

func TestStartManagementBindFailureReportsUnavailable(t *testing.T) {
	// Occupy a port, then point the management listener at it so the bind fails.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()

	cfg := &config.Config{Management: config.Management{Listen: occupied.Addr().String(), CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil)
	if ok {
		t.Error("a listener bind failure must report management unavailable")
	}
	h.Wait()
}

func TestStartManagementServesAndShutsDown(t *testing.T) {
	// The happy path: the listener binds, ok is true, and Wait returns once ctx
	// cancellation drives a graceful shutdown.
	cfg := &config.Config{Management: config.Management{Listen: "127.0.0.1:0", CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil)
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	cancel() // trigger graceful shutdown
	h.Wait()
}
