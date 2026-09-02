package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadCapturingLog runs Load(path) with the standard logger redirected to a
// buffer and returns whatever it wrote, so a permission warning can be asserted.
func loadCapturingLog(t *testing.T, path string) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	if _, err := Load(path); err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	return buf.String()
}

func savedTokenConfig(t *testing.T, token string) string {
	t.Helper()
	c := validBase()
	c.Auth.Token = token
	c.ApplyDefaults()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, &c); err != nil { // Save writes 0600
		t.Fatalf("Save: %v", err)
	}
	return path
}

func TestLoadWarnsOnReadableTokenFile(t *testing.T) {
	path := savedTokenConfig(t, "k7Qm3vX9pL2wR8nT")
	if err := os.Chmod(path, 0o644); err != nil { // widen so group/other can read the token
		t.Fatalf("chmod: %v", err)
	}
	out := loadCapturingLog(t, path)
	if !strings.Contains(out, "accessible") || !strings.Contains(out, "token") {
		t.Errorf("Load did not warn about a group/other-accessible token file; log = %q", out)
	}
}

func TestLoadQuietOnOwnerOnlyTokenFile(t *testing.T) {
	path := savedTokenConfig(t, "k7Qm3vX9pL2wR8nT") // stays 0600
	if out := loadCapturingLog(t, path); out != "" {
		t.Errorf("Load warned on a 0600 token file; log = %q", out)
	}
}

func TestLoadQuietWhenNoTokenEvenIfReadable(t *testing.T) {
	path := savedTokenConfig(t, "") // open access, no secret to leak
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if out := loadCapturingLog(t, path); out != "" {
		t.Errorf("Load warned on an open-access (no token) file; log = %q", out)
	}
}
