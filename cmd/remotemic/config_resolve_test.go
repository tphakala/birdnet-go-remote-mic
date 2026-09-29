//go:build linux

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/service"
)

// Command words the resolution tests run.
const (
	subGet      = "get"
	subGenerate = "generate"
	subSet      = "set"
	argForce    = "-force"
	argPurge    = "-purge"
	argCheck    = "-check"
	subRemove   = "uninstall"
)

// stubInstalledConfig makes the installed unit name path and user.
func stubInstalledConfig(t *testing.T, path, user string) {
	t.Helper()
	prev := installedConfig
	installedConfig = func() (string, string, error) { return path, user, nil }
	t.Cleanup(func() { installedConfig = prev })
}

// TestResolveConfigPrecedence pins the order: --config, then $REMOTEMIC_CONFIG,
// then the installed unit's config, then config.yaml in the working directory.
func TestResolveConfigPrecedence(t *testing.T) {
	const unitCfg, envCfg = "/etc/remote-mic/config.yaml", "/e.yaml"
	for _, tc := range []struct {
		name, flag, env, unit string
		wantPath              string
		wantSource            configSource
	}{
		{"flag wins", "/f.yaml", envCfg, unitCfg, "/f.yaml", fromFlag},
		{"env over unit", "", envCfg, unitCfg, envCfg, fromEnv},
		{"unit over cwd", "", "", unitCfg, unitCfg, fromUnit},
		{"cwd without a unit", "", "", "", configDefault, fromCwd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInstalledConfig(t, tc.unit, service.DefaultUser)
			t.Setenv(configEnv, tc.env)
			t.Chdir(t.TempDir())
			set := flag.NewFlagSet("t", flag.ContinueOnError)
			path := configFlag(set)
			var args []string
			if tc.flag != "" {
				args = []string{flagConfig, tc.flag}
			}
			if err := set.Parse(args); err != nil {
				t.Fatal(err)
			}
			ref, err := resolveConfig(set, *path, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if ref.path != tc.wantPath || ref.source != tc.wantSource {
				t.Errorf("got %q (source %d), want %q (source %d)", ref.path, ref.source, tc.wantPath, tc.wantSource)
			}
			if ref.unitPath != tc.unit {
				t.Errorf("unitPath %q, want %q", ref.unitPath, tc.unit)
			}
		})
	}
}

// TestParseServeFlagsUsesInstalledConfig asserts a bare serve on an installed
// host checks and serves the unit's config, not one in the working directory.
func TestParseServeFlagsUsesInstalledConfig(t *testing.T) {
	const unitCfg = "/etc/remote-mic/config.yaml"
	stubInstalledConfig(t, unitCfg, service.DefaultUser)
	t.Setenv(configEnv, "")
	t.Chdir(t.TempDir())
	cfg, _, _, _, err := parseServeFlags([]string{argCheck}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfg.path != unitCfg {
		t.Errorf("cfgPath = %q, want %q", cfg.path, unitCfg)
	}
}

// installedFixture lays out an installed appliance's config holding tokenOld
// and a stray ./config.yaml without a token in the working directory, and
// returns the installed config's path.
func installedFixture(t *testing.T) string {
	t.Helper()
	unitCfg := filepath.Join(t.TempDir(), "config.yaml")
	seedConfigWithToken(t, unitCfg, tokenOld)
	stubInstalledConfig(t, unitCfg, service.DefaultUser)
	t.Setenv(configEnv, "")
	t.Chdir(t.TempDir())
	seedConfigWithToken(t, configDefault, "")
	return unitCfg
}

// TestTokenGetUsesInstalledConfig is the regression for a stray config.yaml
// in the working directory: token get prints the installed config's token and
// says on stderr which file it used, leaving stdout the bare token.
func TestTokenGetUsesInstalledConfig(t *testing.T) {
	unitCfg := installedFixture(t)
	code, out, errOut := runCLI("token", "get")
	if code != 0 || out != tokenOld+"\n" {
		t.Fatalf("exit %d stdout %q stderr %q, want the installed config's token", code, out, errOut)
	}
	if !strings.Contains(errOut, "using "+unitCfg+" (from remote-mic.service); ./config.yaml is ignored") {
		t.Errorf("stderr %q, want a note naming the installed config", errOut)
	}
}

// TestTokenGenerateUsesInstalledConfig asserts a write command edits the
// installed config and leaves the stray ./config.yaml untouched.
func TestTokenGenerateUsesInstalledConfig(t *testing.T) {
	unitCfg := installedFixture(t)
	code, out, errOut := runCLI(cmdToken, subGenerate, argForce, "-quiet")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if got, want := loadToken(t, unitCfg), strings.TrimSpace(out); got != want {
		t.Errorf("installed config token %q, want the generated %q", got, want)
	}
	if got := loadToken(t, configDefault); got != "" {
		t.Errorf("./config.yaml token %q, want it untouched (empty)", got)
	}
	if errOut != "" {
		t.Errorf("stderr %q under -quiet, want nothing (the note too is guidance)", errOut)
	}
}

// TestTokenGetMissingInstalledConfig asserts the error names the unit when the
// config it points at does not exist, rather than suggesting ./config.yaml.
func TestTokenGetMissingInstalledConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.yaml")
	stubInstalledConfig(t, missing, service.DefaultUser)
	t.Setenv(configEnv, "")
	t.Chdir(t.TempDir())
	code, _, errOut := runCLI("token", "get")
	if code != 1 || !strings.Contains(errOut, "the config "+missing+" named by remote-mic.service does not exist") {
		t.Errorf("exit %d stderr %q, want an error naming the unit's config", code, errOut)
	}
}

