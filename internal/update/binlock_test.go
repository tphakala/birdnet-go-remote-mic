package update

import "testing"

// TestBinLockWaitEndsBeforeTheRequestIsWithdrawn pins that the updater gives
// up on a busy lock before the appliance withdraws its still unclaimed
// request: the updater claims the request only after taking the lock, so a
// longer wait has the request withdrawn as "did not start" instead of failed
// with the lock's own reason.
func TestBinLockWaitEndsBeforeTheRequestIsWithdrawn(t *testing.T) {
	t.Parallel()
	if DefaultBinLockWait >= updaterStartTimeout {
		t.Errorf("DefaultBinLockWait = %v, want less than updaterStartTimeout = %v", DefaultBinLockWait, updaterStartTimeout)
	}
}
