//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// TestPrepareUpdatesWithdrawsBeforeTheManagerExists pins that run withdraws
// an unclaimed update request before the manager, and so the management API
// that serves it, can exist, and builds the manager only with the API on.
func TestPrepareUpdatesWithdrawsBeforeTheManagerExists(t *testing.T) {
	t.Parallel()
	for _, mgmtEnabled := range []bool{true, false} {
		dir := t.TempDir()
		req := filepath.Join(dir, update.RequestFile)
		if err := os.WriteFile(req, []byte(`{"version":"v0.0.1"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		center := notify.NewCenter()
		built, requestGoneWhenBuilt := false, false
		got := prepareUpdates(dir, mgmtEnabled, center, func() *update.Manager {
			built = true
			_, err := os.Lstat(req)
			requestGoneWhenBuilt = os.IsNotExist(err)
			return &update.Manager{}
		})
		if built != mgmtEnabled {
			t.Errorf("management %t: manager built %t", mgmtEnabled, built)
		}
		if (got != nil) != mgmtEnabled {
			t.Errorf("management %t: manager returned %t", mgmtEnabled, got != nil)
		}
		if built && !requestGoneWhenBuilt {
			t.Errorf("management %t: the request was still on disk when the manager was built", mgmtEnabled)
		}
		if _, err := os.Lstat(req); !os.IsNotExist(err) {
			t.Errorf("management %t: the request is still there", mgmtEnabled)
		}
		if n := len(center.Snapshot().Notifications); n != 1 {
			t.Errorf("management %t: %d notifications, want 1", mgmtEnabled, n)
		}
	}
}
