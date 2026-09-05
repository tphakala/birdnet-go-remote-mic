//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// TestInitGeneratesTokenIntoNewConfig asserts init on a missing config creates
// it, writes a generated token, and prints that same token to stdout.
func TestInitGeneratesTokenIntoNewConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path}, &out, &errb); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	token := strings.TrimSpace(out.String())
	if token == "" {
		t.Fatal("no token printed to stdout")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after init: %v", err)
	}
	if cfg.Auth.Token != token {
		t.Fatalf("config token %q != printed token %q", cfg.Auth.Token, token)
	}
}

// TestInitQuietPrintsOnlyToken asserts --quiet emits the bare token on stdout
// and nothing on stderr, so `TOKEN=$(... init --quiet)` works.
func TestInitQuietPrintsOnlyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path, "-quiet"}, &out, &errb); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if errb.Len() != 0 {
		t.Fatalf("quiet mode wrote to stderr: %q", errb.String())
	}
	token := strings.TrimSpace(out.String())
	if msg := auth.ValidToken(token); msg != "" {
		t.Fatalf("printed token invalid: %s", msg)
	}
	if out.String() != token+"\n" {
		t.Fatalf("quiet stdout is not the bare token: %q", out.String())
	}
}

// TestInitRefusesExistingTokenWithoutForce asserts init will not silently
// overwrite a token, which would lock out connected clients.
func TestInitRefusesExistingTokenWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seedConfigWithToken(t, path, "existingtoken123")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path}, &out, &errb); err == nil {
		t.Fatal("expected refusal when a token already exists")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Auth.Token != "existingtoken123" {
		t.Fatalf("existing token was modified: %q", loaded.Auth.Token)
	}
}

// TestInitForceReplacesToken asserts --force rotates an existing token.
func TestInitForceReplacesToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seedConfigWithToken(t, path, "oldtoken12345")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path, "-force"}, &out, &errb); err != nil {
		t.Fatalf("runInit --force: %v", err)
	}
	newTok := strings.TrimSpace(out.String())
	if newTok == "oldtoken12345" {
		t.Fatal("--force did not change the token")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Auth.Token != newTok {
		t.Fatalf("config token %q != printed token %q", loaded.Auth.Token, newTok)
	}
}

// TestInitTokenFlagSetsChosenToken asserts --token seeds a caller-supplied
// token instead of generating one.
func TestInitTokenFlagSetsChosenToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path, "-token", "chosentoken123"}, &out, &errb); err != nil {
		t.Fatalf("runInit -token: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Auth.Token != "chosentoken123" {
		t.Fatalf("chosen token not saved: %q", loaded.Auth.Token)
	}
}

// TestInitTokenFlagRejectsInvalid asserts an invalid --token is rejected and no
// config file is written.
func TestInitTokenFlagRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path, "-token", "short"}, &out, &errb); err == nil {
		t.Fatal("expected invalid --token to be rejected")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config must not be written on invalid token (stat err = %v)", err)
	}
}

// TestInitPreservesExistingDevices asserts seeding a token leaves an existing
// device list intact.
func TestInitPreservesExistingDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		Name: "m1", Device: "hw:1,0", Path: "/m1",
		Mode: config.ModeOpus, Rate: 48000, Channels: []int{1}, Format: testFmtS16,
	}}
	if err := config.Save(path, &cfg); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path}, &out, &errb); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Devices) != 1 || loaded.Devices[0].Name != "m1" {
		t.Fatalf("devices not preserved: %+v", loaded.Devices)
	}
	if loaded.Auth.Token == "" {
		t.Fatal("token not set")
	}
}

// TestInitRejectsPositional asserts a stray positional argument errors instead
// of being silently ignored (e.g. `init secret123` must not quietly generate a
// random token while discarding the intended value).
func TestInitRejectsPositional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errb bytes.Buffer
	if err := runInit([]string{flagConfig, path, "stray"}, &out, &errb); err == nil {
		t.Fatal("expected an error on a stray positional argument")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no config should be written when args are rejected (stat err = %v)", err)
	}
}

func seedConfigWithToken(t *testing.T, path, token string) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Token = token
	if err := config.Save(path, &cfg); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
}
