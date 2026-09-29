package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
// request, or with an abandoned one, are removed at boot.
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
		logs := &logSink{}
		Boot(t.Context(), dir, vOld, nil, logs.logf)
		if n, want := logs.count("abandoned since"), map[bool]int{false: 0, true: 2}[stale]; n != want {
			t.Errorf("stale request %t: %d abandoned requests logged, want %d", stale, n, want)
		}
		for _, name := range []string{BinaryFile, ManifestFile, SignatureFile, downloadFile, RequestFile, TakenFile} {
			if exists(filepath.Join(dir, name)) {
				t.Errorf("stale request %t: %s left behind", stale, name)
			}
		}
	}
}

// TestBootWatchesPendingUpdate pins that a boot with a fresh request (one
// this process wrote after it came up, since WithdrawOrphanedRequest clears an
// earlier run's) leaves the staged files alone, reports a result written after
// the boot, and stops watching once the updater's wait has passed.
func TestBootWatchesPendingUpdate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		req := filepath.Join(dir, RequestFile)
		writeJSON(t, req, Request{Version: vNew})
		staged := time.Now().Add(-time.Minute) // staged a moment ago
		if err := os.Chtimes(req, staged, staged); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, BinaryFile), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := notify.NewCenter()
		Boot(t.Context(), dir, vNew, c, t.Logf)
		synctest.Wait() // the watcher has looked once and found nothing
		if !exists(filepath.Join(dir, BinaryFile)) {
			t.Fatal("Boot removed the staged binary of a pending update")
		}
		status := Result{Outcome: OutcomeUpdated, From: vOld, To: vNew, Installed: vNew}
		writeJSON(t, filepath.Join(dir, StatusFile), status)
		time.Sleep(resultPoll + time.Millisecond)
		synctest.Wait()
		if snap := c.Snapshot(); len(snap.Notifications) != 1 || snap.Notifications[0].Title != titleUpdated {
			t.Fatalf("notifications %+v", snap.Notifications)
		}
	})
}

// TestBootWatcherGivesUp pins that the watcher stops once the updater's wait
// has passed: a result written after that is left for the next boot.
func TestBootWatcherGivesUp(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		req := filepath.Join(dir, RequestFile)
		writeJSON(t, req, Request{Version: vNew})
		now := time.Now()
		if err := os.Chtimes(req, now, now); err != nil {
			t.Fatal(err)
		}
		c := notify.NewCenter()
		Boot(t.Context(), dir, vNew, c, t.Logf)
		synctest.Wait()
		if !exists(req) {
			t.Fatal("Boot treated the fresh request as abandoned instead of watching")
		}
		time.Sleep(DefaultHealthTimeout + 2*time.Minute)
		synctest.Wait()
		writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeUpdated, From: vOld, To: vNew, Installed: vNew})
		time.Sleep(2 * resultPoll)
		synctest.Wait()
		if n := len(c.Snapshot().Notifications); n != 0 {
			t.Errorf("%d notifications after the watcher's limit, want none", n)
		}
	})
}

