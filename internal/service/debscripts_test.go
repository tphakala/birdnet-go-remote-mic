//go:build linux

package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runDebScript runs packaging/deb/<script> with a stub systemctl first on PATH
// that logs its arguments and answers `show` with unit, exiting 1 for the verb
// failVerb. It returns the logged systemctl calls and the exit code.
func runDebScript(t *testing.T, script, unit, failVerb string, args ...string) (calls []string, code int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	unitFile := filepath.Join(dir, "unit")
	if err := os.WriteFile(unitFile, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
echo "$*" >> "$STUB_LOG"
[ "$1" = "$STUB_FAIL" ] && exit 1
[ "$1" = show ] && { cat "$STUB_UNIT"; exit 0; }
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", append([]string{filepath.Join("..", "..", "packaging", "deb", script)}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STUB_LOG="+log, "STUB_UNIT="+unitFile)
	if failVerb != "" {
		cmd.Env = append(cmd.Env, "STUB_FAIL="+failVerb)
	}
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", script, err)
	}
	b, _ := os.ReadFile(log)
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls, code
}

const (
	prevVersion  = "0.3.0"
	argConfigure = "configure"
	argRemove    = "remove"
	showUnit     = "show --property=ExecStart --value remote-mic.service"
	unitPackaged = "{ path=/usr/bin/remote-mic ; argv[]=/usr/bin/remote-mic serve --cert-dir=/var/lib/remote-mic ; ignore_errors=no }\n"
	unitCopy     = "{ path=/usr/local/bin/remote-mic ; argv[]=/usr/local/bin/remote-mic serve --cert-dir=/var/lib/remote-mic ; ignore_errors=no }\n"
	// unitOverridden is a unit whose drop-in replaced ExecStart with a wrapper
	// that merely names the packaged binary as an argument.
	unitOverridden = "{ path=/opt/wrap ; argv[]=/opt/wrap /usr/bin/remote-mic serve ; ignore_errors=no }\n"
)

func TestDebPostinst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		unit string
		fail string // the systemctl verb the stub fails
		args []string
		want []string
	}{
		{"upgrade of a packaged unit restarts only if running", unitPackaged, "", []string{argConfigure, prevVersion}, []string{showUnit, "try-restart remote-mic.service"}},
		{"first install touches nothing", unitPackaged, "", []string{argConfigure}, nil},
		{"a unit that runs a copy is left alone", unitCopy, "", []string{argConfigure, prevVersion}, []string{showUnit}},
		{"a drop-in that replaces ExecStart is left alone", unitOverridden, "", []string{argConfigure, prevVersion}, []string{showUnit}},
		{"no unit", "", "show", []string{argConfigure, prevVersion}, []string{showUnit}},
		{"abort-upgrade", unitPackaged, "", []string{"abort-upgrade", prevVersion}, nil},
		{"a failing try-restart does not fail the upgrade", unitPackaged, "try-restart", []string{argConfigure, prevVersion}, []string{showUnit, "try-restart remote-mic.service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls, code := runDebScript(t, "postinst", tc.unit, tc.fail, tc.args...)
			if code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
			if strings.Join(calls, "|") != strings.Join(tc.want, "|") {
				t.Errorf("systemctl calls %q, want %q", calls, tc.want)
			}
			for _, c := range calls {
				if strings.HasPrefix(c, "enable") || strings.HasPrefix(c, "start") {
					t.Errorf("postinst ran systemctl %s; it must never enable or start", c)
				}
			}
		})
	}
}

func TestDebPrerm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		unit string
		args []string
		want []string
	}{
		{"remove disables a packaged unit", unitPackaged, []string{argRemove}, []string{showUnit, "disable --now remote-mic.service"}},
		{"upgrade does nothing", unitPackaged, []string{"upgrade", "0.4.0"}, nil},
		{"deconfigure does nothing", unitPackaged, []string{"deconfigure"}, nil},
		{"remove leaves a copy's unit alone", unitCopy, []string{argRemove}, []string{showUnit}},
		{"remove leaves an overriding drop-in alone", unitOverridden, []string{argRemove}, []string{showUnit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls, code := runDebScript(t, "prerm", tc.unit, "", tc.args...)
			if code != 0 || strings.Join(calls, "|") != strings.Join(tc.want, "|") {
				t.Errorf("exit %d systemctl calls %q, want exit 0 and %q", code, calls, tc.want)
			}
		})
	}
}
