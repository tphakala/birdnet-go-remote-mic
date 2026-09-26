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
	"testing/synctest"
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
	// mainPID is what the fake systemd reports as the unit's main process;
	// a restart that boots the installed binary gives it a new PID.
	mainPID int
	// units records the unit every systemd call named.
	units []string
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
		Restart: func(unit string) error {
			env.units = append(env.units, unit)
			env.mu.Lock()
			env.restarts++
			n := env.restarts
			env.mu.Unlock()
			env.onRestart(env, n)
			return nil
		},
		MainPID: func(unit string) (int, error) {
			env.units = append(env.units, unit)
			return env.mainPID, nil
		},
		Version: func(_ context.Context, bin string) (string, error) {
			b, err := os.ReadFile(bin)
			if err != nil {
				return "", err
			}
			line, _, _ := strings.Cut(string(b), "\n")
			return line + "\n", nil
		},
		// The test's temporary directories belong to the test user; stand
		// in root as their owner (TestApplyRefusesUntrustedBinDir covers the
		// real check).
		Owner:         func(os.FileInfo) (uint32, uint32, bool) { return 0, 0, true },
		HealthTimeout: 200 * time.Millisecond,
		HealthSettle:  20 * time.Millisecond,
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
	env.mainPID = 1000 + env.restarts
	h, _ := json.Marshal(Health{Version: strings.TrimPrefix(line, "remote-mic "), PID: env.mainPID})
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
	for _, u := range env.units {
		if u != env.a.Unit {
			t.Errorf("systemd was asked about %q, want %q", u, env.a.Unit)
		}
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

// TestApplyHealthNeedsRunningUnitAndVersion pins that a health file naming
// another version, or a unit with no main process, is not healthy, and says
// which.
func TestApplyHealthNeedsRunningUnitAndVersion(t *testing.T) {
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
	env.onRestart = func(e *applyEnv, _ int) {
		h, _ := json.Marshal(Health{Version: vNew})
		_ = os.WriteFile(filepath.Join(e.stateDir, DirName, HealthFile), h, 0o644)
	}
	env.a.MainPID = func(string) (int, error) { return 0, nil } // not running
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "runs as 0") {
		t.Errorf("health naming no process, unit not running: got %v", err)
	}

	env = newApplyEnv(t)
	env.a.MainPID = func(string) (int, error) { return 0, errors.New("bus unavailable") }
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "bus unavailable") {
		t.Errorf("main process query failing: got %v", err)
	}

	env = newApplyEnv(t)
	env.onRestart = func(e *applyEnv, _ int) {
		_ = os.WriteFile(filepath.Join(e.stateDir, DirName, HealthFile), []byte("{"), 0o644)
	}
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("malformed health file: got %v", err)
	}

	env = newApplyEnv(t)
	env.a.MainPID = func(string) (int, error) { return 0, nil } // not running
	if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "runs as 0") {
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

// TestApplyHealthNeedsStablePID pins the health handshake against a new
// version that does not stay up: a health file from a process that is not
// the unit's main process (an earlier, crashed incarnation) is not healthy,
// and neither is a main process that keeps changing (a crash loop), however
// often each incarnation writes a fresh health file.
func TestApplyHealthNeedsStablePID(t *testing.T) {
	t.Parallel()
	t.Run("stale health file", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			env := newApplyEnv(t)
			restart := env.onRestart
			env.onRestart = func(e *applyEnv, n int) {
				restart(e, n)
				if n == 1 {
					e.mainPID++ // it crashed and systemd started another one
				}
			}
			err := env.a.Apply(t.Context())
			if err == nil || !strings.Contains(err.Error(), "is from process") {
				t.Fatalf("Apply: got %v, want a rollback naming the stale process", err)
			}
			if got := env.installed(t); got != oldBinary {
				t.Errorf("installed %q, want the previous binary", got)
			}
		})
	})
	t.Run("crash loop", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			env := newApplyEnv(t)
			dir := filepath.Join(env.stateDir, DirName)
			// Each incarnation starts, writes its health file, and is seen as the
			// main process on a few polls (about 10 ms) before it dies, well
			// inside the 40 ms settle.
			env.a.HealthSettle = 40 * time.Millisecond
			calls := 0
			env.a.MainPID = func(string) (int, error) {
				pid := 6000 + calls/3
				if calls%3 == 0 {
					h, _ := json.Marshal(Health{Version: vNew, PID: pid})
					_ = os.WriteFile(filepath.Join(dir, HealthFile), h, 0o644)
				}
				calls++
				return pid, nil
			}
			env.onRestart = func(*applyEnv, int) {
				h, _ := json.Marshal(Health{Version: vNew, PID: 6000})
				_ = os.WriteFile(filepath.Join(dir, HealthFile), h, 0o644)
			}
			err := env.a.Apply(t.Context())
			if err == nil || !strings.Contains(err.Error(), "rolled_back") {
				t.Fatalf("Apply: got %v, want a rollback for an unstable process", err)
			}
			if got := env.installed(t); got != oldBinary {
				t.Errorf("installed %q, want the previous binary", got)
			}
		})
	})
	t.Run("late but stable start", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			env := newApplyEnv(t)
			env.a.HealthSettle = 60 * time.Millisecond
			start := time.Now()
			env.a.MainPID = func(string) (int, error) {
				// Up and stable only from 170 ms of the 200 ms timeout (fake clock), so its
				// settle ends after the timeout: it must still be kept.
				if time.Since(start) < 170*time.Millisecond {
					return 0, nil
				}
				return env.mainPID, nil
			}
			if err := env.a.Apply(t.Context()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := env.installed(t); got != newBinary {
				t.Errorf("installed %q, want the new binary", got)
			}
		})
	})
}

