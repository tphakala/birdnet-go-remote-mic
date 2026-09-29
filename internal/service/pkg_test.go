//go:build linux

package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileListHas(t *testing.T) {
	t.Parallel()
	list := filepath.Join(t.TempDir(), "birdnet-go-remote-mic.list")
	if err := os.WriteFile(list, []byte("/.\n/usr\n/usr/bin\n/usr/bin/remote-mic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileListHas(list, "/usr/bin/remote-mic") {
		t.Error("listed path not found")
	}
	if fileListHas(list, "/usr/local/bin/remote-mic") || fileListHas(list, "/usr/bin/remote") {
		t.Error("unlisted path found")
	}
	if fileListHas(filepath.Join(t.TempDir(), "missing.list"), "/usr/bin/remote-mic") {
		t.Error("missing list reported a path")
	}
}

func TestPackageOwnsIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "birdnet-go-remote-mic.list"), []byte("/usr/bin/remote-mic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	glob := filepath.Join(dir, "birdnet-go-remote-mic*.list")
	if !packageOwnsIn(glob, "/usr/bin/remote-mic") {
		t.Error("a listed path is not owned")
	}
	if packageOwnsIn(glob, "/usr/local/bin/remote-mic") || packageOwnsIn(filepath.Join(dir, "none*.list"), "/usr/bin/remote-mic") {
		t.Error("an unlisted path, or no file list, reported ownership")
	}
}
