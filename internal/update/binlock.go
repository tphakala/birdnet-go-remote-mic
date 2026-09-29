package update

import (
	"errors"
	"time"
)

// BinLockSuffix is appended to the installed binary's path to name the lock
// that serializes what replaces it (see LockBin). It is a contract between
// versions like the journal's name: every release that takes the lock must
// use the same file.
const BinLockSuffix = ".lock"

// DefaultBinLockWait is how long the updater waits for a `service install`
// that holds the lock, which is a matter of seconds. It ends well before the
// appliance withdraws a request the updater has not claimed, which the
// updater does only after taking the lock, so a busy lock fails the request
// with its own reason instead of a false "did not start".
const DefaultBinLockWait = updaterStartTimeout / 2

// InstallBinLockWait is how long `service install` waits for an update in
// progress: the updater holds the lock until the new version has reported
// healthy and settled or been rolled back, longer than its own health wait.
const InstallBinLockWait = 5 * time.Minute

// ErrBinBusy is returned by LockBin when another process still held the lock
// after the wait.
var ErrBinBusy = errors.New("another install or update of the binary is in progress")
