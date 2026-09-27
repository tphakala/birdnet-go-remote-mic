//go:build linux

package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// applianceUnitPath is where the installer writes the appliance unit.
func applianceUnitPath() string { return filepath.Join(unitDir, DefaultUnitName) }

// Unit keys the readers act on.
const (
	keyEnvironment = "Environment"
	keyExecStart   = "ExecStart"
	keyUser        = "User"
)

// relevantKeys are the unit settings that can set or clear the appliance's
// environment, change its account or program, or remap a path outside
// privateDirs inside the service, per systemd.exec(5) and systemd.service(5)
// of systemd 257. A drop-in that sets none of them cannot change the config
// path or account, whatever else it holds. The list is systemd's, not ours,
// so it needs a re-check when a systemd release adds such a setting.
var relevantKeys = []string{
	keyEnvironment, "EnvironmentFile", "UnsetEnvironment", "PassEnvironment", "PAMName",
	keyExecStart, keyUser, "DynamicUser",
	"RootDirectory", "RootImage", "RootEphemeral", "BindPaths", "BindReadOnlyPaths",
	"TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories",
	"ProtectHome", "PrivateTmp", "PrivateUsers",
}

// privateDirs may look different to the service than to the CLI: the
// installer's PrivateTmp= gives the service its own /tmp and /var/tmp,
// ProtectHome= hides /home, /root and /run/user, and settings a drop-in may
// add without naming a path (LogNamespace=, PrivateDevices=, ProtectProc=,
// PrivateNetwork= and others) remount parts of /run, /dev, /proc and /sys.
// A config there is refused.
var privateDirs = []string{"/tmp", "/var/tmp", "/home", "/root", "/run", "/dev", "/proc", "/sys"}

// maxDropIn caps a drop-in the reader applies. systemd stops reading a file
// at a line longer than 1 MiB, so a larger file could end before an
// assignment the reader would apply; real drop-ins are a few lines.
const maxDropIn = 64 << 10

// InstalledBinPaths reports the binary the installed appliance unit runs and
// the binary the installed root updater unit installs to, each "" when its
// unit is absent, cannot be read as installed (see loadUnit), or a drop-in
// replaces its ExecStart=. The appliance compares them with its own path to
// decide whether it can update itself, so an unreadable unit reads as
// "cannot"; err says why a unit that exists gave "", for the log.
func InstalledBinPaths() (appliance, updater string, err error) {
	appliance, errA := unitBinPath(DefaultUnitName)
	updater, errU := unitBinPath(UpdateServiceUnit)
	return appliance, updater, errors.Join(errA, errU)
}

// unitBinPath returns the program of the unit named name's first ExecStart=
// line, without systemd's special-executable prefixes (-, @, :, +, !), or ""
// (see InstalledBinPaths).
func unitBinPath(name string) (string, error) {
	files, err := loadUnit(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if files == nil {
		return "", nil
	}
	for _, f := range files[1:] {
		if fileSets(f, keyExecStart) {
			return "", fmt.Errorf("%s: %s replaces ExecStart=", name, f.path)
		}
	}
	for _, line := range files[0].lines {
		if key, val, ok := entry(line); ok && key == keyExecStart {
			bin, _, _ := strings.Cut(val, " ")
			bin = strings.TrimLeft(bin, "-@:+!")
			if isPlain(bin) && filepath.IsAbs(bin) {
				return bin, nil
			}
			return "", fmt.Errorf("%s: ExecStart= program %q is not a plain absolute path", name, bin)
		}
	}
	return "", fmt.Errorf("%s: no ExecStart=", name)
}

// InstalledSpec reports the spec the installed appliance unit was written
// from: user, config path, state directory and binary. It reads the unit
// file alone, never its drop-ins, and accepts it only when it is byte for
// byte what the installer renders from that spec with the current template
// or a released one. A zero spec and no error means no unit is installed; an
// error means the unit cannot be read as installed or was edited by hand.
func InstalledSpec() (ServiceSpec, error) {
	unit, err := findUnit(DefaultUnitName)
	if err != nil || unit == "" {
		return ServiceSpec{}, err
	}
	raw, lines, err := readLines(unit)
	if err != nil {
		return ServiceSpec{}, err
	}
	if len(raw) == 0 {
		return ServiceSpec{}, fmt.Errorf("%s is masked (empty)", DefaultUnitName)
	}
	return specFromUnit(unitFile{path: unit, raw: raw, lines: lines, specific: true})
}

// specFromUnit reads the spec from the installer's lines of f and checks that
// it validates and that f is exactly what the installer renders from it.
func specFromUnit(f unitFile) (ServiceSpec, error) {
	var s ServiceSpec
	for _, line := range f.lines {
		key, val, ok := entry(line)
		if !ok {
			continue
		}
		switch key {
		case keyUser:
			s.User = val
		case keyEnvironment:
			s.ConfigPath, _ = strings.CutPrefix(val, ConfigEnv+"=")
		case keyExecStart:
			bin, rest, _ := strings.Cut(val, " ")
			s.BinPath = bin
			s.StateDir, _ = strings.CutPrefix(rest, "serve --cert-dir=")
		}
	}
	edited := fmt.Errorf("%s is not the unit the installer writes (edited by hand?)", f.path)
	if s.User == "" || s.ConfigPath == "" || s.StateDir == "" || s.BinPath == "" {
		return ServiceSpec{}, edited
	}
	if err := s.Validate(); err != nil {
		return ServiceSpec{}, fmt.Errorf("%s names paths this version does not accept: %w", f.path, err)
	}
	for _, tmpl := range releasedUnitTmpls {
		b, err := render(tmpl, s)
		if err == nil && bytes.Equal(b, f.raw) {
			return s, nil
		}
	}
	return ServiceSpec{}, edited
}

// InstalledConfig reports the config path and service account of the
// installed appliance unit, as its next start would use them. Path and user
// are "" with no error when no appliance unit is installed. User is "" when a
// drop-in clears it.
//
// It accepts only what it reads completely: the unit must be exactly what
// the installer writes (see InstalledSpec), and a drop-in that sets any of
// relevantKeys must be name-specific (remote-mic.service.d) and hold nothing
// but the [Service] header, Environment= lines of plain words (see isPlain)
// and a plain User= name. Anything else, a config path under privateDirs,
// or a script at the unit's program path returns an error, so a caller asks
// for --config instead of guessing. It reads the files, not the unit systemd
// has loaded or the process running now: an edit awaiting `systemctl
// daemon-reload`, or one made since the appliance started, already counts.
//
// It parses the files rather than asking systemctl, so a CLI command neither
// forks nor needs systemd running.
func InstalledConfig() (path, user string, err error) {
	files, err := loadUnit(DefaultUnitName)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", DefaultUnitName, err)
	}
	if files == nil {
		return "", "", nil
	}
	spec, err := specFromUnit(files[0])
	if err != nil {
		return "", "", err
	}
	path, user = spec.ConfigPath, spec.User
	for _, f := range files[1:] {
		if !slices.ContainsFunc(relevantKeys, func(k string) bool { return fileSets(f, k) }) {
			continue
		}
		if !f.specific {
			return "", "", fmt.Errorf("%s: a prefix or type-level drop-in sets a setting the reader does not follow", f.path)
		}
		if len(f.raw) > maxDropIn {
			return "", "", fmt.Errorf("%s: larger than %d bytes", f.path, maxDropIn)
		}
		if path, user, err = applyDropIn(f, path, user); err != nil {
			return "", "", fmt.Errorf("%s: %w", f.path, err)
		}
	}
	switch {
	case path == "":
		return "", "", fmt.Errorf("%s: %s is not set", DefaultUnitName, ConfigEnv)
	case inPrivateDir(path):
		return "", "", fmt.Errorf("%s: %s=%s is in a directory the service may see differently", DefaultUnitName, ConfigEnv, path)
	}
	if err := checkProgram(spec.BinPath); err != nil {
		return "", "", err
	}
	return path, user, nil
}

