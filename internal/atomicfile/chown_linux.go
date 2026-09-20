//go:build linux

package atomicfile

import (
	"os"
	"syscall"
)

// preserveOwner chowns the open temp file f to match target's current owner, so
// an atomic replace keeps the file's uid and gid instead of adopting the
// writer's. It is best-effort: a missing target (first write) or a stat/chown
// that cannot apply leaves f with the writer's ownership, which is the prior
// behavior. A same-owner chown (the common self-write) succeeds even unprivileged.
func preserveOwner(f *os.File, target string) {
	fi, err := os.Stat(target)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = f.Chown(int(st.Uid), int(st.Gid))
}
