//go:build linux

package main

import (
	"bytes"
	"errors"
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
			ref, err := resolveConfig(tc.flag, &bytes.Buffer{})
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
	cfgPath, _, _, _, err := parseServeFlags([]string{"--check"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfgPath != unitCfg {
		t.Errorf("cfgPath = %q, want %q", cfgPath, unitCfg)
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

// TestInstalledConfigOr asserts service install and uninstall default --config
// to the installed unit's config, then to the package default.
func TestInstalledConfigOr(t *testing.T) {
	stubInstalledConfig(t, "/srv/rm/config.yaml", "mic")
	if got := installedConfigOr(""); got != "/srv/rm/config.yaml" {
		t.Errorf("with a unit: got %q", got)
	}
	if got := installedConfigOr(cfgPathX); got != cfgPathX {
		t.Errorf("explicit flag: got %q", got)
	}
	stubInstalledConfig(t, "", "")
	if got := installedConfigOr(""); got != service.DefaultConfigPath {
		t.Errorf("without a unit: got %q, want %q", got, service.DefaultConfigPath)
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
		{cmdToken, subGet}, {cmdToken, subGenerate, argForce}, {cmdToken, subSet}, {cmdToken, "clear", "-yes"}, {"serve", "--check"},
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
