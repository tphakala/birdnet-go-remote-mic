//go:build !unix

package filesource

import "os"

// openFile opens path for reading. Without a Unix non-blocking open, a FIFO
// swapped in for a file can still block the open; the appliance runs on Linux.
func openFile(path string) (*os.File, error) { return os.Open(path) }