// TestTokenGetUnreadableInstalledConfig asserts a permission failure on the
// installed config names the unit's account and the command to run.
func TestTokenGetUnreadableInstalledConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	unitCfg := installedFixture(t)
	if err := os.Chmod(unitCfg, 0); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("token", "get")
	want := "run this command as the appliance account: sudo -u remote-mic remote-mic token get --config " + unitCfg
	if code != 1 || out != "" || !strings.Contains(errOut, want) {
		t.Errorf("exit %d stdout %q stderr %q, want %q", code, out, errOut, want)
	}
}

// TestConfigRefExplain pins when a permission failure gets the exact command:
// only for the unit's config, with a known account, and an unwrapped error.
func TestConfigRefExplain(t *testing.T) {
	t.Parallel()
	const unitCfg = "/etc/remote-mic/config.yaml"
	denied := withPermHint(&fs.PathError{Op: "open", Path: unitCfg, Err: fs.ErrPermission})
	specific := "run this command as the appliance account: sudo -u remote-mic remote-mic token clear -yes --config " + unitCfg
	for _, tc := range []struct {
		name         string
		ref          configRef
		err          error
		wantSpecific bool
	}{
		{"unit config", configRef{path: unitCfg, unitPath: unitCfg, unitUser: service.DefaultUser}, denied, true},
		{"another config", configRef{path: "/srv/other.yaml", unitPath: unitCfg, unitUser: service.DefaultUser}, denied, false},
		{"no account in the unit", configRef{path: unitCfg, unitPath: unitCfg}, denied, false},
		{"wrapped", configRef{path: unitCfg, unitPath: unitCfg, unitUser: service.DefaultUser}, fmt.Errorf("context: %w", denied), false},
	} {
		got := tc.ref.explain(tc.err, "token clear", []string{"-yes"})
		if strings.Contains(got.Error(), specific) != tc.wantSpecific {
			t.Errorf("%s: got %q, want specific hint %t", tc.name, got, tc.wantSpecific)
		}
		if !errors.Is(got, fs.ErrPermission) {
			t.Errorf("%s: %v no longer wraps fs.ErrPermission", tc.name, got)
		}
	}
	if err := (configRef{}).explain(nil, "token get", nil); err != nil {
		t.Errorf("explain(nil) = %v, want nil", err)
	}
}

