package update

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// maxBinarySize caps the staged binary the updater reads into memory; release
// binaries are a few tens of MB.
const maxBinarySize = 256 << 20

// journal is the content of the install journal, BinPath+".pending". It is
// written, root-owned beside the binary, after the previous binary is kept
// and before the new one is swapped in, and removed once the outcome is final.
// Finding it when the updater starts means a run was cut off mid-install (a
// power loss or a kill): the installed binary was never confirmed healthy,
// so it is rolled back. It never lives in the state directory, where the
// service user could forge one to force a downgrade.
//
// Recovery runs in whatever binary is installed, which after a cut is the
// new one, so the file name and these fields are a contract between
// versions: a release must read the journal an older one wrote. Add fields,
// never rename or repurpose one.
type journal struct {
	From       string `json:"from"`
	To         string `json:"to"`
	PrevSHA256 string `json:"prevSha256"`
	NewSHA256  string `json:"newSha256"`
}

// Default timings for Applier.
const (
	DefaultHealthTimeout = 2 * time.Minute
	defaultPoll          = time.Second
	versionTimeout       = 15 * time.Second
)

// Applier installs a staged release as root. It is the body of
// `remote-mic service apply-update`, started by the systemd path unit when
// the appliance writes the request file.
//
// Nothing in the staging directory is trusted: the manifest is verified
// against the keys compiled into this (the installed, root-owned) binary, the
// version must be strictly newer than this binary's own, and the staged
// binary is read into memory once and checked against the manifest there, so
// what is installed is exactly what was checked. Every staging-directory
// access goes through an os.Root on the state directory, so a symlink planted
// there cannot redirect a read or the status write outside it.
type Applier struct {
	// StateDir is the service state directory holding DirName.
	StateDir string
	// BinPath is the installed binary the service runs.
	BinPath string
	// Unit is the appliance's systemd unit, restarted after the swap.
	Unit string
	// Running is this binary's version, the version being replaced.
	Running string
	// Target is this binary's release target key.
	Target string
	// Trusted are the release signing keys.
	Trusted map[string]ed25519.PublicKey

	// Restart restarts a systemd unit; Active reports whether it is active.
	Restart func(unit string) error
	Active  func(unit string) (bool, error)
	// Version runs a binary's version command and returns its output.
	Version func(ctx context.Context, bin string) (string, error)

	// HealthTimeout bounds the wait for the new version to report healthy;
	// Poll is the interval between checks.
	HealthTimeout time.Duration
	Poll          time.Duration
	// Now stamps the result; time.Now when nil.
	Now func() time.Time
	// Logf logs progress; log.Printf when nil.
	Logf func(format string, args ...any)
}

// Apply claims the pending request and installs the staged release, or does
// nothing when no request is pending. It claims the request by renaming it to
// TakenFile, so the path unit does not start it again, and removes the claim
// at exit; it writes the outcome to the status file for the appliance to
// report. When an install journal is found, a previous run was cut off
// mid-install, and Apply rolls that back (recoverInterrupted) instead of
// installing anything. The returned error is the same outcome, for the unit's
// log.
func (a *Applier) Apply(ctx context.Context) error {
	root, err := os.OpenRoot(a.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	reqPath := path.Join(DirName, RequestFile)
	takenPath := path.Join(DirName, TakenFile)
	defer func() { _ = root.Remove(takenPath) }()
	if _, err := os.Lstat(a.journalPath()); err == nil {
		defer func() { _ = root.Remove(reqPath) }()
		return a.finish(root, a.recoverInterrupted(root))
	}
	// Claim the request by renaming it: the appliance withdraws one nobody
	// took by removing it, and exactly one of the two wins.
	if err := root.Rename(reqPath, takenPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			a.logf("apply-update: no update requested")
			return nil
		}
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, Installed: a.Running, Reason: err.Error()})
	}
	reqBytes, err := readFileIn(root, takenPath, maxSmallFile)
	if err != nil {
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, Installed: a.Running, Reason: err.Error()})
	}
	var req Request
	if err := decodeSmall(RequestFile, reqBytes, &req); err != nil {
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, Installed: a.Running, Reason: err.Error()})
	}
	a.logf("apply-update: update to %s requested", req.Version)

	m, bin, err := a.verifyStaged(root)
	if err != nil {
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, To: req.Version, Installed: a.Running, Reason: err.Error()})
	}
	res := a.install(ctx, root, m, bin)
	if res.Outcome == OutcomeUpdated {
		for _, name := range []string{BinaryFile, ManifestFile, SignatureFile} {
			_ = root.Remove(path.Join(DirName, name))
		}
	}
	return a.finish(root, res)
}

