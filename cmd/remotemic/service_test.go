//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	selfBin    = "/usr/local/bin/remote-mic"
	optBin     = "/opt/bin/remote-mic"
	rmState    = "/srv/rm-state"
	micUser    = "mic"
	rmConfig   = "/srv/rm/config.yaml"
)

// saveServiceSeams snapshots the package seams the service tests mutate and
// restores them on cleanup, so cases do not leak state into each other.
func saveServiceSeams(t *testing.T) {
	t.Helper()
	ge, lp, ox, es := geteuid, lookPath, osExecutable, execSelf
	it, inst, uninst, st := stdinIsTerminal, installService, uninstallService, statusService
	args, po := os.Args, packageOwns
	packageOwns = func(string) bool { return false }
	t.Cleanup(func() {
		packageOwns = po
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
	osExecutable = func() (string, error) { return selfBin, nil }
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
	want := []string{sudoPath, selfBin, cmdService, cmdInstall, flagUser, userBird, escalateGuard}
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
	var gotStart, gotAllow bool
	installService = func(spec service.ServiceSpec, start, allowDowngrade bool) error {
		gotSpec, gotStart, gotAllow = spec, start, allowDowngrade
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{
		cmdService, cmdInstall,
		flagUser, userBird,
		"--config", "/etc/bird/config.yaml",
		"--state-dir", "/var/lib/bird",
		"--bin-path", "/usr/local/bin/bird",
		"--no-start",
		"--allow-downgrade",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	// Every flag must thread into the spec, not just --user.
	want := service.ServiceSpec{User: userBird, ConfigPath: "/etc/bird/config.yaml", StateDir: "/var/lib/bird", BinPath: "/usr/local/bin/bird"}
	if gotSpec != want {
		t.Errorf("spec = %+v, want %+v", gotSpec, want)
	}
	if gotStart {
		t.Error("--no-start should pass start=false")
	}
	if !gotAllow {
		t.Error("--allow-downgrade should pass allowDowngrade=true")
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

// TestDispatchServiceGuardOnly guards the panic fix: input that is only the
// hidden escalation guard (no subcommand) must print usage and exit 2, not
// panic on an empty argument slice after the guard is stripped.
func TestDispatchServiceGuardOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{cmdService, escalateGuard}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestServiceInstallBadFlagNoEscalation(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 1000 }
	// Make escalation fully reachable: with a TTY, sudo, and a resolvable self,
	// ensureRoot WOULD re-exec and hit this fatal if parsing did not fail first.
	// So the fatal genuinely enforces "parse before escalate".
	stdinIsTerminal = func() bool { return true }
	lookPath = func(string) (string, error) { return sudoPath, nil }
	osExecutable = func() (string, error) { return selfBin, nil }
	execSelf = func(string, []string, []string) error {
		t.Fatal("a bad flag must fail at parse, before any sudo re-exec")
		return nil
	}
	var stderr bytes.Buffer
	if err := runServiceInstall([]string{"--nope"}, false, &stderr); err == nil {
		t.Fatal("bad flag = nil error, want usage error")
	}
}

// TestNewInstaller pins that the production installer knows this binary's
// version and the --allow-downgrade choice, which its downgrade guard needs.
func TestNewInstaller(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v9.9.9"
	in := newInstaller(service.ServiceSpec{}, true)
	if in.Version != "v9.9.9" || !in.AllowDowngrade {
		t.Errorf("got Version %q, AllowDowngrade %t; want v9.9.9, true", in.Version, in.AllowDowngrade)
	}
	if newInstaller(service.ServiceSpec{}, false).AllowDowngrade {
		t.Error("AllowDowngrade set without the flag")
	}
}

// TestServiceInstallDoesNotAllowDowngradeByDefault pins that a plain
// `service install` keeps the downgrade guard on.
func TestServiceInstallDoesNotAllowDowngradeByDefault(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 } // skip escalation
	oldSpec := installedSpec
	t.Cleanup(func() { installedSpec = oldSpec })
	installedSpec = func() (service.ServiceSpec, error) { return service.ServiceSpec{}, nil }
	got := true
	installService = func(_ service.ServiceSpec, _, allowDowngrade bool) error {
		got = allowDowngrade
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := dispatch([]string{cmdService, cmdInstall, "--no-start"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if got {
		t.Error("allowDowngrade = true without --allow-downgrade")
	}
}

// packagedBinary points osExecutable at a real file, so the path packagedSelf
// resolves is one the test controls, and makes the package own it.
func packagedBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "remote-mic")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	osExecutable = func() (string, error) { return bin, nil }
	packageOwns = func(p string) bool { return p == resolved }
	return resolved
}

// TestServiceInstallRunsThePackagedBinaryInPlace asserts that, from a binary
// the .deb owns, an install with no --bin-path takes that binary's own path
// over the installed unit's, and that an explicit --bin-path keeps the copy.
func TestServiceInstallRunsThePackagedBinaryInPlace(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	bin := packagedBinary(t)
	prev := installedSpec
	installedSpec = func() (service.ServiceSpec, error) {
		return service.ServiceSpec{User: micUser, ConfigPath: rmConfig, StateDir: rmState, BinPath: selfBin}, nil
	}
	t.Cleanup(func() { installedSpec = prev })
	var got service.ServiceSpec
	installService = func(s service.ServiceSpec, _, _ bool) error { got = s; return nil }

	code, _, errOut := runCLI(cmdService, cmdInstall, "--no-start")
	want := service.ServiceSpec{User: micUser, ConfigPath: rmConfig, StateDir: rmState, BinPath: bin}
	if code != 0 || got != want {
		t.Fatalf("install: exit %d spec %+v stderr %q, want %+v", code, got, errOut, want)
	}
	for _, line := range []string{"running the packaged " + bin + " in place", "using --user=mic", "running the packaged binary (update it with apt)", "not started"} {
		if !strings.Contains(errOut, line) {
			t.Errorf("install stderr %q, want %q", errOut, line)
		}
	}
	if strings.Contains(errOut, "using --bin-path") {
		t.Errorf("install stderr %q reports adopting a bin path over the packaged binary", errOut)
	}

	code, _, errOut = runCLI(cmdService, cmdInstall, "--bin-path", "/opt/rm/remote-mic")
	if code != 0 || got.BinPath != "/opt/rm/remote-mic" || strings.Contains(errOut, "running the packaged") {
		t.Errorf("explicit --bin-path: exit %d spec %+v stderr %q, want the copy path kept", code, got, errOut)
	}
}

// TestServiceInstallOfANonPackagedBinaryTakesTheInstalledPath asserts that a
// binary the package does not own still adopts the installed unit's path.
func TestServiceInstallOfANonPackagedBinaryTakesTheInstalledPath(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	osExecutable = func() (string, error) { return selfBin, nil }
	prev := installedSpec
	installedSpec = func() (service.ServiceSpec, error) {
		return service.ServiceSpec{User: micUser, ConfigPath: rmConfig, StateDir: rmState, BinPath: optBin}, nil
	}
	t.Cleanup(func() { installedSpec = prev })
	var got service.ServiceSpec
	installService = func(s service.ServiceSpec, _, _ bool) error { got = s; return nil }
	if code, _, errOut := runCLI(cmdService, cmdInstall); code != 0 || got.BinPath != optBin || strings.Contains(errOut, "packaged") {
		t.Errorf("install: exit %d spec %+v stderr %q, want the installed unit's bin path", code, got, errOut)
	}
}

// TestServicePurgeAdoptsAPackagedBinPath asserts that --purge takes a
// package-owned bin path from the installed unit without it being given, and
// says the package removes it.
func TestServicePurgeAdoptsAPackagedBinPath(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	packageOwns = func(p string) bool { return p == "/usr/bin/remote-mic" }
	prev := installedSpec
	def := service.ServiceSpec{User: service.DefaultUser, ConfigPath: service.DefaultConfigPath, StateDir: service.DefaultStateDir, BinPath: "/usr/bin/remote-mic"}
	installedSpec = func() (service.ServiceSpec, error) { return def, nil }
	t.Cleanup(func() { installedSpec = prev })
	var purged service.ServiceSpec
	uninstallService = func(s service.ServiceSpec, _ bool) error { purged = s; return nil }
	code, _, errOut := runCLI(cmdService, subRemove, argPurge)
	if code != 0 || purged != def {
		t.Fatalf("purge: exit %d spec %+v stderr %q, want %+v", code, purged, errOut, def)
	}
	if !strings.Contains(errOut, "the package owns /usr/bin/remote-mic, so apt removes it") {
		t.Errorf("purge stderr %q, want a note that apt removes the binary", errOut)
	}
}
