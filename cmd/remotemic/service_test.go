//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/service"
)

// Repeated fixture strings, factored out to satisfy goconst.
const (
	sudoPath   = "/usr/bin/sudo"
	flagUser   = "--user"
	userBird   = "bird"
	cmdService = "service"
	cmdInstall = "install"
)

// saveServiceSeams snapshots the package seams the service tests mutate and
// restores them on cleanup, so cases do not leak state into each other.
func saveServiceSeams(t *testing.T) {
	t.Helper()
	ge, lp, ox, es := geteuid, lookPath, osExecutable, execSelf
	it, inst, uninst, st := stdinIsTerminal, installService, uninstallService, statusService
	args := os.Args
	t.Cleanup(func() {
		geteuid, lookPath, osExecutable, execSelf = ge, lp, ox, es
		stdinIsTerminal, installService, uninstallService, statusService = it, inst, uninst, st
		os.Args = args
	})
}

func TestEnsureRootNoopWhenRoot(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	called := false
	execSelf = func(string, []string, []string) error { called = true; return nil }
	if err := ensureRoot(false); err != nil {
		t.Fatalf("ensureRoot as root = %v, want nil", err)
	}
	if called {
		t.Error("must not re-exec when already root")
	}
}

func TestEnsureRootReexecsUnderSudo(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 1000 }
	stdinIsTerminal = func() bool { return true }
	lookPath = func(string) (string, error) { return sudoPath, nil }
	osExecutable = func() (string, error) { return "/usr/local/bin/remote-mic", nil }
	os.Args = []string{"remote-mic", cmdService, cmdInstall, flagUser, userBird}
	var gotArgv0 string
	var gotArgv []string
	execSelf = func(argv0 string, argv, _ []string) error {
		gotArgv0, gotArgv = argv0, argv
		return nil
	}
	if err := ensureRoot(false); err != nil {
		t.Fatalf("ensureRoot = %v, want nil (re-exec)", err)
	}
	if gotArgv0 != sudoPath {
		t.Errorf("argv0 = %q, want /usr/bin/sudo", gotArgv0)
	}
	want := []string{sudoPath, "/usr/local/bin/remote-mic", cmdService, cmdInstall, flagUser, userBird, escalateGuard}
	if !reflect.DeepEqual(gotArgv, want) {
		t.Errorf("argv = %v, want %v", gotArgv, want)
	}
}

func TestEnsureRootRefusesLoopWhenEscalated(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 1000 }
	called := false
	execSelf = func(string, []string, []string) error { called = true; return nil }
	if err := ensureRoot(true); err == nil {
		t.Fatal("ensureRoot(escalated) still non-root = nil, want refusal")
	}
	if called {
		t.Error("must not re-exec again after a prior escalation")
	}
}

func TestEnsureRootRefusesWithoutTTY(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 1000 }
	stdinIsTerminal = func() bool { return false }
	called := false
	execSelf = func(string, []string, []string) error { called = true; return nil }
	if err := ensureRoot(false); err == nil {
		t.Fatal("ensureRoot without a TTY = nil, want refusal")
	}
	if called {
		t.Error("must not re-exec without a terminal to prompt on")
	}
}

func TestStripGuard(t *testing.T) {
	esc, rest := stripGuard([]string{cmdInstall, flagUser, userBird, escalateGuard})
	if !esc {
		t.Error("guard present but escalated = false")
	}
	if !reflect.DeepEqual(rest, []string{cmdInstall, flagUser, userBird}) {
		t.Errorf("rest = %v, want [install --user bird]", rest)
	}
	esc, rest = stripGuard([]string{cmdInstall})
	if esc {
		t.Error("no guard but escalated = true")
	}
	if !reflect.DeepEqual(rest, []string{cmdInstall}) {
		t.Errorf("rest = %v, want [install]", rest)
	}
}

func TestDispatchServiceInstall(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 } // skip escalation
	var gotSpec service.ServiceSpec
	var gotStart bool
	installService = func(spec service.ServiceSpec, start bool) error {
		gotSpec, gotStart = spec, start
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{cmdService, cmdInstall, flagUser, userBird, "--no-start"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if gotSpec.User != userBird {
		t.Errorf("spec.User = %q, want bird", gotSpec.User)
	}
	if gotStart {
		t.Error("--no-start should pass start=false")
	}
}

func TestDispatchServiceUninstallPurge(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	var gotPurge bool
	uninstallService = func(_ service.ServiceSpec, purge bool) error { gotPurge = purge; return nil }
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{cmdService, "uninstall", "--purge"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !gotPurge {
		t.Error("--purge should pass purge=true")
	}
}

func TestDispatchServiceStatus(t *testing.T) {
	saveServiceSeams(t)
	called := false
	statusService = func(w io.Writer) error {
		called = true
		out(w, "unit: remote-mic.service\n")
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{cmdService, "status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !called {
		t.Error("status action was not invoked")
	}
}

func TestDispatchServiceUnknown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{cmdService, "bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestServiceInstallBadFlagNoEscalation(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 1000 }
	execSelf = func(string, []string, []string) error {
		t.Fatal("a bad flag must fail at parse, before any sudo re-exec")
		return nil
	}
	var stderr bytes.Buffer
	// An unknown flag fails at parse, before ensureRoot, so no escalation.
	if err := runServiceInstall([]string{"--nope"}, false, &stderr, &stderr); err == nil {
		t.Fatal("bad flag = nil error, want usage error")
	}
}