// TestApplyRefusesUntrustedBinDir pins that the updater installs nothing, and
// does not even run a pending recovery, when the bin directory or binary is
// writable by anyone but root; a group-writable directory owned by root's
// group is accepted.
func TestApplyRefusesUntrustedBinDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		uid, gid uint32
		dirMode  os.FileMode
		want     string // "" means the update goes ahead
	}{
		{name: "owned by a user", uid: 1000, dirMode: 0o755, want: "not root"},
		{name: "world-writable", dirMode: 0o757, want: "writable by everyone"},
		{name: "group-writable by staff", gid: 50, dirMode: 0o775, want: "writable by group 50"},
		{name: "group-writable by root", gid: 0, dirMode: 0o775},
		{name: "another group, not writable by it", gid: 50, dirMode: 0o755},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newApplyEnv(t)
			if err := os.Chmod(filepath.Dir(env.binPath), tt.dirMode); err != nil {
				t.Fatal(err)
			}
			env.a.Owner = func(os.FileInfo) (uint32, uint32, bool) { return tt.uid, tt.gid, true }
			err := env.a.Apply(t.Context())
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply: got %v, want %q", err, tt.want)
			}
			if got := env.installed(t); got != oldBinary || env.restarts != 0 {
				t.Errorf("installed %q with %d restarts, want nothing touched", got, env.restarts)
			}
			env.requestGone(t)
		})
	}
	t.Run("only the binary owned by a user", func(t *testing.T) {
		t.Parallel()
		env := newApplyEnv(t)
		binFI, err := os.Lstat(env.binPath)
		if err != nil {
			t.Fatal(err)
		}
		env.a.Owner = func(fi os.FileInfo) (uint32, uint32, bool) {
			if os.SameFile(fi, binFI) {
				return 1000, 1000, true
			}
			return 0, 0, true
		}
		if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "owned by uid 1000") {
			t.Errorf("Apply: got %v", err)
		}
	})
	t.Run("owner unknown", func(t *testing.T) {
		t.Parallel()
		env := newApplyEnv(t)
		env.a.Owner = func(os.FileInfo) (uint32, uint32, bool) { return 0, 0, false }
		if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "cannot tell who owns") {
			t.Errorf("Apply: got %v", err)
		}
	})
	t.Run("binary is a symlink", func(t *testing.T) {
		t.Parallel()
		env := newApplyEnv(t)
		target := filepath.Join(t.TempDir(), "remote-mic")
		if err := os.Rename(env.binPath, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, env.binPath); err != nil {
			t.Fatal(err)
		}
		if err := env.a.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Apply: got %v", err)
		}
	})
	t.Run("recovery waits too", func(t *testing.T) {
		t.Parallel()
		env := interruptedEnv(t)
		env.a.Owner = func(os.FileInfo) (uint32, uint32, bool) { return 1000, 1000, true }
		err := env.a.Apply(t.Context())
		if err == nil || !strings.Contains(err.Error(), "interrupted update is waiting") {
			t.Errorf("Apply: got %v, want the pending rollback named", err)
		}
		if got := env.installed(t); got != newBinary || !exists(env.binPath+".pending") {
			t.Errorf("installed %q, journal kept %t: a recovery ran in an untrusted directory", got, exists(env.binPath+".pending"))
		}
	})
}

// TestCheckRootOnlyWalksAncestors pins that the check covers every directory
// above the bin directory, after resolving links, not only the directory.
func TestCheckRootOnlyWalksAncestors(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	bin := filepath.Join(base, "opt", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	// Everything is root's except the ancestor opt, owned by a user.
	opt := filepath.Join(base, "opt")
	optFI, err := os.Lstat(opt)
	if err != nil {
		t.Fatal(err)
	}
	owner := func(fi os.FileInfo) (uint32, uint32, bool) {
		if os.SameFile(fi, optFI) {
			return 1000, 1000, true
		}
		return 0, 0, true
	}
	for _, dir := range []string{bin, link} {
		if err := checkRootOnly(dir, owner); err == nil || !strings.Contains(err.Error(), "opt is owned by uid 1000") {
			t.Errorf("checkRootOnly(%s): got %v, want the user-owned ancestor named", dir, err)
		}
	}
	if err := checkRootOnly(bin, func(os.FileInfo) (uint32, uint32, bool) { return 0, 0, true }); err != nil {
		t.Errorf("all root-owned: %v", err)
	}
}
