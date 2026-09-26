//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
)

// readUnitFile is a seam so InstalledBinPaths is testable without real units.
var readUnitFile = os.ReadFile

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
