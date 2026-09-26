//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// cmdApplyUpdate is the root updater's subcommand.
const cmdApplyUpdate = "apply-update"

func saveApplySeams(t *testing.T) {
	t.Helper()
	ge, au := geteuid, applyUpdate
	t.Cleanup(func() { geteuid, applyUpdate = ge, au })
}

func TestServiceApplyUpdateDispatch(t *testing.T) {
	saveApplySeams(t)
	geteuid = func() int { return 0 }
	var gotBin, gotState string
	applyUpdate = func(_ context.Context, bin, state string) error {
		gotBin, gotState = bin, state
		return nil
	}
	var stderr bytes.Buffer
	code := dispatch([]string{cmdService, cmdApplyUpdate, "--bin-path=/opt/rm/remote-mic", "--state-dir=/srv/rm"}, &bytes.Buffer{}, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if gotBin != "/opt/rm/remote-mic" || gotState != "/srv/rm" {
		t.Errorf("applyUpdate(%q, %q)", gotBin, gotState)
	}

	gotBin = ""
	if code := dispatch([]string{cmdService, cmdApplyUpdate}, &bytes.Buffer{}, &stderr); code != 0 || gotBin != selfBin {
		t.Errorf("defaults: exit %d, bin %q", code, gotBin)
	}
}

func TestServiceApplyUpdateRefusesNonRoot(t *testing.T) {
	saveApplySeams(t)
	geteuid = func() int { return 1000 }
	applyUpdate = func(context.Context, string, string) error {
		t.Error("applyUpdate ran without root")
		return nil
	}
	var stderr bytes.Buffer
	if code := dispatch([]string{cmdService, cmdApplyUpdate}, &bytes.Buffer{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "must run as root") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	if code := dispatch([]string{cmdService, cmdApplyUpdate, "stray"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("stray argument: exit %d, want 2", code)
	}
}

func TestUpdateDirFor(t *testing.T) {
	t.Parallel()
	cfg := config.Config{}
	if got := updateDirFor(&cfg, "/etc/remote-mic/config.yaml"); got != "/etc/remote-mic/update" {
		t.Errorf("no cert dir: %q", got)
	}
	cfg.Management.CertDir = "/var/lib/remote-mic"
	if got := updateDirFor(&cfg, "/etc/remote-mic/config.yaml"); got != "/var/lib/remote-mic/update" {
		t.Errorf("cert dir: %q", got)
	}
}

func TestFileListHas(t *testing.T) {
	t.Parallel()
	list := filepath.Join(t.TempDir(), "birdnet-go-remote-mic.list")
	if err := os.WriteFile(list, []byte("/.\n/usr\n/usr/bin\n/usr/bin/remote-mic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileListHas(list, "/usr/bin/remote-mic") {
		t.Error("listed path not found")
	}
	if fileListHas(list, "/usr/local/bin/remote-mic") || fileListHas(list, "/usr/bin/remote") {
		t.Error("unlisted path found")
	}
	if fileListHas(filepath.Join(t.TempDir(), "missing.list"), "/usr/bin/remote-mic") {
		t.Error("missing list reported a path")
	}
}

// TestNewUpdateManager pins the production wiring: the running version, and
// a test binary (unpacked, not a service install) that cannot update itself.
func TestNewUpdateManager(t *testing.T) {
	t.Parallel()
	m := newUpdateManager(t.Context(), filepath.Join(t.TempDir(), "update"), nil)
	if m == nil {
		t.Fatal("newUpdateManager returned nil")
	}
	st := m.Status()
	if st.Current != version || st.Install.CanApply || st.Install.Hint == "" {
		t.Errorf("status %+v", st)
	}
}
