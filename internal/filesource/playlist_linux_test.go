package filesource

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
)

// A FIFO with no writer blocks os.Open for good, so a directory pass must
// stat every entry first and skip what is not a regular file.
func TestDirectorySkipsNonRegularEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	aPCM := ramp(periodFrames*2, 2)
	writeFile(t, dir, "a.flac", flacBytes(t, testRate, 2, 16, aPCM))
	if err := syscall.Mkfifo(filepath.Join(dir, "x.flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A listed entry that no longer resolves is skipped without a log line.
	if err := os.Symlink(filepath.Join(dir, "missing.flac"), filepath.Join(dir, "dangling.flac")); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(dir, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		if got, want := readN(t, s, 6), bytes.Repeat(aPCM, 3); !bytes.Equal(got, want) {
			t.Fatal("passes did not replay a.flac around the FIFO")
		}
		if lines := rec.all(); len(lines) != 1 || !strings.Contains(lines[0], "x.flac") {
			t.Fatalf("logged %q, want one line naming x.flac", lines)
		}
	})
}

// Open must refuse a path that is a FIFO without opening it, which would block
// with no way for Close to interrupt it.
func TestOpenRejectsFifoPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.flac")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	var rec recorder
	s, err := Open(cfgFor(fifo, &rec))
	if err == nil {
		closeQuiet(s)
		t.Fatal("Open succeeded on a FIFO")
	}
	if !errors.Is(err, errNotPlayable) {
		t.Fatalf("Open = %v, want errNotPlayable", err)
	}
	if containsPath(err, dir) {
		t.Fatalf("error text carries a path: %v", err)
	}
	if lines := rec.all(); len(lines) != 0 {
		t.Fatalf("failed Open logged %q, want nothing", lines)
	}
}
