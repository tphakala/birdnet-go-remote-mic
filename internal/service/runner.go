//go:build linux

package service

import (
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs an external command and returns its combined output. It is the
// package seam that makes the installer and the systemd wrapper testable
// without invoking real useradd, chown, or systemctl: a test injects a Runner
// that records the calls and returns canned output.
type Runner func(name string, args ...string) ([]byte, error)

// execRunner is the production Runner. It captures combined stdout and stderr so
// a failure carries the tool's own diagnostic, and wraps the error with the
// command line so the caller reports exactly what ran.
func execRunner(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed tool names; args are internal constants and validated spec fields
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
