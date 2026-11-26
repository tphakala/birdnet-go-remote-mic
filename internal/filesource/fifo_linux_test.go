package filesource

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
)

// A path replaced by a FIFO with no writer would block os.Open for good, and
// Close cannot interrupt an open that has not installed its file yet.
func TestReadSkipsFifoReplacement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeFile(t, dir, "a.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames+40, 2)))
	fifo := filepath.Join(dir, "swap.tmp")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		readN(t, s, 1)
		if err := os.Rename(fifo, path); err != nil {
			t.Fatal(err)
		}
		_, err = s.Read()
		na, ok := errors.AsType[*NoAudioError](err)
		if !ok || len(na.Skipped) != 1 || na.Skipped[0].Class != SkipUnreadable {
			t.Fatalf("Read = %v, want *NoAudioError with one unreadable skip", err)
		}
	})
}
