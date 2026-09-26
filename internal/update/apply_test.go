package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// applyEnv is a staged release plus an Applier wired to fakes: an installed
// binary in a temp bin dir, a fake systemctl whose restart runs onRestart
// (standing in for the appliance booting and writing its health file), and a
// version command that reports what the binary's first line says.
type applyEnv struct {
	stateDir, binPath string
	rel               *release
	a                 *Applier
	mu                sync.Mutex
	restarts          int
	onRestart         func(env *applyEnv, n int)
}

// oldBinary is the installed binary's content; its first line is what the
// fake version command prints.
const oldBinary = "remote-mic v0.2.0\nold"

// newBinary is the staged release's content (see newApplyEnv).
const newBinary = "remote-mic v0.3.0\nnew"

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	priv, trusted := testKeys(t)
	env := &applyEnv{stateDir: t.TempDir()}
	binDir := t.TempDir()
	env.binPath = filepath.Join(binDir, "remote-mic")
	if err := os.WriteFile(env.binPath, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	env.rel = newRelease(t, priv, vNew, testTarget, "https://example.invalid/r.tar.gz", []byte(newBinary))
	dir := filepath.Join(env.stateDir, DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(Request{Version: vNew})
	for name, b := range map[string][]byte{
		BinaryFile: env.rel.bin, ManifestFile: env.rel.raw, SignatureFile: env.rel.sig, RequestFile: req,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// By default a restart boots whatever binary is installed and it reports
	// healthy under the version its first line names.
	env.onRestart = func(env *applyEnv, _ int) { env.bootInstalled(t) }
	env.a = &Applier{
		StateDir: env.stateDir,
		BinPath:  env.binPath,
		Unit:     "remote-mic.service",
		Running:  vOld,
		Target:   testTarget,
		Trusted:  trusted,
		Restart: func(string) error {
			env.mu.Lock()
			env.restarts++
			n := env.restarts
			env.mu.Unlock()
			env.onRestart(env, n)
			return nil
		},
		Active: func(string) (bool, error) { return true, nil },
		Version: func(_ context.Context, bin string) (string, error) {
			b, err := os.ReadFile(bin)
			if err != nil {
				return "", err
			}
			line, _, _ := strings.Cut(string(b), "\n")
			return line + "\n", nil
		},
		HealthTimeout: 200 * time.Millisecond,
		Poll:          5 * time.Millisecond,
		Logf:          t.Logf,
	}
	return env
}

// bootInstalled plays the appliance starting on the installed binary: it
// writes a health file naming the version the binary reports.
func (env *applyEnv) bootInstalled(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(env.binPath)
	if err != nil {
		t.Error(err)
		return
	}
	line, _, _ := strings.Cut(string(b), "\n")
	h, _ := json.Marshal(Health{Version: strings.TrimPrefix(line, "remote-mic "), PID: 1})
	if err := os.WriteFile(filepath.Join(env.stateDir, DirName, HealthFile), h, 0o644); err != nil {
		t.Error(err)
	}
}

func (env *applyEnv) result(t *testing.T) Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.stateDir, DirName, StatusFile))
	if err != nil {
		t.Fatalf("status file: %v", err)
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (env *applyEnv) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(env.binPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (env *applyEnv) requestGone(t *testing.T) {
	t.Helper()
	for _, name := range []string{RequestFile, TakenFile} {
		if _, err := os.Stat(filepath.Join(env.stateDir, DirName, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", name, err)
		}
	}
}

func TestApplyInstallsAndKeepsPrevious(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	journalAtRestart, journalAtStatus := false, true
	restart := env.onRestart
	claimedAtRestart := false
	env.onRestart = func(e *applyEnv, n int) {
		journalAtRestart = exists(e.binPath + ".pending")
		dir := filepath.Join(e.stateDir, DirName)
		claimedAtRestart = !exists(filepath.Join(dir, RequestFile)) && exists(filepath.Join(dir, TakenFile))
		restart(e, n)
	}
	env.a.Now = func() time.Time {
		journalAtStatus = exists(env.binPath + ".pending")
		return time.Now()
	}
	if err := env.a.Apply(t.Context()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := env.installed(t); got != string(env.rel.bin) {
		t.Errorf("installed %q, want the new binary", got)
	}
	if prev, _ := os.ReadFile(env.binPath + ".prev"); string(prev) != oldBinary {
		t.Errorf("previous binary %q not kept", prev)
	}
	if r := env.result(t); r.Outcome != OutcomeUpdated || r.From != vOld || r.To != vNew {
		t.Errorf("result %+v", r)
	}
	if env.restarts != 1 {
		t.Errorf("got %d restarts, want 1", env.restarts)
	}
	env.requestGone(t)
	if _, err := os.Stat(env.binPath + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary binary left behind: %v", err)
	}
	// The journal covers the restart and is gone before the outcome is
	// written: a crash after the status must not roll back a healthy update.
	if !claimedAtRestart {
		t.Error("the request was not claimed (renamed to the taken file) while the update ran")
	}
	if !journalAtRestart || journalAtStatus {
		t.Errorf("journal at restart %t (want true), at status %t (want false)", journalAtRestart, journalAtStatus)
	}
}

// TestApplyRollsBack pins that a new version that never reports healthy is
// replaced by the previous binary and the unit restarted on it, with the
// rolled-back result on disk before that restart.
func TestApplyRollsBack(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	var statusAtSecondRestart Outcome
	journalAtSecondRestart := true
	writes := 0
	env.a.Now = func() time.Time { writes++; return time.Now() }
	env.onRestart = func(env *applyEnv, n int) {
		if n == 2 {
			statusAtSecondRestart = env.result(t).Outcome
			journalAtSecondRestart = exists(env.binPath + ".pending")
		}
		// The new binary crash-loops: no health file ever appears.
	}
	err := env.a.Apply(t.Context())
	if err == nil || !strings.Contains(err.Error(), "rolled_back") {
		t.Fatalf("Apply: got %v, want a rollback", err)
	}
	if got := env.installed(t); got != oldBinary {
		t.Errorf("installed %q, want the previous binary restored", got)
	}
	if env.restarts != 2 {
		t.Errorf("got %d restarts, want 2", env.restarts)
	}
	if writes != 1 {
		t.Errorf("status written %d times, want once", writes)
	}
	if journalAtSecondRestart {
		t.Error("install journal still present after the rollback")
	}
	if statusAtSecondRestart != OutcomeRolledBack {
		t.Errorf("status before the restoring restart: %q, want rolled_back", statusAtSecondRestart)
	}
	if r := env.result(t); r.Outcome != OutcomeRolledBack || !strings.Contains(r.Reason, "no healthy start") {
		t.Errorf("result %+v", r)
	}
	env.requestGone(t)
}

// TestApplyIgnoresStaleHealth pins that a health file left from before the
// restart, naming the old version, does not pass for the new one.
func TestApplyIgnoresStaleHealth(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	h, _ := json.Marshal(Health{Version: vNew, PID: 1})
	if err := os.WriteFile(filepath.Join(env.stateDir, DirName, HealthFile), h, 0o644); err != nil {
		t.Fatal(err)
	}
	env.onRestart = func(*applyEnv, int) {}
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "rolled_back") {
		t.Fatalf("Apply: got %v, want a rollback", err)
	}
}

func TestApplyRefusesBeforeTouchingTheBinary(t *testing.T) {
	t.Parallel()
	_, otherTrusted := testKeys(t)
	tests := []struct {
		name   string
		mutate func(t *testing.T, env *applyEnv)
		want   string
	}{
		{
			name:   wantUntrusted,
			mutate: func(_ *testing.T, env *applyEnv) { env.a.Trusted = otherTrusted },
			want:   wantUntrusted,
		},
		{
			name:   wantNotNewer,
			mutate: func(_ *testing.T, env *applyEnv) { env.a.Running = vNew },
			want:   wantNotNewer,
		},
		{
			name:   wantNoTarget,
			mutate: func(_ *testing.T, env *applyEnv) { env.a.Target = "linux/amd64" },
			want:   wantNoTarget,
		},
		{
			name: "staged binary swapped",
			mutate: func(t *testing.T, env *applyEnv) {
				t.Helper()
				evil := []byte("remote-mic v0.3.0\nev!")
				if err := os.WriteFile(filepath.Join(env.stateDir, DirName, BinaryFile), evil, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "staged binary sha256",
		},
		{
			name: "staged binary is a symlink",
			mutate: func(t *testing.T, env *applyEnv) {
				t.Helper()
				p := filepath.Join(env.stateDir, DirName, BinaryFile)
				_ = os.Remove(p)
				if err := os.Symlink(env.binPath, p); err != nil {
					t.Fatal(err)
				}
			},
			want: "not a regular file",
		},
		{
			name: "new binary reports another version",
			mutate: func(_ *testing.T, env *applyEnv) {
				env.a.Version = func(context.Context, string) (string, error) { return "remote-mic v9.9.9\n", nil }
			},
			want: "reports",
		},
		{
			name: "new binary does not run",
			mutate: func(_ *testing.T, env *applyEnv) {
				env.a.Version = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
			},
			want: "does not run",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newApplyEnv(t)
			tt.mutate(t, env)
			err := env.a.Apply(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply: got %v, want an error containing %q", err, tt.want)
			}
			if got := env.installed(t); got != oldBinary {
				t.Errorf("installed binary changed to %q", got)
			}
			if env.restarts != 0 {
				t.Errorf("got %d restarts, want 0", env.restarts)
			}
			if r := env.result(t); r.Outcome != OutcomeFailed {
				t.Errorf("result %+v, want failed", r)
			}
			env.requestGone(t)
		})
	}
}

func TestApplyWithoutRequestDoesNothing(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	if err := os.Remove(filepath.Join(env.stateDir, DirName, RequestFile)); err != nil {
		t.Fatal(err)
	}
	if err := env.a.Apply(t.Context()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, DirName, StatusFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("status written with no request: %v", err)
	}
}

// TestApplyStatusReplacesPlantedLink pins that a symlink planted at the status
// path is replaced, not written through.
func TestApplyStatusReplacesPlantedLink(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(env.stateDir, DirName, StatusFile)); err != nil {
		t.Fatal(err)
	}
	env.a.Running = vNew // refused early, so only the status is written
	_ = env.a.Apply(t.Context())
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("the status write went through the planted link: victim now %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(env.stateDir, DirName, StatusFile)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("status file is not a regular file: %v", err)
	}
}

// TestApplyRollsBackOnRestartFailure pins that a failed restart onto the new
// binary restores the previous one.
func TestApplyRollsBackOnRestartFailure(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	restart := env.a.Restart
	calls := 0
	env.a.Restart = func(unit string) error {
		calls++
		if calls == 1 {
			return errors.New("unit failed to start")
		}
		return restart(unit)
	}
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "unit failed to start") {
		t.Fatalf("Apply: got %v", err)
	}
	if got := env.installed(t); got != oldBinary {
		t.Errorf("installed %q, want the previous binary", got)
	}
}

// TestApplyHealthNeedsActiveUnitAndVersion pins that a health file naming
// another version, or an inactive unit, is not healthy, and says which.
func TestApplyHealthNeedsActiveUnitAndVersion(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	env.onRestart = func(env *applyEnv, n int) {
		if n == 1 {
			h, _ := json.Marshal(Health{Version: "v0.2.9", PID: 1})
			_ = os.WriteFile(filepath.Join(env.stateDir, DirName, HealthFile), h, 0o644)
		}
	}
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "names v0.2.9") {
		t.Errorf("wrong version: got %v", err)
	}

	env = newApplyEnv(t)
	env.a.Active = func(string) (bool, error) { return false, nil }
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "is not active") {
		t.Errorf("inactive unit: got %v", err)
	}
}