// TestReadResultRefusesLinkAndGarbage pins that a linked status file is not
// followed (nor its target removed) and a malformed one is refused and removed.
func TestReadResultRefusesLinkAndGarbage(t *testing.T) {
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
// while an attempt is in flight (its owner may still read it) and when it is
// the updater's rollback away from this version (the restored version reports
// it), and dropped, unreported, otherwise.
func TestBootResultForAnotherVersion(t *testing.T) {
	t.Parallel()
	rolledBackFromNew := Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Installed: vOld}
	tests := []struct {
		name     string
		inFlight bool
		res      Result
		wantKept bool
	}{
		{"in flight", true, rolledBackFromNew, true},
		{"rollback away from this version", false, rolledBackFromNew, true},
		{"rollback away from another version", false, Result{Outcome: OutcomeRolledBack, From: "v0.1.0", To: vOld, Installed: "v0.1.0"}, false},
		{"failure for another version", false, Result{Outcome: OutcomeFailed, From: vOld, To: vNew, Installed: vOld}, false},
		{"rollback with no installed version", false, Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew}, false},
	}
	for _, tt := range tests {
		// A bubble, so the in-flight watcher has settled before the checks.
		synctest.Test(t, func(t *testing.T) {
			dir := t.TempDir()
			if tt.inFlight {
				taken := filepath.Join(dir, TakenFile)
				writeJSON(t, taken, Request{Version: vNew})
				now := time.Now()
				if err := os.Chtimes(taken, now, now); err != nil {
					t.Fatal(err)
				}
			}
			writeJSON(t, filepath.Join(dir, StatusFile), tt.res)
			c := notify.NewCenter()
			Boot(t.Context(), dir, vNew, c, t.Logf)
			time.Sleep(3 * resultPoll)
			synctest.Wait()
			if n := len(c.Snapshot().Notifications); n != 0 {
				t.Errorf("%s: %d notifications, want none", tt.name, n)
			}
			if got := exists(filepath.Join(dir, StatusFile)); got != tt.wantKept {
				t.Errorf("%s: status file present %t, want %t", tt.name, got, tt.wantKept)
			}
		})
	}
}

// TestBootKeepsTheRollbackForTheRestoredVersion pins the reboot mid-install
// case: the new version boots while the updater's recovery rolls it back, and
// the claim looks abandoned (a long power cut, or a clock restored far off).
// The new version must leave the rollback result for the restored version,
// which reports it once.
func TestBootKeepsTheRollbackForTheRestoredVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		claimAge time.Duration
	}{
		{"claim two hours old", 2 * time.Hour},
		{"claim an hour in the future", -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			taken := filepath.Join(dir, TakenFile)
			writeJSON(t, taken, Request{Version: vNew})
			when := time.Now().Add(-tc.claimAge)
			if err := os.Chtimes(taken, when, when); err != nil {
				t.Fatal(err)
			}
			status := filepath.Join(dir, StatusFile)
			writeJSON(t, status, Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Installed: vOld, Reason: "power lost mid-install"})

			c := notify.NewCenter()
			Boot(t.Context(), dir, vNew, c, t.Logf)
			if n := len(c.Snapshot().Notifications); n != 0 {
				t.Fatalf("the rolled-back version published %d notifications, want none", n)
			}
			if !exists(status) {
				t.Fatal("the rolled-back version dropped the result meant for the restored one")
			}

			Boot(t.Context(), dir, vOld, c, t.Logf)
			snap := c.Snapshot()
			if len(snap.Notifications) != 1 || snap.Notifications[0].Title != "Update rolled back" {
				t.Fatalf("notifications %+v, want one rollback report", snap.Notifications)
			}
			if exists(status) {
				t.Error("the restored version left the reported result in place")
			}
		})
	}
}

