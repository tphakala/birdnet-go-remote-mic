//go:build linux

package service

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// readUnitFile and globDropIns are seams so InstalledBinPaths and
// InstalledConfig are testable without real units.
var (
	readUnitFile = os.ReadFile
	globDropIns  = filepath.Glob
)

// InstalledBinPaths reports the binary the installed appliance unit runs and
// the binary the installed root updater unit installs to, each "" when its
// unit is absent or names none. The appliance compares them with its own path
// to decide whether it can update itself.
func InstalledBinPaths() (appliance, updater string) {
	return unitBinPath(filepath.Join(unitDir, DefaultUnitName)), unitBinPath(filepath.Join(unitDir, UpdateServiceUnit))
}

// unitBinPath returns the program of a unit's ExecStart line, or "".
func unitBinPath(path string) string {
	b, err := readUnitFile(path)
	if err != nil {
		return ""
	}
	return execStartBin(string(b))
}

// execStartBin returns the program an ExecStart= line runs: the first word,
// without systemd's special-executable prefixes (-, @, :, +, !).
func execStartBin(unit string) string {
	for line := range strings.Lines(unit) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return ""
		}
		return strings.TrimLeft(fields[0], "-@:+!")
	}
	return ""
}

// InstalledConfig reports the config path and service account of the installed
// appliance unit, each "" when the unit is absent or names none. It reads the
// unit's REMOTEMIC_CONFIG and User= lines, then the drop-ins in
// remote-mic.service.d in lexical order, so an override made with
// `systemctl edit remote-mic` wins as it does in systemd. A relative config
// path is treated as absent: the installer only writes absolute ones, and a
// relative one would resolve against whatever directory the CLI runs in.
//
// It parses the files rather than asking systemctl, so a CLI command neither
// forks nor needs systemd as PID 1. The parse covers what the installer writes
// and plain overrides, not every systemd feature (EnvironmentFile=, specifiers,
// line continuations).
func InstalledConfig() (path, user string) {
	unit := filepath.Join(unitDir, DefaultUnitName)
	b, err := readUnitFile(unit)
	if err != nil {
		return "", ""
	}
	texts := []string{string(b)}
	dropIns, _ := globDropIns(filepath.Join(unit+".d", "*.conf"))
	slices.Sort(dropIns)
	for _, d := range dropIns {
		if b, err := readUnitFile(d); err == nil {
			texts = append(texts, string(b))
		}
	}
	for _, text := range texts {
		path, user = serviceConfig(text, path, user)
	}
	if !filepath.IsAbs(path) {
		path = ""
	}
	return path, user
}

// serviceConfig applies one unit file's [Service] Environment= and User= lines
// to the path and user found so far. A later assignment replaces an earlier
// one, and an empty Environment= or User= resets it, as in systemd.
func serviceConfig(text, path, user string) (newPath, newUser string) {
	inService := false
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inService = line == "[Service]"
			continue
		}
		if !inService {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "User":
			user = strings.TrimSpace(val)
		case "Environment":
			words := unitWords(val)
			if len(words) == 0 {
				path = ""
			}
			for _, w := range words {
				if v, ok := strings.CutPrefix(w, "REMOTEMIC_CONFIG="); ok {
					path = v
				}
			}
		}
	}
	return path, user
}

// unitWords splits an Environment= value into its assignments: words separated
// by whitespace, where single or double quotes group a word and are removed,
// and a backslash takes the next character literally.
func unitWords(s string) []string {
	var (
		words           []string
		cur             strings.Builder
		quote           rune
		inWord, escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped, inWord = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}
