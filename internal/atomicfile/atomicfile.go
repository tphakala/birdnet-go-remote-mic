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
// an atomic rename, fsyncing the contents first so the replacement is durable.
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
	return os.Rename(tmp, target)
}
