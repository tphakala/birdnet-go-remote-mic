// Package atomicfile writes a file atomically: a reader sees either the old
// contents or the new, never a partial write. It is shared by the config
// persister and the certificate writer, which need the same durable-replace
// semantics.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path via a temp file in the same directory followed by
// an atomic rename. The contents are fsynced before the rename and the
// directory after it, so the replacement survives a power cut: without the
// directory sync, ext4 can lose the rename itself and come back with the old
// file (or, depending on mount options, an empty one).
// If path is a symlink, its target is rewritten rather than replaced with a
// regular file, so an operator's symlinked config or certificate path survives.
func Write(path string, data []byte, perm os.FileMode) error {
	// Resolve a symlinked path so the rename updates the real file instead of
	// clobbering the link. A missing or plain path resolves to itself; on any
	// resolve error, fall back to the path as given.
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename has consumed tmp
	// Preserve the existing file's ownership so a privileged writer (an admin
	// running a CLI command under sudo) does not silently re-home a
	// service-user-owned config or certificate to root and lock the service out
	// of its own files. Best-effort: on a first write there is nothing to match,
	// and on a filesystem without ownership the chown is moot. This runs BEFORE
	// Chmod because a chown clears the setuid/setgid bits for a non-root caller,
	// which would silently strip a mode the caller requested via perm.
	preserveOwner(f, target)
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs dir so a rename inside it is durable. It is best effort and
// reports nothing: the new contents are already in place, and failing the write
// now would tell the caller its change did not happen when it did (a config
// PATCH would answer 500 over a file that already holds the new config). Some
// filesystems cannot sync a directory at all and return EINVAL.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
