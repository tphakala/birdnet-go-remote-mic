//go:build linux

package service

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

func TestWithdrawStagedRequest(t *testing.T) {
	t.Parallel()
	t.Run("removes the request and leaves staged files", func(t *testing.T) {
		t.Parallel()
		state := t.TempDir()
		dir := filepath.Join(state, UpdateDirName)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(dir, "remote-mic"+update.NewSuffix)
		for _, p := range []string{filepath.Join(dir, update.RequestFile), keep} {
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := withdrawStagedRequest(state)
		if err != nil || !got {
			t.Fatalf("got (%t, %v), want (true, nil)", got, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, update.RequestFile)); !os.IsNotExist(err) {
			t.Error("the request is still there")
		}
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("a staged file was removed: %v", err)
		}
	})
	t.Run("nothing to withdraw", func(t *testing.T) {
		t.Parallel()
		state := t.TempDir()
		if err := os.Mkdir(filepath.Join(state, UpdateDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		if got, err := withdrawStagedRequest(state); err != nil || got {
			t.Errorf("got (%t, %v), want (false, nil)", got, err)
		}
	})
	t.Run("a link at the staging directory is not followed", func(t *testing.T) {
		t.Parallel()
		state, outside := t.TempDir(), t.TempDir()
		victim := filepath.Join(outside, update.RequestFile)
		if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(state, UpdateDirName)); err != nil {
			t.Fatal(err)
		}
		if got, err := withdrawStagedRequest(state); err == nil || got {
			t.Errorf("got (%t, %v), want an error", got, err)
		}
		if _, err := os.Lstat(victim); err != nil {
			t.Errorf("the file behind the link was removed: %v", err)
		}
	})
}

// withdrawTestInstaller is testInstaller with the host's login shell pinned,
// and the events it records.
func withdrawTestInstaller(t *testing.T) (*Installer, *[]string) {
	t.Helper()
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = func(p string) bool { return p == nologinPath }
	events := new([]string)
	userThere := false
	return testInstaller(events, &fakeInit{events: events, present: true}, &userThere), events
}

func TestInstallWithdrawsALeftoverRequestBeforeEnablingTheUpdater(t *testing.T) {
	in, events := withdrawTestInstaller(t)
	var warned bytes.Buffer
	in.warn = &warned
	in.withdrawRequest = func(d string) (bool, error) { *events = append(*events, "withdraw "+d); return true, nil }
	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantOrder(t, *events, "staging /var/lib/remote-mic 990:990", "withdraw /var/lib/remote-mic", evReload, "enable --now remote-mic-update.path")
	if !strings.Contains(warned.String(), "update request") {
		t.Errorf("warning %q does not mention the update request", warned.String())
	}
}

func TestInstallStopsWhenTheRequestCannotBeWithdrawn(t *testing.T) {
	in, events := withdrawTestInstaller(t)
	in.withdrawRequest = func(string) (bool, error) { return false, errors.New("boom") }
	if err := in.Install(true); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v, want the withdrawal error", err)
	}
	for _, e := range *events {
		if e == evReload || strings.HasPrefix(e, "enable") {
			t.Errorf("event %q ran after the withdrawal failed", e)
		}
	}
}

func TestCheckNotOverBinDirLooksAtTheStagingDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	state := filepath.Join(root, "state")
	for _, d := range []string{bin, state, filepath.Join(root, "etc")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := ServiceSpec{BinPath: filepath.Join(bin, "remote-mic"), ConfigPath: filepath.Join(root, "etc", "config.yaml"), StateDir: state}
	if err := checkNotOverBinDir(s); err != nil {
		t.Fatalf("a plain staging directory: %v", err)
	}
	if err := os.Symlink(bin, filepath.Join(state, UpdateDirName)); err != nil {
		t.Fatal(err)
	}
	if err := checkNotOverBinDir(s); err == nil || !strings.Contains(err.Error(), "update staging directory") {
		t.Errorf("staging directory linked to the bin directory: got %v", err)
	}
}