// verifyStaged checks the staged manifest pair with this binary's keys, that
// it names a newer version with a build for this platform, and that the
// staged binary matches it; it returns the manifest and the binary's bytes.
func (a *Applier) verifyStaged(root *os.Root) (*releasemanifest.Manifest, []byte, error) {
	raw, err := readFileIn(root, path.Join(DirName, ManifestFile), releasemanifest.MaxManifestSize)
	if err != nil {
		return nil, nil, err
	}
	sig, err := readFileIn(root, path.Join(DirName, SignatureFile), releasemanifest.MaxSignatureSize)
	if err != nil {
		return nil, nil, err
	}
	m, err := releasemanifest.Verify(raw, sig, a.Trusted)
	if err != nil {
		return nil, nil, err
	}
	newer, err := Newer(m.Version, a.Running)
	if err != nil {
		return nil, nil, err
	}
	if !newer {
		return nil, nil, fmt.Errorf("%s is not newer than the installed %s", m.Version, a.Running)
	}
	t, ok := m.Targets[a.Target]
	if !ok {
		return nil, nil, fmt.Errorf("%w (%s)", ErrNoTarget, a.Target)
	}
	if t.Binary.Size > maxBinarySize {
		return nil, nil, fmt.Errorf("binary of %d bytes is over the %d byte limit", t.Binary.Size, maxBinarySize)
	}
	bin, err := readFileIn(root, path.Join(DirName, BinaryFile), t.Binary.Size)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(bin)) != t.Binary.Size {
		return nil, nil, fmt.Errorf("staged binary is %d bytes, want %d", len(bin), t.Binary.Size)
	}
	if got := sha256Hex(bin); got != t.Binary.SHA256 {
		return nil, nil, fmt.Errorf("staged binary sha256 %s, want %s", got, t.Binary.SHA256)
	}
	return m, bin, nil
}

// install swaps in bin, restarts the unit and waits for the new version to
// report healthy, restoring the previous binary when it does not, including
// when ctx is cancelled (the updater being stopped) during the wait. The
// install journal covers the span from the swap to the final outcome.
func (a *Applier) install(ctx context.Context, root *os.Root, m *releasemanifest.Manifest, bin []byte) *Result {
	res := &Result{From: a.Running, To: m.Version, Installed: a.Running}
	fail := func(err error) *Result {
		res.Outcome, res.Reason = OutcomeFailed, err.Error()
		return res
	}
	newPath := a.BinPath + ".new"
	if err := atomicfile.Write(newPath, bin, 0o755); err != nil {
		return fail(err)
	}
	defer func() { _ = os.Remove(newPath) }()
	vctx, cancel := context.WithTimeout(ctx, versionTimeout)
	out, err := a.Version(vctx, newPath)
	cancel()
	if cerr := ctx.Err(); cerr != nil {
		return fail(fmt.Errorf("the updater was stopped: %w", cerr))
	}
	if err != nil {
		return fail(fmt.Errorf("the new binary does not run: %w", err))
	}
	if want := "remote-mic " + m.Version; strings.TrimSpace(out) != want {
		return fail(fmt.Errorf("the new binary reports %q, want %q", strings.TrimSpace(out), want))
	}
	prev, err := os.ReadFile(a.BinPath)
	if err != nil {
		return fail(fmt.Errorf("read the installed binary: %w", err))
	}
	if err := atomicfile.Write(a.BinPath+".prev", prev, 0o755); err != nil {
		return fail(fmt.Errorf("keep the installed binary: %w", err))
	}
	j, err := json.Marshal(journal{From: a.Running, To: m.Version, PrevSHA256: sha256Hex(prev), NewSHA256: sha256Hex(bin)})
	if err == nil {
		err = atomicfile.Write(a.journalPath(), j, 0o600)
	}
	if err != nil {
		return fail(fmt.Errorf("write the install journal: %w", err))
	}
	// A health file from before the restart must not pass for the new one.
	_ = root.Remove(path.Join(DirName, HealthFile))
	if err := os.Rename(newPath, a.BinPath); err != nil {
		a.removeJournal()
		return fail(fmt.Errorf("install the new binary: %w", err))
	}
	atomicfile.SyncDir(filepath.Dir(a.BinPath))
	res.Installed = m.Version
	a.logf("apply-update: installed %s, restarting %s", m.Version, a.Unit)

	if err := a.Restart(a.Unit); err != nil {
		return a.rollback(root, res, fmt.Errorf("restart %s: %w", a.Unit, err))
	}
	if err := a.awaitHealthy(ctx, root, m.Version); err != nil {
		return a.rollback(root, res, err)
	}
	// Removing the journal is the commit point: from here a crash keeps the
	// new version, which has proven healthy.
	a.removeJournal()
	res.Outcome = OutcomeUpdated
	a.logf("apply-update: %s is up", m.Version)
	return res
}