// TestApplyRollsBackOnCancel pins that stopping the updater during the health
// wait (SIGTERM cancels ctx) restores the previous binary and reports it,
// rather than leaving the unverified new binary installed.
func TestApplyRollsBackOnCancel(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	env.onRestart = func(_ *applyEnv, n int) {
		if n == 1 {
			cancel() // stopped while the new version starts
		}
	}
	env.a.HealthTimeout = time.Minute // only the cancel can end the wait
	err := env.a.Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Apply: got %v, want a stopped rollback", err)
	}
	if got := env.installed(t); got != oldBinary {
		t.Errorf("installed %q, want the previous binary", got)
	}
	r := env.result(t)
	if r.Outcome != OutcomeRolledBack || r.Installed != vOld {
		t.Errorf("result %+v, want rolled_back with %s installed", r, vOld)
	}
	if env.restarts != 2 {
		t.Errorf("got %d restarts, want 2 (onto the new binary, then the restored one)", env.restarts)
	}
}

// TestApplyRestoreFailureWritesStatus pins that when the previous binary
// cannot be put back, the result still gets written (once), names the new
// version as installed, and the unit is still restarted.
func TestApplyRestoreFailureWritesStatus(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	writes := 0
	env.a.Now = func() time.Time { writes++; return time.Now() }
	env.onRestart = func(env *applyEnv, n int) {
		if n == 1 {
			// The new version never comes up, and the kept copy vanishes.
			_ = os.Remove(env.binPath + ".prev")
		}
	}
	err := env.a.Apply(t.Context())
	if err == nil || !strings.Contains(err.Error(), "stays installed") {
		t.Fatalf("Apply: got %v", err)
	}
	r := env.result(t)
	if r.Outcome != OutcomeFailed || r.Installed != vNew || !strings.Contains(r.Reason, "no healthy start") {
		t.Errorf("result %+v, want failed with %s installed", r, vNew)
	}
	if env.restarts != 2 {
		t.Errorf("got %d restarts, want 2", env.restarts)
	}
	if writes != 1 {
		t.Errorf("status written %d times, want once", writes)
	}
}