// TestServiceDefaultsFromInstalledUnit asserts service install and
// uninstall --purge take every unset flag from the installed unit, report
// each one, and keep a flag the operator gave; a plain uninstall does not
// read the unit.
func TestServiceDefaultsFromInstalledUnit(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	inst := service.ServiceSpec{User: micUser, ConfigPath: rmConfig, StateDir: rmState, BinPath: optBin}
	prev := installedSpec
	reads := 0
	installedSpec = func() (service.ServiceSpec, error) { reads++; return inst, nil }
	t.Cleanup(func() { installedSpec = prev })

	var got service.ServiceSpec
	installService = func(s service.ServiceSpec, _, _ bool) error { got = s; return nil }
	code, _, errOut := runCLI(cmdService, cmdInstall, "--state-dir", "/srv/other", "--no-start")
	want := inst
	want.StateDir = "/srv/other"
	if code != 0 || got != want {
		t.Fatalf("install: exit %d spec %+v stderr %q, want %+v", code, got, errOut, want)
	}
	for _, line := range []string{"using --user=mic", "using --config=/srv/rm/config.yaml", "using --bin-path=/opt/bin/remote-mic"} {
		if !strings.Contains(errOut, line) {
			t.Errorf("install stderr %q, want %q", errOut, line)
		}
	}
	if strings.Contains(errOut, "using --state-dir") {
		t.Errorf("install stderr %q reports a flag the operator gave", errOut)
	}

	var purged service.ServiceSpec
	called := false
	uninstallService = func(s service.ServiceSpec, _ bool) error { purged, called = s, true; return nil }
	code, _, errOut = runCLI(cmdService, subRemove, argPurge)
	if code != 1 || called || !strings.Contains(errOut, "uses --user=mic --config=/srv/rm/config.yaml --state-dir=/srv/rm-state --bin-path=/opt/bin/remote-mic; --purge deletes those only when they are given explicitly") {
		t.Fatalf("purge of a custom install: exit %d called %t stderr %q, want a refusal naming each custom value", code, called, errOut)
	}
	code, _, errOut = runCLI(cmdService, subRemove, argPurge, flagUser, "mic", flagConfig, inst.ConfigPath, "--state-dir", inst.StateDir, "--bin-path", inst.BinPath)
	if code != 0 || purged != inst {
		t.Fatalf("purge with the values given: exit %d spec %+v stderr %q, want %+v", code, purged, errOut, inst)
	}
	if !strings.Contains(errOut, "purging config directory /srv/rm, state directory /srv/rm-state, binary /opt/bin/remote-mic, and user mic") {
		t.Errorf("purge stderr %q, want the paths it removes", errOut)
	}

	defaults := service.ServiceSpec{User: service.DefaultUser, ConfigPath: service.DefaultConfigPath, StateDir: service.DefaultStateDir, BinPath: service.DefaultBinPath}
	installedSpec = func() (service.ServiceSpec, error) { return defaults, nil }
	if code, _, errOut := runCLI(cmdService, subRemove, argPurge); code != 0 || purged != defaults {
		t.Errorf("purge of a default install: exit %d spec %+v stderr %q, want %+v", code, purged, errOut, defaults)
	}
	installedSpec = func() (service.ServiceSpec, error) { return service.ServiceSpec{}, nil }
	if code, _, errOut := runCLI(cmdService, subRemove, argPurge); code != 0 || !strings.Contains(errOut, "no installed remote-mic.service; unset flags take the package defaults") {
		t.Errorf("purge with no unit: exit %d stderr %q, want a note that defaults are used", code, errOut)
	}

	reads = 0
	code, _, errOut = runCLI(cmdService, subRemove)
	if code != 0 || reads != 0 || purged != (service.ServiceSpec{User: service.DefaultUser, ConfigPath: service.DefaultConfigPath, StateDir: service.DefaultStateDir, BinPath: service.DefaultBinPath}) {
		t.Errorf("plain uninstall: exit %d, %d unit reads, spec %+v, stderr %q; want the package defaults and no read", code, reads, purged, errOut)
	}
}

