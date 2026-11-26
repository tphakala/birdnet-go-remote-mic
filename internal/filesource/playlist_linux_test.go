package filesource

import (
	"bytes"
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