// TestWithdrawOrphanedRequest pins that a request left unclaimed by an
// earlier run is removed and reported as a failed update, while a claim, a
// missing request and a missing staging directory are left alone.
func TestWithdrawOrphanedRequest(t *testing.T) {
	t.Parallel()
	t.Run("request", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		req := filepath.Join(dir, RequestFile)
		writeJSON(t, req, Request{Version: vNew})
		c := notify.NewCenter()
		logs := &logSink{}
		WithdrawOrphanedRequest(dir, vOld, c, logs.logf)
		if exists(req) {
			t.Error("the request is still there")
		}
		snap := c.Snapshot()
		if len(snap.Notifications) != 1 || snap.Notifications[0].Title != "Update failed" || !strings.Contains(snap.Notifications[0].Message, vNew) {
			t.Fatalf("notifications %+v, want one failure naming %s", snap.Notifications, vNew)
		}
		if n := logs.count("withdrew"); n != 1 {
			t.Errorf("%d withdrawal log lines, want 1: %q", n, logs.lines)
		}
	})
	t.Run("unreadable request", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		req := filepath.Join(dir, RequestFile)
		if err := os.WriteFile(req, []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := notify.NewCenter()
		WithdrawOrphanedRequest(dir, vOld, nil, t.Logf)
		WithdrawOrphanedRequest(dir, vOld, c, t.Logf)
		if exists(req) {
			t.Error("the malformed request is still there")
		}
		if n := len(c.Snapshot().Notifications); n != 0 {
			t.Errorf("%d notifications for a request already withdrawn, want none", n)
		}
	})
	t.Run("claim", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		taken := filepath.Join(dir, TakenFile)
		writeJSON(t, taken, Request{Version: vNew})
		c := notify.NewCenter()
		WithdrawOrphanedRequest(dir, vOld, c, t.Logf)
		if !exists(taken) {
			t.Error("the updater's claim was removed")
		}
		if n := len(c.Snapshot().Notifications); n != 0 {
			t.Errorf("%d notifications, want none", n)
		}
	})
	t.Run("staging directory it cannot change", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		for _, tc := range []struct {
			mode os.FileMode
			want string
		}{
			{0o500, "withdraw an unclaimed update request"},
			{0, "look for an unclaimed update request"},
		} {
			dir := t.TempDir()
			req := filepath.Join(dir, RequestFile)
			writeJSON(t, req, Request{Version: vNew})
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			c := notify.NewCenter()
			logs := &logSink{}
			WithdrawOrphanedRequest(dir, vOld, c, logs.logf)
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if n := logs.count(tc.want); n != 1 {
				t.Errorf("mode %o: log %q, want one line with %q", tc.mode, logs.lines, tc.want)
			}
			if n := len(c.Snapshot().Notifications); n != 0 {
				t.Errorf("mode %o: %d notifications for a request not withdrawn, want none", tc.mode, n)
			}
			if !exists(req) {
				t.Errorf("mode %o: the request is gone", tc.mode)
			}
		}
	})
	t.Run("no staging directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), DirName)
		logs := &logSink{}
		WithdrawOrphanedRequest(dir, vOld, nil, logs.logf)
		if n := logs.count("update:"); n != 0 {
			t.Errorf("%d log lines, want none: %q", n, logs.lines)
		}
	})
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

// TestAttemptsAges pins how long a request and the updater's claim count as
// in flight: the request until the appliance would have withdrawn it (plus a
// margin), the claim for the updater unit's lifetime (plus a margin), and
// neither when dated in the future.
func TestAttemptsAges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
		age  time.Duration
		live bool
	}{
		{"fresh request", RequestFile, 110 * time.Second, true},
		{"request past its withdrawal", RequestFile, 130 * time.Second, false},
		{"claim mid-install", TakenFile, 14 * time.Minute, true},
		{"claim past the updater's lifetime", TakenFile, 16 * time.Minute, false},
		{"request from the future", RequestFile, -time.Hour, false},
		{"claim from the future", TakenFile, -time.Hour, false},
		{"request after a small clock step back", RequestFile, -10 * time.Second, true},
		{"claim after a small clock step back", TakenFile, -10 * time.Second, true},
		{"claim after a big clock step back", TakenFile, -16 * time.Minute, false},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		p := filepath.Join(dir, tt.file)
		writeJSON(t, p, Request{Version: vNew})
		mt := time.Now().Add(-tt.age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		live, abandoned := attempts(dir)
		if live != tt.live || len(abandoned) != map[bool]int{true: 0, false: 1}[tt.live] {
			t.Errorf("%s: live %t with %d abandoned, want live %t", tt.name, live, len(abandoned), tt.live)
		}
		if got := inFlight(dir); got != tt.live {
			t.Errorf("%s: inFlight %t, want %t", tt.name, got, tt.live)
		}
	}
}