// rollback puts the previous binary back by renaming BinPath.prev over it,
// which needs no free space, then removes the install journal, writes the
// result and restarts the unit on the restored binary. The result is written
// before the restart so the restored appliance finds it when it boots. When the rename fails the new
// binary stays installed: the result says so, and the unit is restarted
// anyway, since leaving it stopped helps nobody.
func (a *Applier) rollback(root *os.Root, res *Result, cause error) *Result {
	res.Outcome, res.Reason = OutcomeRolledBack, cause.Error()
	a.logf("apply-update: %s did not come up (%v); restoring %s", res.To, cause, res.From)
	if err := os.Rename(a.BinPath+".prev", a.BinPath); err != nil {
		res.Outcome = OutcomeFailed
		res.Reason += fmt.Sprintf("; restoring %s failed, so %s stays installed: %v", res.From, res.To, err)
	} else {
		atomicfile.SyncDir(filepath.Dir(a.BinPath))
		res.Installed = res.From
	}
	// The outcome is final either way; a journal left behind would make the
	// next start roll back again.
	a.removeJournal()
	if err := a.writeResult(root, res); err != nil {
		a.logf("apply-update: write status: %v", err)
	}
	res.written = true
	if err := a.Restart(a.Unit); err != nil {
		a.logf("apply-update: restart %s on the restored binary: %v", a.Unit, err)
	}
	return res
}

// recoverInterrupted finishes a run that was cut off after the swap, from
// the journal: the installed binary was never confirmed healthy, so the
// previous one goes back. The installed binary's hash says where the cut
// fell: still the new binary (rename the kept copy back), already the old
// one (a rollback that finished its rename), or neither (nothing safe to do
// but report it, addressed to this updater's own version, which is what the
// restarted appliance runs). The journal and any stale .new copy are removed
// and the unit restarted whatever the outcome, so a failure here cannot
// repeat on every start.
func (a *Applier) recoverInterrupted(root *os.Root) *Result {
	res := &Result{Outcome: OutcomeFailed, From: a.Running, Installed: a.Running}
	defer func() {
		_ = os.Remove(a.BinPath + ".new") // a cut before the swap leaves it
		a.removeJournal()
		if err := a.writeResult(root, res); err != nil {
			a.logf("apply-update: write status: %v", err)
		}
		res.written = true
		if err := a.Restart(a.Unit); err != nil {
			a.logf("apply-update: restart %s: %v", a.Unit, err)
		}
	}()
	var j journal
	b, err := os.ReadFile(a.journalPath())
	if err == nil {
		err = decodeSmall(filepath.Base(a.journalPath()), b, &j)
	}
	if err == nil && (j.PrevSHA256 == "" || j.NewSHA256 == "") {
		err = errors.New("it names no binary hashes")
	}
	if err != nil {
		res.Reason = fmt.Sprintf("an interrupted update left an unreadable journal: %v", err)
		return res
	}
	res.From, res.To = j.From, j.To
	a.logf("apply-update: the update from %s to %s was interrupted before it was confirmed healthy", j.From, j.To)
	res.Reason = fmt.Sprintf("the updater was interrupted before %s was confirmed healthy", j.To)
	installed := fileSHA256(a.BinPath)
	switch {
	case installed == j.PrevSHA256:
		res.Outcome, res.Installed = OutcomeRolledBack, j.From
	case installed == j.NewSHA256 && fileSHA256(a.BinPath+".prev") == j.PrevSHA256:
		if err := os.Rename(a.BinPath+".prev", a.BinPath); err != nil {
			res.Installed = j.To
			res.Reason += fmt.Sprintf("; restoring %s failed, so %s stays installed: %v", j.From, j.To, err)
			return res
		}
		atomicfile.SyncDir(filepath.Dir(a.BinPath))
		a.logf("apply-update: restored %s", j.From)
		res.Outcome, res.Installed = OutcomeRolledBack, j.From
	default:
		// This updater runs as the binary at BinPath (ExecStart), so its own
		// version is what the restarted appliance runs: address it there.
		res.Installed = a.Running
		res.Reason += fmt.Sprintf("; %s matches neither version, so it was left as it is", a.BinPath)
	}
	return res
}

