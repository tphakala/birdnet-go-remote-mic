//go:build !unix

package update

import "os"

// fileOwner cannot tell a file's owner here, so the updater refuses to act.
func fileOwner(os.FileInfo) (uid, gid uint32, ok bool) { return 0, 0, false }

// openNonblock opens without blocking on a FIFO.
const openNonblock = 0