// applyDropIn applies a name-specific drop-in that sets a relevant key. Every
// line must be one the reader follows completely, so no earlier line can make
// systemd stop reading the file before an assignment the reader applies.
func applyDropIn(f unitFile, path, user string) (newPath, newUser string, err error) {
	newPath, newUser = path, user
	inService := false
	for _, line := range f.lines {
		if line == "[Service]" {
			inService = true
			continue
		}
		if !inService {
			return "", "", fmt.Errorf("holds %q before [Service], which the reader does not follow", line)
		}
		key, val, ok := entry(line)
		switch {
		case ok && key == keyEnvironment && val == "":
			newPath = ""
		case ok && key == keyEnvironment:
			for w := range strings.FieldsFuncSeq(val, func(r rune) bool { return r == ' ' || r == '\t' }) {
				if !isPlain(w) {
					return "", "", errors.New("an Environment= value is not plain")
				}
				if v, found := strings.CutPrefix(w, ConfigEnv+"="); found {
					if !filepath.IsAbs(v) || filepath.Clean(v) != v {
						return "", "", fmt.Errorf("%s=%s is not a clean absolute path", ConfigEnv, v)
					}
					newPath = v
				}
			}
		case ok && key == keyUser && (val == "" || userNameRe.MatchString(val)):
			newUser = val
		default:
			return "", "", fmt.Errorf("holds %q, which the reader does not follow", line)
		}
	}
	return newPath, newUser, nil
}

// fileSets reports whether any line of f assigns key, in any section.
func fileSets(f unitFile, key string) bool {
	for _, line := range f.lines {
		if k, _, ok := entry(line); ok && k == key {
			return true
		}
	}
	return false
}

// inPrivateDir reports whether path lies in one of privateDirs, lexically or
// after resolving symbolic links: the whole path when it exists, else its
// directory. Resolution is best effort; a directory this caller cannot
// search leaves the lexical check, and the caller's own open of the file
// then fails in the same place.
func inPrivateDir(path string) bool {
	paths := []string{path}
	resolved, err := filepath.EvalSymlinks(hostPath(path))
	if errors.Is(err, fs.ErrNotExist) {
		if dir, derr := filepath.EvalSymlinks(hostPath(filepath.Dir(path))); derr == nil {
			resolved, err = filepath.Join(dir, filepath.Base(path)), nil
		}
	}
	if err == nil {
		if rel, rerr := filepath.Rel(unitRoot, resolved); rerr == nil {
			paths = append(paths, string(filepath.Separator)+rel)
		}
	}
	for _, p := range paths {
		for _, d := range privateDirs {
			if within(p, d) {
				return true
			}
		}
	}
	return false
}

// checkProgram fails when the installed binary is a script: a wrapper there
// could set REMOTEMIC_CONFIG itself. A binary this caller cannot open is
// left alone, since nothing about it can be told from here.
func checkProgram(bin string) error {
	f, err := os.Open(hostPath(bin))
	if err != nil {
		return nil //nolint:nilerr // unreadable here says nothing about what the service runs
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 2)
	if n, _ := io.ReadFull(f, head); n == 2 && string(head) == "#!" {
		return fmt.Errorf("%s runs %s, a script that may set %s itself", DefaultUnitName, bin, ConfigEnv)
	}
	return nil
}