// TestServiceDefaultsRefuseHandEditedUnit asserts a unit that is not exactly
// what the installer wrote never supplies what purge deletes or install
// chowns: the command asks for the flags instead, and runs once they are
// all given.
func TestServiceDefaultsRefuseHandEditedUnit(t *testing.T) {
	saveServiceSeams(t)
	geteuid = func() int { return 0 }
	prev := installedSpec
	installedSpec = func() (service.ServiceSpec, error) {
		return service.ServiceSpec{}, errors.New("/etc/systemd/system/remote-mic.service is not the unit the installer writes")
	}
	t.Cleanup(func() { installedSpec = prev })
	called := false
	uninstallService = func(service.ServiceSpec, bool) error { called = true; return nil }
	installService = func(service.ServiceSpec, bool, bool) error { called = true; return nil }

	for _, args := range [][]string{{cmdService, subRemove, argPurge}, {cmdService, cmdInstall}} {
		code, _, errOut := runCLI(args...)
		if code != 1 || called || !strings.Contains(errOut, "pass --user, --config, --state-dir and --bin-path") {
			t.Errorf("%v: exit %d called %t stderr %q, want a refusal before any change", args, code, called, errOut)
		}
	}
	all := []string{cmdService, subRemove, argPurge, flagUser, "mic", flagConfig, "/srv/rm/config.yaml", "--state-dir", "/srv/s", "--bin-path", "/opt/rm"}
	if code, _, errOut := runCLI(all...); code != 0 || !called {
		t.Errorf("all flags given: exit %d called %t stderr %q, want it to run", code, called, errOut)
	}
}

// TestResolveConfigUnitError asserts an installed unit whose config cannot be
// known stops every command that has no --config or $REMOTEMIC_CONFIG,
// instead of falling back to ./config.yaml, and is ignored when either names
// the file.
func TestResolveConfigUnitError(t *testing.T) {
	prev := installedConfig
	installedConfig = func() (string, string, error) { return "", "", errors.New("ExecStart= is not the installer's") }
	t.Cleanup(func() { installedConfig = prev })
	t.Setenv(configEnv, "")
	t.Chdir(t.TempDir())
	seedConfigWithToken(t, configDefault, tokenOld)
	want := "cannot tell which config " + service.DefaultUnitName + " uses"

	for _, args := range [][]string{
		{cmdToken, subGet}, {cmdToken, subGenerate, argForce}, {cmdToken, subSet}, {cmdToken, "clear", "-yes"}, {cmdServe, argCheck},
	} {
		code, out, errOut := runCLI(args...)
		if code != 1 || out != "" || !strings.Contains(errOut, want) {
			t.Errorf("%v: exit %d stdout %q stderr %q, want exit 1 and %q", args, code, out, errOut, want)
		}
	}
	if got := loadToken(t, configDefault); got != tokenOld {
		t.Errorf("./config.yaml token %q, want it untouched (%q)", got, tokenOld)
	}
	if code, out, errOut := runCLI("token", "get", flagConfig, configDefault); code != 0 || out != tokenOld+"\n" {
		t.Errorf("--config given: exit %d stdout %q stderr %q, want the token", code, out, errOut)
	}
	t.Setenv(configEnv, configDefault)
	if code, out, errOut := runCLI("token", "get"); code != 0 || out != tokenOld+"\n" {
		t.Errorf("env given: exit %d stdout %q stderr %q, want the token", code, out, errOut)
	}
}

// stubRunAppliance replaces the appliance start with a recorder, so a test of
// serve's checks never starts it. It returns whether the start was reached.
func stubRunAppliance(t *testing.T) *bool {
	t.Helper()
	prev := runAppliance
	started := new(bool)
	runAppliance = func(string, serveOverrides, bool, string) error { *started = true; return nil }
	t.Cleanup(func() { runAppliance = prev })
	return started
}

