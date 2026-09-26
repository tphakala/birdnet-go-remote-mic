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

// TestBootWatchesPendingUpdate pins that a boot with a fresh request (this
// process is the new version the updater is waiting on) leaves the staged
// files alone, reports a result the updater writes only after the boot (the
// normal order: the updater waits for this process's health file first), and
// stops watching once the updater's wait has passed.
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
// while an attempt is in flight (its owner may still read it) and dropped,
// unreported, when nothing is.
func TestBootResultForAnotherVersion(t *testing.T) {
	t.Parallel()
	for _, inFlight := range []bool{true, false} {
		// A bubble, so the in-flight watcher has settled before the checks.
		synctest.Test(t, func(t *testing.T) {
			dir := t.TempDir()
			if inFlight {
				taken := filepath.Join(dir, TakenFile)
				writeJSON(t, taken, Request{Version: vNew})
				now := time.Now()
				if err := os.Chtimes(taken, now, now); err != nil {
					t.Fatal(err)
				}
			}
			// The rolled-back result belongs to the restored old version.
			writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeRolledBack, From: vOld, To: vNew, Installed: vOld})
			c := notify.NewCenter()
			Boot(t.Context(), dir, vNew, c, t.Logf)
			time.Sleep(3 * resultPoll)
			synctest.Wait()
			if n := len(c.Snapshot().Notifications); n != 0 {
				t.Errorf("in flight %t: %d notifications, want none", inFlight, n)
			}
			if got := exists(filepath.Join(dir, StatusFile)); got != inFlight {
				t.Errorf("in flight %t: status file present %t, want %t", inFlight, got, inFlight)
			}
		})
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
