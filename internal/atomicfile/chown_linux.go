//go:build linux

package atomicfile

import (
	"os"
	"syscall"
)

// chownFile is the chown seam preserveOwner applies. It is a package var so a
// test can observe that the ownership-preservation path stat'd the target and
// chowned the temp file to its uid/gid, which a same-uid rewrite cannot reveal
// through the resulting file alone.
var chownFile = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }

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
	_ = chownFile(f, int(st.Uid), int(st.Gid))
}
