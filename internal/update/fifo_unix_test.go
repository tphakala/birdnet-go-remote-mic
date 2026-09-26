//go:build unix

package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReadFileInRefusesFIFO pins that a FIFO planted in the staging directory
// is refused at once instead of blocking the root updater's open.
func TestReadFileInRefusesFIFO(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "health.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	done := make(chan error, 1)
	// readFileIn's Lstat refuses the FIFO; exercise the open itself too,
	// as if the FIFO had been swapped in after the Lstat.
	go func() {
		f, err := openRegular(root, "health.json")
		if err == nil {
			_ = f.Close()
			err = errors.New("opened")
		}
		if _, rerr := readFileIn(root, "health.json", maxSmallFile); rerr == nil {
			err = errors.Join(err, errors.New("readFileIn read it"))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("opening a FIFO: got %v, want it refused as not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}
