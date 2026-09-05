package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadOrDefaultMissingFile asserts a missing config file yields Default()
// with no error, so a first-run serve or init boots without a config present.
func TestLoadOrDefaultMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	got, err := LoadOrDefault(path)
	if err != nil {
		t.Fatalf("LoadOrDefault(missing) error = %v, want nil", err)
	}
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("LoadOrDefault(missing) = %+v, want Default() %+v", got, want)
	}
}

// TestLoadOrDefaultExistingFile asserts an existing valid file is loaded as-is,
// not replaced by defaults.
func TestLoadOrDefaultExistingFile(t *testing.T) {
	t.Parallel()
	c := validBase()
	c.ApplyDefaults()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, &c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadOrDefault(path)
	if err != nil {
		t.Fatalf("LoadOrDefault(existing) error = %v", err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("LoadOrDefault(existing) = %+v, want %+v", got, c)
	}
}

// TestLoadOrDefaultParseErrorNotSwallowed asserts a real load error (malformed
// YAML) is returned, not masked as Default(): only os.ErrNotExist falls back.
func TestLoadOrDefaultParseErrorNotSwallowed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: [unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrDefault(path)
	if err == nil {
		t.Fatal("LoadOrDefault(malformed) error = nil, want a parse error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadOrDefault(malformed) returned ErrNotExist, want a parse error: %v", err)
	}
}
