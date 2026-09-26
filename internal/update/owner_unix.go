//go:build unix

package update

import (
	"os"
	"syscall"
)

// fileOwner returns the numeric owner of a file from its stat data.
func fileOwner(fi os.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

// openNonblock opens without blocking on a FIFO.
const openNonblock = syscall.O_NONBLOCK