// TestServeRefusesAnotherAccount asserts serve run by hand as an account
// other than the config's owner is refused before anything starts, however
// the config was named, while --check (read only) and a start by systemd
// (where a drop-in may set another User=) go ahead.
func TestServeRefusesAnotherAccount(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubEUID(t, os.Geteuid()+1)
	t.Setenv(configEnv, "")

	started := stubRunAppliance(t)
	code, _, errOut := runCLI(cmdServe, flagConfig, path)
	if code != 1 || *started || !strings.Contains(errOut, "run this command as that account") || !strings.Contains(errOut, "remote-mic serve --config "+path) {
		t.Errorf("serve --config as another account: exit %d started %t stderr %q, want a refusal naming the command", code, *started, errOut)
	}

	started = stubRunAppliance(t)
	if code, _, errOut := runCLI(cmdServe, flagConfig, path, argCheck); code != 0 || !*started {
		t.Errorf("serve --check: exit %d started %t stderr %q, want it to run", code, *started, errOut)
	}

	started = stubRunAppliance(t)
	t.Setenv(configEnv, path)
	if code, _, errOut := runCLI(cmdServe); code != 1 || *started || !strings.Contains(errOut, "run this command as that account") {
		t.Errorf("serve by hand from $%s: exit %d started %t stderr %q, want the owner refusal", configEnv, code, *started, errOut)
	}
	t.Setenv("INVOCATION_ID", "0123456789abcdef")
	if code, _, errOut := runCLI(cmdServe); code != 0 || !*started {
		t.Errorf("serve started by systemd: exit %d started %t stderr %q, want it to run", code, *started, errOut)
	}
	t.Setenv("INVOCATION_ID", "")

	stubEUID(t, os.Geteuid())
	started = stubRunAppliance(t)
	t.Setenv(configEnv, "")
	if code, _, errOut := runCLI(cmdServe, flagConfig, path); code != 0 || !*started {
		t.Errorf("serve as the owner: exit %d started %t stderr %q, want it to run", code, *started, errOut)
	}
}

// TestConfigFlagEmptyRefused asserts an explicitly empty --config is a usage
// error, not a fall through to the installed appliance's config (a script
// passing an unset variable would otherwise rotate the live token).
func TestConfigFlagEmptyRefused(t *testing.T) {
	unitCfg := installedFixture(t)
	for _, args := range [][]string{
		{cmdToken, subGet, "--config="}, {cmdToken, subGenerate, argForce, flagConfig, ""}, {cmdServe, argCheck, "--config="},
	} {
		code, out, errOut := runCLI(args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "--config is empty") {
			t.Errorf("%v: exit %d stdout %q stderr %q, want exit 2 and a usage error", args, code, out, errOut)
		}
	}
	if got := loadToken(t, unitCfg); got != tokenOld {
		t.Errorf("installed config token %q, want it untouched (%q)", got, tokenOld)
	}
}

// TestServeStartedBySystemdKeepsWorkingDirectory asserts a serve that systemd
// started with no --config and no $REMOTEMIC_CONFIG (a unit someone wrote)
// reads config.yaml in its working directory, even when the installed unit
// cannot be read or names another config, and is not owner-checked.
func TestServeStartedBySystemdKeepsWorkingDirectory(t *testing.T) {
	prev := installedConfig
	installedConfig = func() (string, string, error) { return "", "", errors.New("not the unit the installer writes") }
	t.Cleanup(func() { installedConfig = prev })
	t.Setenv(configEnv, "")
	t.Setenv("INVOCATION_ID", "0123456789abcdef")
	t.Chdir(t.TempDir())
	stubEUID(t, os.Geteuid()+1)
	var gotPath string
	prevRun := runAppliance
	runAppliance = func(p string, _ serveOverrides, _ bool, _ string) error { gotPath = p; return nil }
	t.Cleanup(func() { runAppliance = prevRun })

	if code, _, errOut := runCLI(cmdServe); code != 0 || gotPath != configDefault {
		t.Errorf("serve under systemd: exit %d path %q stderr %q, want %q", code, gotPath, errOut, configDefault)
	}
	t.Setenv("INVOCATION_ID", "")
	if code, _, errOut := runCLI(cmdServe); code != 1 || !strings.Contains(errOut, "cannot tell which config") {
		t.Errorf("serve by hand: exit %d stderr %q, want the unit error", code, errOut)
	}
}
