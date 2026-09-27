//go:build linux

package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// unitRoot prefixes every path the unit readers open. It is "/" in
// production; tests point it at a temporary tree so they never read the
// host's real units.
var unitRoot = "/"

// hostPath maps an absolute host path into unitRoot.
func hostPath(p string) string { return filepath.Join(unitRoot, p) }

// unitSearchDirs is systemd's system unit search path, highest priority
// first, as `systemd-analyze unit-paths` prints it on systemd 257 (Debian
// 13). systemd read a probe unit's drop-ins from each directory of it.
var unitSearchDirs = []string{
	"/etc/systemd/system.control",
	"/run/systemd/system.control",
	"/run/systemd/transient",
	"/run/systemd/generator.early",
	unitDir,
	"/etc/systemd/system.attached",
	"/run/systemd/system",
	"/run/systemd/system.attached",
	"/run/systemd/generator",
	"/usr/local/lib/systemd/system",
	"/usr/lib/systemd/system",
	"/run/systemd/generator.late",
}

// unitFile is one file of a unit: its bytes and its logical lines.
type unitFile struct {
	path  string
	raw   []byte
	lines []string
	// specific is true for the unit itself and its name-specific drop-ins,
	// false for prefix (remote-.service.d) and type-level (service.d) ones.
	specific bool
}

