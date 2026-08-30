package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesAndOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := Write(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("content = %q, want first", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
	}
	// Overwrite atomically.
	if err := Write(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "second" {
		t.Errorf("content after overwrite = %q, want second", got)
	}
	// No temp files must be left behind in the directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1 (no leftover temp files)", len(entries))
	}
}

func TestWritePreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	if err := Write(realPath, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(realPath, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := Write(link, []byte("v2"), 0o600); err != nil {
		t.Fatalf("write via link: %v", err)
	}
	// The link must survive as a symlink and the real file carry the update.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("Write replaced the symlink with a regular file")
	}
	got, _ := os.ReadFile(realPath)
	if string(got) != "v2" {
		t.Errorf("real file = %q, want v2", got)
	}
}
