//go:build linux

package filesource

import (
	"os"
	"syscall"
)

// openFile opens path for reading without blocking, so a FIFO swapped in for
// a file cannot hang the open waiting for a writer before the caller can
// install the descriptor and check it. O_NONBLOCK has no effect on reads of a
// regular file, so the decoders see the same blocking reads as with os.Open.
func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
