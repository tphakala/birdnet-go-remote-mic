package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// titleUpdated is the notification title of a successful update to vNew.
const titleUpdated = "Updated to v0.3.0"

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
		{Result{Outcome: OutcomeUpdated, From: vOld, To: vNew, Installed: vNew}, titleUpdated, notify.SeverityInfo},
		{Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Installed: vOld, Reason: "timeout"}, "Update rolled back", notify.SeverityWarning},
		{Result{Outcome: OutcomeFailed, From: vOld, To: vNew, Installed: vOld, Reason: "bad"}, "Update failed", notify.SeverityWarning},
	}
	for _, tt := range tests {
		t.Run(string(tt.res.Outcome), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeJSON(t, filepath.Join(dir, StatusFile), tt.res)
			c := notify.NewCenter()
			Boot(t.Context(), dir, tt.res.Installed, c, t.Logf)
			snap := c.Snapshot()
			if len(snap.Notifications) != 1 || snap.Notifications[0].Title != tt.title || snap.Notifications[0].Severity != tt.severity {
				t.Errorf("notifications %+v, want one %q (%s)", snap.Notifications, tt.title, tt.severity)
			}
			if exists(filepath.Join(dir, StatusFile)) {
				t.Error("status file not removed after reporting")
			}
			var h Health
			b, err := os.ReadFile(filepath.Join(dir, HealthFile))
			if err != nil || json.Unmarshal(b, &h) != nil || h.Version != tt.res.Installed || h.PID != os.Getpid() {
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
			for _, name := range []string{RequestFile, TakenFile} {
				req := filepath.Join(dir, name)
				writeJSON(t, req, Request{Version: vNew})
				old := time.Now().Add(-2 * time.Hour)
				if err := os.Chtimes(req, old, old); err != nil {
					t.Fatal(err)
				}
			}
		}
		Boot(t.Context(), dir, vOld, nil, t.Logf)
		for _, name := range []string{BinaryFile, ManifestFile, SignatureFile, downloadFile, RequestFile, TakenFile} {
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
		writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeUpdated, From: vOld, To: vNew, Installed: vNew})
		time.Sleep(resultPoll + time.Millisecond)
		synctest.Wait()
		if snap := c.Snapshot(); len(snap.Notifications) != 1 || snap.Notifications[0].Title != titleUpdated {
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
	if _, err := readResult(dir); err == nil {
		t.Error("a symlinked status file was read")
	}
	if !exists(target) {
		t.Error("the link target was removed")
	}
	if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readResult(dir); err == nil {
		t.Error("a malformed status file was accepted")
	}
	if _, err := readResult(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("malformed status file not removed: %v", err)
	}
}

// TestResultMessage pins that a failed result says whether the new version
// stayed installed (a restore that failed) or was never installed.
func TestResultMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		res  Result
		want string
	}{
		{Result{Outcome: OutcomeFailed, From: vOld, To: vNew, Installed: vOld, Reason: "bad"}, "The update to v0.3.0 was not installed: bad"},
		{Result{Outcome: OutcomeFailed, From: vOld, To: vNew, Installed: vNew, Reason: "gone"}, "v0.3.0 did not come up and v0.2.0 could not be restored: gone"},
		{Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Installed: vOld, Reason: "slow"}, "v0.3.0 did not come up, so v0.2.0 was restored: slow"},
		// A request the updater could not read names no version.
		{Result{Outcome: OutcomeFailed, From: vOld, Reason: "unreadable"}, "The update was not installed: unreadable"},
	}
	for _, tt := range tests {
		if got := resultMessage(&tt.res); got != tt.want {
			t.Errorf("resultMessage(%+v) = %q, want %q", tt.res, got, tt.want)
		}
	}
}

// TestBootResultForAnotherVersion pins that Boot reports only results
// addressed to its own version: one for another version is left in place
// while an attempt is in flight (its owner may still read it) and dropped,
// unreported, when nothing is.
func TestBootResultForAnotherVersion(t *testing.T) {
	t.Parallel()
	for _, inFlight := range []bool{true, false} {
		dir := t.TempDir()
		if inFlight {
			writeJSON(t, filepath.Join(dir, TakenFile), Request{Version: vNew})
		}
		// The rolled-back result belongs to the restored old version.
		writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Installed: vOld})
		c := notify.NewCenter()
		ctx, cancel := context.WithCancel(t.Context())
		Boot(ctx, dir, vNew, c, t.Logf)
		cancel()
		if n := len(c.Snapshot().Notifications); n != 0 {
			t.Errorf("in flight %t: %d notifications, want none", inFlight, n)
		}
		if got := exists(filepath.Join(dir, StatusFile)); got != inFlight {
			t.Errorf("in flight %t: status file present %t, want %t", inFlight, got, inFlight)
		}
	}
}

// TestBootWatchesClaimedRequest pins that a request the updater has already
// claimed still counts as in flight, so the new version waits for its result.
func TestBootWatchesClaimedRequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		taken := filepath.Join(dir, TakenFile)
		writeJSON(t, taken, Request{Version: vNew})
		now := time.Now()
		if err := os.Chtimes(taken, now, now); err != nil {
			t.Fatal(err)
		}
		c := notify.NewCenter()
		Boot(t.Context(), dir, vNew, c, t.Logf)
		synctest.Wait()
		writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeUpdated, From: vOld, To: vNew, Installed: vNew})
		time.Sleep(resultPoll + time.Millisecond)
		synctest.Wait()
		if snap := c.Snapshot(); len(snap.Notifications) != 1 || snap.Notifications[0].Title != titleUpdated {
			t.Errorf("notifications %+v", snap.Notifications)
		}
	})
}
