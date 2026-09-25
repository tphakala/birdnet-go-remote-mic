// syscall.Flock and its LOCK_EX/LOCK_NB/EWOULDBLOCK constants are absent on aix
// and solaris, so carve them out of the unix set. The appliance builds only for
// linux; this keeps runlock buildable and vettable on the other unix platforms.
//go:build unix && !aix && !solaris

// Package runlock marks a running appliance with an advisory lock on a file
// beside its config, and publishes where that appliance's management API
// listens. The token commands use it to tell whether an appliance is serving a
// given config: a running appliance keeps the config in memory and rewrites the
// whole file on every web UI save, so a token change must go through its API
// rather than into the file, where it would be ignored and later overwritten.
//
// The lock file is never removed, only emptied: on acquire, to drop a crashed
// holder's leftover state, and on release. Unlinking a flock'd path lets a late
// opener lock the orphaned inode while a new file is locked under the same name,
// so two holders could both believe they are alone.
//
// A second lock, the edit lock (EditPathFor, taken with LockEdits), serializes
// direct edits of the config file between token commands. The run lock cannot
// do that while an appliance runs without its management API: the appliance
// holds the run lock for its whole life, so two token commands that both fall
// back to editing the file would race their load, check, and save, and the last
// writer would silently win. The edit lock lives in its own file, so taking it
// never contends with the appliance's run lock, and the appliance never takes
// it. It carries no content and, for the same reason as the run lock, is never
// unlinked.
package runlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrHeld reports that another process holds the lock.
var ErrHeld = errors.New("runlock: held by another process")

// retryInterval is how often Acquire (and so LockEdits) retries a held lock
// while it waits.
const retryInterval = 50 * time.Millisecond

// State is what a running appliance publishes in its lock file. MgmtAddr and
// CertPath are empty when the management API is not serving (disabled, it
// failed to start, or it stopped and is being retried), in which case the
// appliance holds no writer for the config file.
type State struct {
	PID      int    `json:"pid"`
	MgmtAddr string `json:"mgmtAddr,omitempty"` // bound listener address, host:port
	CertPath string `json:"certPath,omitempty"` // PEM certificate the listener serves
}

// Lock is a held lock. The zero of *Lock (nil) is valid for Publish and
// Release, which then do nothing, so a caller that runs without a lock need not
// branch.
type Lock struct {
	f *os.File
}

// PathFor returns the lock file path for the config file at cfgPath. It resolves
// symlinks so a config reached through a symlink and through its target map to
// one lock file, rather than two locks that would each report no appliance
// running and let a token command edit the file under a live appliance.
//
// When the config file does not exist yet (a first-run token command that will
// create it), its own symlinks cannot be resolved, so the directory is resolved
// instead and the base name appended: two spellings of a not-yet-created config
// that share a resolved directory still take one lock. If the base is itself a
// dangling symlink (its target not created yet), the link is followed one hop so
// the link and its future target still converge on one lock. Only when the
// directory cannot be resolved is the unresolved path used (made absolute when
// the working directory is known, as every result is).
func PathFor(cfgPath string) string {
	// Anchor a relative spelling first, so it yields the same lock path string as
	// the absolute one (EvalSymlinks keeps a relative input relative). Join by
	// hand rather than with filepath.Abs, which cleans ".." as text: after a
	// symlink, the kernel and EvalSymlinks resolve ".." against the link's
	// target, so cleaning first could name a different file than the one opened.
	if !filepath.IsAbs(cfgPath) {
		if wd, err := os.Getwd(); err == nil {
			cfgPath = wd + string(filepath.Separator) + cfgPath
		}
	}
	if resolved, err := filepath.EvalSymlinks(cfgPath); err == nil {
		return resolved + ".lock"
	}
	dir, base := filepath.Split(cfgPath)
	if base == "" {
		return cfgPath + ".lock"
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return cfgPath + ".lock"
	}
	full := filepath.Join(resolvedDir, base)
	// A dangling final symlink (its target not created yet) fails the
	// EvalSymlinks above; follow it one hop and derive the lock beside the
	// target's own resolved directory, so the link and its future target converge
	// on one lock exactly as PathFor(target) would, even when the target is
	// absolute or its directory is itself a symlink.
	if target, err := os.Readlink(full); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(resolvedDir, target)
		}
		if tDir, tBase := filepath.Split(target); tBase != "" {
			if resolvedTDir, err := filepath.EvalSymlinks(tDir); err == nil {
				return filepath.Join(resolvedTDir, tBase) + ".lock"
			}
		}
		return filepath.Clean(target) + ".lock"
	}
	return full + ".lock"
}

