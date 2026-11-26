//go:build !linux

package filesource

import "os"

// openFile opens path for reading. Only Linux has the non-blocking open the
// appliance relies on, so elsewhere a FIFO swapped in for a file can still
// block the open.
func openFile(path string) (*os.File, error) { return os.Open(path) }
