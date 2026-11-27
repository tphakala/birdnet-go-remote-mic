//go:build linux

package service

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The names the directory checks call a service's directories in messages.
const (
	configDirLabel = "config directory"
	stateDirLabel  = "state directory"
)

// mountinfoPath lists the mounts of this process's namespace, one per line
// (proc(5)).
const mountinfoPath = "/proc/self/mountinfo"

// mountEntry is one line of a mountinfo listing (proc(5)): the mount's id,
// its parent's id, the root of the mount within its filesystem, and the
// mount point, both paths decoded.
type mountEntry struct{ id, parent, root, point string }

// parseMountinfo returns the entries of a mountinfo listing, whose first,
// second, fourth and fifth fields are the id, parent id, root and mount point.
// The kernel escapes a space, tab, newline or backslash in a path as a
// three-digit octal sequence (\040), which is decoded here.
func parseMountinfo(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			return nil, fmt.Errorf("malformed line %q", sc.Text())
		}
		out = append(out, mountEntry{id: f[0], parent: f[1], root: unescapeMountPath(f[3]), point: unescapeMountPath(f[4])})
	}
	return out, sc.Err()
}

func unescapeMountPath(p string) string {
	if !strings.Contains(p, `\`) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) {
			if n, err := strconv.ParseUint(p[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// readMountinfo reads this process's mount table.
func readMountinfo() ([]mountEntry, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return parseMountinfo(f)
}

// mountPoints lists this host's mount points.
func mountPoints() ([]string, error) {
	entries, err := readMountinfo()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.point)
	}
	return out, nil
}

// SandboxHiddenDir reports the directory HiddenDir names for path when this
// process's own mount table shows a systemd sandbox mount there on the
// path's way down (ProtectHome='s inaccessible directory, PrivateTmp='s
// private directory or its read-only empty fallback): the process sees none
// of the real content, so no open under it succeeds. Unlike HiddenDir it is
// not a prediction for the installed unit but a reading of this process. It
// reads /proc/self/mountinfo only when HiddenDir names a directory, touches
// nothing under path, and reports nothing when the table cannot be read or
// parsed.
func SandboxHiddenDir(path string) (dir string, ok bool) {
	return sandboxHiddenDir(path, readMountinfo)
}

func sandboxHiddenDir(path string, mounts func() ([]mountEntry, error)) (string, bool) {
	dir, ok := HiddenDir(path)
	if !ok {
		return "", false
	}
	entries, err := mounts()
	if err != nil {
		return "", false
	}
	if m := resolveMount(entries, path); m != nil && m.point == dir && sandboxRoot(m.root) {
		return dir, true
	}
	return "", false
}

// resolveMount returns the mount path lands on, walking the mount tree the
// way the kernel walks a path: from the namespace root (the / entry whose
// parent is itself or is not listed), into the child of the current mount that covers
// path at the shallowest mount point, until no child does. An overmount at
// the same point is a child of the mount it covers, so it is taken; a mount
// propagated beneath a covering one, or attached under a covered directory,
// is never reached, whatever order the table lists them in. It returns nil
// when no root is listed (a chroot onto a directory that is not a mount
// point) or the walk does not end within one step per entry, which only a
// malformed table with repeated ids can cause.
func resolveMount(entries []mountEntry, path string) *mountEntry {
	ids := make(map[string]bool, len(entries))
	for _, e := range entries {
		ids[e.id] = true
	}
	var cur *mountEntry
	for i := range entries {
		if e := entries[i]; e.point == "/" && (e.parent == e.id || !ids[e.parent]) {
			cur = &entries[i]
			break
		}
	}
	if cur == nil {
		return nil
	}
	for range entries {
		var next *mountEntry
		for i := range entries {
			e := &entries[i]
			if e != cur && e.parent == cur.id && within(path, e.point) && (next == nil || len(e.point) <= len(next.point)) {
				next = e
			}
		}
		if next == nil {
			return cur
		}
		cur = next
	}
	return nil
}

// sandboxRoot reports whether a mount's root is one systemd binds over a
// directory it hides: ProtectHome='s <runtime>/systemd/inaccessible/dir,
// PrivateTmp='s read-only fallback <runtime>/systemd/empty, or the tmp
// directory of a PrivateTmp= private directory (/tmp/systemd-private-.../tmp,
// /var/tmp/systemd-private-.../tmp, or /systemd-private-.../tmp when /tmp is
// its own filesystem). The root is relative to the filesystem holding it, so
// only its suffix or a distinctive part is matched; the filesystem type and
// options say nothing, as the inaccessible bind shows /run's tmpfs.
func sandboxRoot(root string) bool {
	return strings.HasSuffix(root, "/systemd/inaccessible/dir") ||
		strings.HasSuffix(root, "/systemd/empty") ||
		(strings.Contains(root, "/systemd-private-") && strings.HasSuffix(root, "/tmp"))
}

// checkPurgeDirs is the purge's directory check: checkNotOverBinDir plus
// purgeDirsOK against the host's mounts.
func checkPurgeDirs(s ServiceSpec) error {
	if err := checkNotOverBinDir(s); err != nil {
		return err
	}
	return purgeDirsOK(s, mountPoints)
}

// purgeDirsOK refuses a purge when something is mounted inside the config or
// state directory: os.RemoveAll descends into a mount before its rmdir fails
// with EBUSY, so the data on it (a bind mount of the bin directory, of an
// operator's recordings) would be deleted. A mount at the directory itself is
// allowed (a dedicated state partition), and a directory that is missing or a
// link is skipped, since removal does not descend through either. Failing to
// read the mount list refuses, because a guess could destroy data.
func purgeDirsOK(s ServiceSpec, mounts func() ([]string, error)) error {
	type dir struct{ label, path string }
	var todo []dir
	for _, d := range []dir{{configDirLabel, s.ConfigDir()}, {stateDirLabel, s.StateDir}} {
		fi, err := os.Lstat(d.path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Mode()&fs.ModeSymlink != 0) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check the %s %s: %w", d.label, d.path, err)
		}
		resolved, err := filepath.EvalSymlinks(d.path)
		if err != nil {
			return fmt.Errorf("check the %s %s: %w", d.label, d.path, err)
		}
		todo = append(todo, dir{d.label, resolved})
	}
	if len(todo) == 0 {
		return nil
	}
	list, err := mounts()
	if err != nil {
		return fmt.Errorf("read the mount list to check the directories a purge removes: %w", err)
	}
	for _, d := range todo {
		for _, m := range list {
			if strings.HasPrefix(m, d.path+string(filepath.Separator)) {
				return fmt.Errorf("the %s %s has %s mounted inside it; removing it would delete what is on that mount: unmount it first", d.label, d.path, m)
			}
		}
	}
	return nil
}
