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

// mountinfoPath lists the mounts of this process's namespace, one per line;
// the fifth field is the mount point.
const mountinfoPath = "/proc/self/mountinfo"

// parseMountPoints returns the mount points in a mountinfo listing. The kernel
// escapes a space, tab, newline or backslash in a path as a three-digit octal
// sequence (\040), which is decoded here.
func parseMountPoints(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			return nil, fmt.Errorf("malformed line %q", sc.Text())
		}
		out = append(out, unescapeMountPath(f[4]))
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

// mountPoints lists this host's mount points.
func mountPoints() ([]string, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return parseMountPoints(f)
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