func (a *Applier) journalPath() string { return a.BinPath + ".pending" }

func (a *Applier) removeJournal() {
	if err := os.Remove(a.journalPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.logf("apply-update: remove the install journal: %v", err)
	}
	atomicfile.SyncDir(filepath.Dir(a.BinPath))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileSHA256 hashes a file, or returns "" when it cannot be read.
func fileSHA256(p string) string {
	b, err := os.ReadFile(p) //nolint:gosec // the binary path and its kept copy, root-owned
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

// awaitHealthy waits until the unit is active and the appliance has written a
// health file naming version, or the timeout passes, or ctx is cancelled (the
// updater being stopped); the timeout error says which of the two was missing
// last.
func (a *Applier) awaitHealthy(ctx context.Context, root *os.Root, version string) error {
	timeout := cmp.Or(a.HealthTimeout, DefaultHealthTimeout)
	poll := cmp.Or(a.Poll, defaultPoll)
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(poll)
	defer t.Stop()
	var last string
	for {
		if b, err := readFileIn(root, path.Join(DirName, HealthFile), maxSmallFile); err == nil {
			var h Health
			if json.Unmarshal(b, &h) == nil && h.Version == version {
				if active, err := a.Active(a.Unit); err == nil && active {
					return nil
				}
				last = a.Unit + " is not active"
			} else {
				last = "the health file names " + h.Version
			}
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return fmt.Errorf("the updater was stopped before %s was confirmed healthy", version)
			}
			if last == "" {
				last = "it never wrote its health file"
			}
			return fmt.Errorf("no healthy start within %s: %s", timeout, last)
		case <-t.C:
		}
	}
}

// finish writes res to the status file, unless a rollback already did, and
// returns it as an error for the unit's log (nil for a successful update).
func (a *Applier) finish(root *os.Root, res *Result) error {
	// A rollback writes its result before its restart.
	if !res.written {
		if err := a.writeResult(root, res); err != nil {
			a.logf("apply-update: write status: %v", err)
		}
	}
	if res.Outcome == OutcomeUpdated {
		return nil
	}
	return fmt.Errorf("update to %s %s: %s", res.To, res.Outcome, res.Reason)
}

// writeResult writes the status file through root: a fresh temporary file
// (O_EXCL, so a planted file or link is never written through) renamed over
// the status file, which replaces a planted link rather than following it.
func (a *Applier) writeResult(root *os.Root, res *Result) error {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	res.Time = now().UTC()
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	tmp := path.Join(DirName, StatusFile+".tmp")
	_ = root.Remove(tmp)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, path.Join(DirName, StatusFile))
}

func (a *Applier) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// readFileIn reads name inside root, refusing anything but a regular file and
// a file longer than limit.
func readFileIn(root *os.Root, name string, limit int64) ([]byte, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := regularFile(name, fi); err != nil {
		return nil, err
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// Check the opened file too: the entry could be swapped between the
	// Lstat and the Open.
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if err := regularFile(name, fi); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("%s is over %d bytes", name, limit)
	}
	return buf.Bytes(), nil
}