// TestApplyCancelledBeforeSwap pins that a stop before the binary is swapped
// leaves it untouched and restarts nothing.
func TestApplyCancelledBeforeSwap(t *testing.T) {
	t.Parallel()
	env := newApplyEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	env.a.Version = func(context.Context, string) (string, error) {
		cancel() // stopped while checking the new binary, which the stop kills
		return "", errors.New("signal: killed")
	}
	if err := env.a.Apply(ctx); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Apply: got %v", err)
	}
	if got := env.installed(t); got != oldBinary {
		t.Errorf("installed %q, want it untouched", got)
	}
	if r := env.result(t); r.Outcome != OutcomeFailed || r.Installed != vOld || env.restarts != 0 {
		t.Errorf("result %+v, %d restarts", r, env.restarts)
	}
}

// interruptedEnv is an applyEnv left as a run cut off after the swap: the
// new binary installed, the old one kept as .prev, and the journal naming
// both.
func interruptedEnv(t *testing.T) *applyEnv {
	t.Helper()
	env := newApplyEnv(t)
	if err := os.WriteFile(env.binPath+".prev", []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.binPath, env.rel.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(journal{From: vOld, To: vNew, PrevSHA256: sha([]byte(oldBinary)), NewSHA256: sha(env.rel.bin)})
	if err := os.WriteFile(env.binPath+".pending", j, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.binPath+".new", env.rel.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The cut run had claimed the request.
	dir := filepath.Join(env.stateDir, DirName)
	if err := os.Rename(filepath.Join(dir, RequestFile), filepath.Join(dir, TakenFile)); err != nil {
		t.Fatal(err)
	}
	env.a.Running = vNew // recovery runs in the new binary
	return env
}

// TestApplyRecoversInterruptedInstall pins the journal recovery: whatever
// point the cut fell at, the next start reports the interrupted update, puts
// the previous binary back when it can, removes the journal and the request,
// restarts the unit, and does not install anything.
func TestApplyRecoversInterruptedInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(t *testing.T, env *applyEnv)
		outcome   Outcome
		installed string
		binary    string
	}{
		{name: "new binary installed", outcome: OutcomeRolledBack, installed: vOld, binary: oldBinary},
		{
			name: "rollback already renamed",
			setup: func(t *testing.T, env *applyEnv) {
				t.Helper()
				if err := os.Rename(env.binPath+".prev", env.binPath); err != nil {
					t.Fatal(err)
				}
			},
			outcome: OutcomeRolledBack, installed: vOld, binary: oldBinary,
		},
		{
			name: "kept copy missing",
			setup: func(t *testing.T, env *applyEnv) {
				t.Helper()
				_ = os.Remove(env.binPath + ".prev")
			},
			outcome: OutcomeFailed, installed: vNew, binary: newBinary,
		},
		{
			name: "installed binary is neither version",
			setup: func(t *testing.T, env *applyEnv) {
				t.Helper()
				if err := os.WriteFile(env.binPath, []byte("hand-installed"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			outcome: OutcomeFailed, installed: vNew, binary: "hand-installed",
		},
		{
			name: "journal without hashes",
			setup: func(t *testing.T, env *applyEnv) {
				t.Helper()
				if err := os.WriteFile(env.binPath+".pending", []byte(`{"from":"v0.2.0","to":"v0.3.0"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			outcome: OutcomeFailed, installed: vNew, binary: newBinary,
		},
		{
			name: "unreadable journal",
			setup: func(t *testing.T, env *applyEnv) {
				t.Helper()
				if err := os.WriteFile(env.binPath+".pending", []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			outcome: OutcomeFailed, installed: vNew, binary: newBinary,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := interruptedEnv(t)
			if tt.setup != nil {
				tt.setup(t, env)
			}
			_ = env.a.Apply(t.Context())
			if got := env.installed(t); got != tt.binary {
				t.Errorf("installed %q, want %q", got, tt.binary)
			}
			r := env.result(t)
			if r.Outcome != tt.outcome || r.Installed != tt.installed || !strings.Contains(r.Reason, "interrupted") {
				t.Errorf("result %+v, want %s with %q installed", r, tt.outcome, tt.installed)
			}
			if !strings.Contains(tt.name, "journal") && (r.From != vOld || r.To != vNew) {
				t.Errorf("result %+v, want %s with %q installed", r, tt.outcome, tt.installed)
			}
			if exists(env.binPath + ".pending") {
				t.Error("journal left behind")
			}
			if exists(env.binPath + ".new") {
				t.Error("staged copy left behind")
			}
			if env.restarts != 1 {
				t.Errorf("got %d restarts, want 1", env.restarts)
			}
			env.requestGone(t)
		})
	}
}
