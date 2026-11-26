package filesource

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A plain open of a FIFO with no writer blocks until one shows up, so the
// check that rejects it has to run on a descriptor that opened at once.
func TestOpenFileDoesNotBlockOnFifo(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "pipe.flac")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openFile(fifo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().IsRegular() {
		t.Fatalf("mode = %v, want a FIFO", fi.Mode())
	}
}
