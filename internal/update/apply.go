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

// Apply installs the staged release, or does nothing when no request is
// pending. It always removes the request, so the path unit does not start it
// again, and writes the outcome to the status file for the appliance to
// report. The returned error is the same outcome, for the unit's log.
func (a *Applier) Apply(ctx context.Context) error {
	root, err := os.OpenRoot(a.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	reqPath := path.Join(DirName, RequestFile)
	reqBytes, err := readFileIn(root, reqPath, maxSmallFile)
	if errors.Is(err, fs.ErrNotExist) {
		a.logf("apply-update: no update requested")
		return nil
	}
	defer func() { _ = root.Remove(reqPath) }()
	if err != nil {
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, Reason: err.Error()})
	}
	var req Request
	if err := decodeSmall(RequestFile, reqBytes, &req); err != nil {
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, Reason: err.Error()})
	}
	a.logf("apply-update: update to %s requested", req.Version)

	m, bin, err := a.verifyStaged(root)
	if err != nil {
		return a.finish(root, &Result{Outcome: OutcomeFailed, From: a.Running, To: req.Version, Reason: err.Error()})
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
	sum := sha256.Sum256(bin)
	if got := hex.EncodeToString(sum[:]); got != t.Binary.SHA256 {
		return nil, nil, fmt.Errorf("staged binary sha256 %s, want %s", got, t.Binary.SHA256)
	}
	return m, bin, nil
}

// install swaps in bin, restarts the unit and waits for the new version to
// report healthy, restoring the previous binary when it does not.
func (a *Applier) install(ctx context.Context, root *os.Root, m *releasemanifest.Manifest, bin []byte) *Result {
	res := &Result{From: a.Running, To: m.Version}
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
	// A health file from before the restart must not pass for the new one.
	_ = root.Remove(path.Join(DirName, HealthFile))
	if err := os.Rename(newPath, a.BinPath); err != nil {
		return fail(fmt.Errorf("install the new binary: %w", err))
	}
	atomicfile.SyncDir(filepath.Dir(a.BinPath))
	a.logf("apply-update: installed %s, restarting %s", m.Version, a.Unit)

	if err := a.Restart(a.Unit); err != nil {
		return a.rollback(root, res, prev, fmt.Errorf("restart %s: %w", a.Unit, err))
	}
	if err := a.awaitHealthy(ctx, root, m.Version); err != nil {
		return a.rollback(root, res, prev, err)
	}
	res.Outcome = OutcomeUpdated
	a.logf("apply-update: %s is up", m.Version)
	return res
}

// rollback restores the previous binary and restarts the unit on it. The
// result is written before the restart, so the restored appliance finds it
// when it boots.
func (a *Applier) rollback(root *os.Root, res *Result, prev []byte, cause error) *Result {
	res.Outcome, res.Reason = OutcomeRolledBack, cause.Error()
	a.logf("apply-update: %s did not come up (%v); restoring %s", res.To, cause, res.From)
	if err := atomicfile.Write(a.BinPath, prev, 0o755); err != nil {
		res.Reason += fmt.Sprintf("; restoring the previous binary failed: %v", err)
		return res
	}
	if err := a.writeResult(root, res); err != nil {
		a.logf("apply-update: write status: %v", err)
	}
	if err := a.Restart(a.Unit); err != nil {
		a.logf("apply-update: restart %s on the restored binary: %v", a.Unit, err)
	}
	return res
}

// awaitHealthy waits until the unit is active and the appliance has written a
// health file naming version, or the timeout passes.
func (a *Applier) awaitHealthy(ctx context.Context, root *os.Root, version string) error {
	timeout := cmp.Or(a.HealthTimeout, DefaultHealthTimeout)
	poll := cmp.Or(a.Poll, defaultPoll)
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
			if last == "" {
				last = "it never reported healthy"
			}
			return fmt.Errorf("%s did not come up within %s: %s", version, timeout, last)
		case <-t.C:
		}
	}
}

// finish writes res to the status file and returns it as an error for the
// unit's log (nil for a successful update).
func (a *Applier) finish(root *os.Root, res *Result) error {
	// A rolled-back result was written before its restart.
	if res.Outcome != OutcomeRolledBack {
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
