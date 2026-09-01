package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validAuthToken = "k7Qm3vX9pL2wR8nT"

func authTestConfig(token string) Config {
	c := Default()
	c.Auth.Token = token
	return c
}

func TestValidateAuthToken(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		reason string // "" means valid
	}{
		{"empty token is open access", "", ""},
		{"short token rejected", "short", "12"},
		{"twelve chars accepted", "abcdefghijkl", ""},
		{"space rejected", "abcdef ghijkl", "letters"},
		{"slash rejected", "abcdef/ghijkl", "letters"},
		{"quote rejected", `abcdef"ghijkl`, "letters"},
		{"too long rejected", strings.Repeat("x", 129), "128"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := authTestConfig(tt.token)
			err := c.Validate()
			if tt.reason == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error %v", err)
				}
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate returned %v, want *ValidationError", err)
			}
			if verr.Field != "auth.token" {
				t.Errorf("field = %q, want auth.token", verr.Field)
			}
			if !strings.Contains(verr.Reason, tt.reason) {
				t.Errorf("reason %q does not mention %q", verr.Reason, tt.reason)
			}
		})
	}
}

func TestAuthRequired(t *testing.T) {
	c := authTestConfig("")
	if c.AuthRequired() {
		t.Error("empty token must report open access")
	}
	c.Auth.Token = validAuthToken
	if !c.AuthRequired() {
		t.Error("a set token must report auth required")
	}
}

func TestCloneCopiesAuth(t *testing.T) {
	c := authTestConfig(validAuthToken)
	out := c.Clone()
	if out.Auth.Token != validAuthToken {
		t.Errorf("clone token = %q, want %q", out.Auth.Token, validAuthToken)
	}
}

func TestSaveLoadRoundTripsAuthToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := authTestConfig(validAuthToken)
	if err := Save(path, &c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "token: "+validAuthToken) {
		t.Errorf("saved YAML lacks the token:\n%s", data)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.AuthRequired() || got.Auth.Token != validAuthToken {
		t.Errorf("loaded token = %q, want %q", got.Auth.Token, validAuthToken)
	}
}

func TestSaveOmitsEmptyAuthBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := authTestConfig("")
	if err := Save(path, &c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "auth:") {
		t.Errorf("an empty token should not write an auth block:\n%s", data)
	}
}

func TestLoadRejectsInvalidAuthToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: \":8554\"\nauth:\n  token: short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "auth.token" {
		t.Fatalf("Load returned %v, want an auth.token validation error", err)
	}
}
