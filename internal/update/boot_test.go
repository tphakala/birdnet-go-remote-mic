package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestBootWithoutStagingDirDoesNothing pins that an install without the
// staging directory (no root updater) gets no health write attempt and no
// log noise on every boot.
func TestBootWithoutStagingDirDoesNothing(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), DirName)
	logs := &logSink{}
	Boot(t.Context(), dir, vOld, nil, logs.logf)
	if exists(dir) {
		t.Error("Boot created the staging directory")
	}
	if n := logs.count("update:"); n != 0 {
		t.Errorf("Boot logged %d lines without a staging directory: %q", n, logs.lines)
	}
}

func TestBootReportsResultAndWritesHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		res      Result
		title    string
		severity notify.Severity
	}{
		{Result{Outcome: OutcomeUpdated, From: vOld, To: vNew}, "Updated to v0.3.0", notify.SeverityInfo},
		{Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Reason: "timeout"}, "Update rolled back", notify.SeverityWarning},
		{Result{Outcome: OutcomeFailed, From: vOld, To: vNew, Reason: "bad"}, "Update failed", notify.SeverityWarning},
	}
	for _, tt := range tests {
		t.Run(string(tt.res.Outcome), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeJSON(t, filepath.Join(dir, StatusFile), tt.res)
			c := notify.NewCenter()
			Boot(t.Context(), dir, vNew, c, t.Logf)
			snap := c.Snapshot()
			if len(snap.Notifications) != 1 || snap.Notifications[0].Title != tt.title || snap.Notifications[0].Severity != tt.severity {
				t.Errorf("notifications %+v, want one %q (%s)", snap.Notifications, tt.title, tt.severity)
			}
			if exists(filepath.Join(dir, StatusFile)) {
				t.Error("status file not removed after reporting")
			}
			var h Health
			b, err := os.ReadFile(filepath.Join(dir, HealthFile))
			if err != nil || json.Unmarshal(b, &h) != nil || h.Version != vNew || h.PID != os.Getpid() {
				t.Errorf("health file %q, %v", b, err)
			}
		})
	}
}

// TestBootCleansAbandonedAttempt pins that staged files with no pending
// request, or with a request older than an hour, are removed at boot.
func TestBootCleansAbandonedAttempt(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		dir := t.TempDir()
		for _, name := range []string{BinaryFile, ManifestFile, SignatureFile, downloadFile} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if stale {
			req := filepath.Join(dir, RequestFile)
			writeJSON(t, req, Request{Version: vNew})
			old := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(req, old, old); err != nil {
				t.Fatal(err)
			}
		}
		Boot(t.Context(), dir, vOld, nil, t.Logf)
		for _, name := range []string{BinaryFile, ManifestFile, SignatureFile, downloadFile, RequestFile} {
			if exists(filepath.Join(dir, name)) {
				t.Errorf("stale request %t: %s left behind", stale, name)
			}
		}
	}
}

// TestBootWatchesPendingUpdate pins that a boot with a fresh request (this
// process is the new version the updater is waiting on) leaves the staged
// files alone and reports the result once the updater writes it.
func TestBootWatchesPendingUpdate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		writeJSON(t, filepath.Join(dir, RequestFile), Request{Version: vNew})
		if err := os.WriteFile(filepath.Join(dir, BinaryFile), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := notify.NewCenter()
		Boot(t.Context(), dir, vNew, c, t.Logf)
		if !exists(filepath.Join(dir, BinaryFile)) {
			t.Fatal("Boot removed the staged binary of a pending update")
		}
		writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeUpdated, From: vOld, To: vNew})
		time.Sleep(resultPoll + time.Millisecond)
		synctest.Wait()
		if snap := c.Snapshot(); len(snap.Notifications) != 1 || snap.Notifications[0].Title != "Updated to v0.3.0" {
			t.Errorf("notifications %+v", snap.Notifications)
		}
		// The watcher gives up once the updater's wait has passed.
		time.Sleep(DefaultHealthTimeout + 2*time.Minute)
		synctest.Wait()
	})
}

func TestTakeResultRefusesLinkAndGarbage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	writeJSON(t, target, Result{Outcome: OutcomeUpdated})
	if err := os.Symlink(target, filepath.Join(dir, StatusFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := takeResult(dir); err == nil {
		t.Error("a symlinked status file was read")
	}
	if !exists(target) {
		t.Error("the link target was removed")
	}
	if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := takeResult(dir); err == nil {
		t.Error("a malformed status file was accepted")
	}
	if _, err := takeResult(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("malformed status file not removed: %v", err)
	}
}
