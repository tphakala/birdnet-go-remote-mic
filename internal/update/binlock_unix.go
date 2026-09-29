// syscall.Flock and its constants are absent on aix and solaris (see
// internal/runlock); the appliance builds only for linux.
//go:build unix && !aix && !solaris

package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// binLockPoll is how often a waiting LockBin retries.
const binLockPoll = 100 * time.Millisecond

// LockBin takes the exclusive lock that serializes whatever replaces the
// binary at binPath: the root updater (Applier.Apply) and `service install`.
// The lock file, binPath+BinLockSuffix, sits beside the binary and the
// journal, in a directory only root can write; the state directory is the
// service user's, so it must not hold a lock root waits on. The caller has
// made sure that directory exists and passes CheckRootOnly, and the file is
// opened without following a link.
//
// It retries for up to wait while another process holds the lock, calling
// waiting once the first time it finds it held (nil is fine), and returns
// ErrBinBusy after that or ctx's error when ctx ends first. LockBin never
// removes the file: unlinking a locked path lets a late opener lock the
// orphaned inode while a new file is locked under the same name.
//
// A release without the lock does not take it, so it cannot be held against
// one; the two stay unserialized until the installed binary has it.
func LockBin(ctx context.Context, binPath string, wait time.Duration, waiting func()) (release func(), err error) {
	path := binPath + BinLockSuffix
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600) //nolint:gosec // beside the validated bin path, in a root-only directory
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	notified := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // an fd fits in int
		if err == nil {
			// Closing the descriptor drops the flock.
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !notified && waiting != nil {
			notified = true
			waiting()
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrBinBusy
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(binLockPoll):
		}
	}
}