// editSuffix names the edit lock file beside the run lock file.
const editSuffix = ".edit"

// EditPathFor returns the edit lock file path for the config file at cfgPath:
// the run lock path from PathFor plus ".edit", so every spelling of one config
// converges on one edit lock exactly as it does on one run lock.
func EditPathFor(cfgPath string) string {
	return PathFor(cfgPath) + editSuffix
}

// LockEdits takes the exclusive edit lock for the config file at cfgPath,
// creating the lock file (0600) if needed. It retries for up to wait while
// another process holds the lock, so a concurrent token command waits its turn
// for the few milliseconds of an edit rather than failing; a lock still held
// after wait returns ErrHeld. The caller holds it across the whole load, check,
// and save of the config, then calls release, which empties the (contentless)
// file and closes the descriptor, dropping the flock.
func LockEdits(cfgPath string, wait time.Duration) (release func() error, err error) {
	l, err := Acquire(EditPathFor(cfgPath), wait)
	if err != nil {
		return nil, err
	}
	return l.Release, nil
}

// TryAcquire takes the lock at path without waiting, creating the file (0600)
// if needed. It returns ErrHeld when another process holds it.
func TryAcquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path derives from the operator's --config
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // an fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("runlock: lock %s: %w", path, err)
	}
	// Clear whatever a previous holder left: a crash skips Release, so its stale
	// published state would otherwise be read as a live appliance until this
	// holder's own Publish. A held lock returns ErrHeld above, so this only ever
	// empties an orphaned file, never the current holder's state.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("runlock: truncate %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Acquire takes the lock at path, retrying for up to wait while another process
// holds it. The wait covers a token command that holds the lock for the few
// milliseconds of a file edit; a lock still held after wait returns ErrHeld.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := TryAcquire(path)
		if !errors.Is(err, ErrHeld) || !time.Now().Before(deadline) {
			return l, err
		}
		time.Sleep(retryInterval)
	}
}

// Publish replaces the lock file's content with s.
func (l *Lock) Publish(s State) error {
	if l == nil {
		return nil
	}
	// Marshal cannot fail: State holds only an int and strings.
	b, _ := json.Marshal(s)
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt(b, 0)
	if err == nil {
		err = l.f.Sync()
	}
	return err
}

// Release empties the lock file and releases the lock. Closing the descriptor
// drops the flock.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	terr := l.f.Truncate(0)
	cerr := l.f.Close()
	return errors.Join(terr, cerr)
}

// ReadState returns the state published at path. ok is false when nothing has
// been published: the holder is still starting, or the content is a partial
// write caught mid-Publish. Callers use it only after TryAcquire returned
// ErrHeld, so a held lock with no state means "starting", not "stopped".
func ReadState(path string) (s State, ok bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the operator's --config
	if err != nil {
		return State{}, false, err
	}
	s, ok = parseState(b)
	return s, ok, nil
}

// parseState decodes published lock content. Empty or undecodable content is
// "nothing published yet", not an error: a reader can catch a Publish between
// its truncate and its write.
func parseState(b []byte) (State, bool) {
	var s State
	if len(b) == 0 || json.Unmarshal(b, &s) != nil || s.PID == 0 {
		return State{}, false
	}
	return s, true
}