// isPlain reports whether s is non-empty and made only of the characters the
// unit readers accept without quoting: ASCII letters and digits and
// ._/+,:@=~-. systemd reads anything else (quotes, backslashes, %, $,
// whitespace, non-ASCII) with rules the readers do not model.
func isPlain(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', strings.IndexByte("._/+,:@=~-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// blanks are the characters systemd splits unit values on and trims.
const blanks = " \t"

// readLines reads the file at path and its logical lines as systemd sees
// them, trimmed of blanks, comments dropped. As measured on systemd 257: a
// comment line (# or ; first) never continues and is skipped inside a
// continuation; a line ending in an odd number of backslashes continues on
// the next, the backslash becoming a space; a continuation pending at the end
// of the file still counts. A file holding a byte order mark, a NUL, a CR
// not followed by LF, or a backslash followed by trailing blanks is an
// error, since systemd's reading of those is not modelled.
func readLines(path string) (raw []byte, lines []string, err error) {
	raw, err = os.ReadFile(hostPath(path))
	if err != nil {
		return nil, nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.ContainsAny(text, "\x00\r\uFEFF") {
		return nil, nil, fmt.Errorf("%s: contains a byte order mark, a NUL or a bare carriage return", path)
	}
	var (
		cont strings.Builder
		open bool
	)
	for phys := range strings.Lines(text) {
		phys = strings.TrimSuffix(phys, "\n")
		line := strings.Trim(phys, blanks)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if trailing := len(line) - len(strings.TrimRight(line, `\`)); trailing%2 == 1 {
			if strings.TrimRight(phys, blanks) != phys {
				return nil, nil, fmt.Errorf("%s: a continuation backslash followed by blanks", path)
			}
			cont.WriteString(phys[:len(phys)-1])
			cont.WriteByte(' ')
			open = true
			continue
		}
		if open {
			cont.WriteString(phys)
			line = strings.Trim(cont.String(), blanks)
			cont.Reset()
			open = false
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if open {
		if line := strings.Trim(cont.String(), blanks); line != "" {
			lines = append(lines, line)
		}
	}
	return raw, lines, nil
}

// entry splits a logical line into its key and value, trimmed of blanks. ok
// is false for a section header or a line with no "=" (which systemd
// ignores).
func entry(line string) (key, val string, ok bool) {
	if strings.HasPrefix(line, "[") {
		return "", "", false
	}
	key, val, ok = strings.Cut(line, "=")
	key = strings.Trim(key, blanks)
	return key, strings.Trim(val, blanks), ok && key != ""
}

// loadUnit reads the unit named name (such as "remote-mic.service") and the
// drop-ins systemd applies to it, in application order. It returns no files
// and no error when no search directory has the unit. It fails when the unit
// cannot be read as installed: a copy of that name in a search directory
// that outranks /etc/systemd/system, only a copy below it, a link to another
// name, an empty file or a link to /dev/null (masked), an alias link to it,
// or an unreadable directory or file.
func loadUnit(name string) ([]unitFile, error) {
	unit, err := findUnit(name)
	if err != nil || unit == "" {
		return nil, err
	}
	if err := checkAliases(name); err != nil {
		return nil, err
	}
	raw, lines, err := readLines(unit)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s is masked (empty)", name)
	}
	files := []unitFile{{path: unit, raw: raw, lines: lines, specific: true}}
	dropIns, err := dropInPaths(name)
	if err != nil {
		return nil, err
	}
	for _, d := range dropIns {
		raw, lines, err := readLines(d.path)
		if err != nil {
			return nil, err
		}
		files = append(files, unitFile{path: d.path, raw: raw, lines: lines, specific: d.specific})
	}
	return files, nil
}

// findUnit returns the path to read for the unit named name, "" when no
// search directory has it. See loadUnit for what it refuses.
func findUnit(name string) (string, error) {
	for _, dir := range unitSearchDirs {
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(hostPath(p))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if dir != unitDir {
			return "", fmt.Errorf("%s is defined in %s, not where the installer writes it", name, dir)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		target, err := os.Readlink(hostPath(p))
		if err != nil {
			return "", err
		}
		if target == os.DevNull {
			return "", fmt.Errorf("%s is masked", name)
		}
		if filepath.Base(target) != name {
			return "", fmt.Errorf("%s is a link to %s", p, target)
		}
		// Read the target through unitRoot, not the link: an absolute
		// target would otherwise be resolved outside it.
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		return target, nil
	}
	return "", nil
}

// checkAliases fails when a search directory holds a link of another name to
// the unit: systemd then also applies that name's drop-ins (measured on
// systemd 257), which the reader does not follow.
func checkAliases(name string) error {
	for _, dir := range unitSearchDirs {
		entries, err := os.ReadDir(hostPath(dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() == name || e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			target, err := os.Readlink(hostPath(filepath.Join(dir, e.Name())))
			if err != nil {
				return err
			}
			if filepath.Base(target) == name {
				return fmt.Errorf("%s is an alias of %s", filepath.Join(dir, e.Name()), name)
			}
		}
	}
	return nil
}

// dropIn is one drop-in file systemd applies to a unit.
type dropIn struct {
	path     string
	specific bool // from a name-specific directory
}

// dropInPaths lists the drop-ins systemd applies to the unit named name, in
// application order. As measured on systemd 257 with a probe unit: for one
// file name, the name-specific (remote-mic.service.d) and prefix
// (remote-.service.d) directories are searched in search-path priority
// order, the more specific name first within one directory, and the first
// match wins; a type-level service.d file applies only when no name or prefix
// directory has that file name. Hidden files are skipped, an empty file or a
// link to /dev/null masks its name, and the files apply in file name order.
// A drop-in directory that is itself a link is an error.
func dropInPaths(name string) ([]dropIn, error) {
	stem, suffix, ok := strings.Cut(name, ".")
	if !ok {
		return nil, fmt.Errorf("unit name %q has no type suffix", name)
	}
	// Most specific first: remote-mic.service.d, then remote-.service.d.
	names := []string{name + ".d"}
	for i := len(stem) - 1; i > 0; i-- {
		if stem[i-1] == '-' {
			names = append(names, stem[:i]+"."+suffix+".d")
		}
	}
	chosen := make(map[string]dropIn)
	var order []string
	collect := func(dir string, specific bool) error {
		fi, err := os.Lstat(hostPath(dir))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		// systemd resolves a linked drop-in directory's target from its
		// own working directory, not the link's, and skips it when that
		// is no directory (checked with systemd-analyze on 257); the
		// reader does not model that.
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a link, which the reader does not follow", dir)
		}
		entries, err := os.ReadDir(hostPath(dir))
		if err != nil {
			return err
		}
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".conf") {
				continue
			}
			if _, seen := chosen[n]; !seen {
				chosen[n] = dropIn{path: filepath.Join(dir, n), specific: specific}
				order = append(order, n)
			}
		}
		return nil
	}
	for _, dir := range unitSearchDirs {
		for i, n := range names {
			if err := collect(filepath.Join(dir, n), i == 0); err != nil {
				return nil, err
			}
		}
	}
	for _, dir := range unitSearchDirs {
		if err := collect(filepath.Join(dir, suffix+".d"), false); err != nil {
			return nil, err
		}
	}
	slices.Sort(order)
	out := make([]dropIn, len(order))
	for i, n := range order {
		out[i] = chosen[n]
	}
	return out, nil
}
