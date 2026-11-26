package filesource

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/tphakala/go-opus/oggopus"
)

func TestDirectoryPlaysInNameOrderAndRescans(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	aPCM, bPCM := ramp(periodFrames*2, 2), ramp(periodFrames*3, 2)
	writeFile(t, dir, "b.flac", flacBytes(t, testRate, 2, 16, bPCM))
	a := writeFile(t, dir, "a.flac", flacBytes(t, testRate, 2, 16, aPCM))
	writeFile(t, dir, "notes.txt", []byte("not audio"))
	zPCM := noise(periodFrames, 2)
	for _, sub := range []string{"sub", "sub.flac"} { // a directory is never played, even one named like a file
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, sub), "z.flac", flacBytes(t, testRate, 2, 16, zPCM))
	}
	cEnc := opusBytes(t, testRate, 2, ramp(periodFrames*2, 2))
	cPCM, _, err := oggopus.DecodeInterleaved(bytes.NewReader(cEnc))
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(dir, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		if got, want := readN(t, s, 5), append(bytes.Clone(aPCM), bPCM...); !bytes.Equal(got, want) {
			t.Fatal("pass 1 is not a.flac then b.flac")
		}
		writeFile(t, dir, "c.opus", cEnc)
		if err := os.Remove(a); err != nil {
			t.Fatal(err)
		}
		n := len(bPCM) + len(cPCM)
		got := readN(t, s, n/(periodFrames*4)+1)
		if want := append(bytes.Clone(bPCM), cPCM...); !bytes.Equal(got[:n], want) {
			t.Fatal("pass 2 is not b.flac then c.opus")
		}
		for _, name := range []string{"b.flac", "c.opus"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		// The descriptors already open play out; the next listing is empty.
		var na *NoAudioError
		for range 20 {
			if _, err = s.Read(); err != nil {
				break
			}
		}
		if !errors.As(err, &na) || len(na.Skipped) != 0 {
			t.Fatalf("Read on an emptied directory = %v, want *NoAudioError with no skips", err)
		}
		if lines := rec.all(); len(lines) != 0 {
			t.Fatalf("logged %q, want nothing for files that played or were ignored", lines)
		}
	})
}

func TestDirectorySkipsMismatchesAndLogsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a_stereo.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames*2, 2)))
	writeFile(t, dir, "b_rate.flac", flacBytes(t, 44100, 2, 16, ramp(periodFrames, 2)))
	writeFile(t, dir, "c_mono.flac", flacBytes(t, testRate, 1, 16, ramp(periodFrames, 1)))
	writeFile(t, dir, "d_bad.flac", []byte("this is not a flac stream at all, just text"))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(dir, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		readN(t, s, 8) // four passes of two periods
		lines := rec.all()
		if len(lines) != 3 {
			t.Fatalf("logged %q over several passes, want one line per skipped file", lines)
		}
		for i, name := range []string{"b_rate.flac", "c_mono.flac", "d_bad.flac"} {
			if !strings.Contains(lines[i], name) {
				t.Fatalf("line %d = %q, want it to name %s", i, lines[i], name)
			}
		}

		// The same file at another rate is a new reason, so it is logged again.
		writeFile(t, dir, "c_mono.flac", flacBytes(t, 44100, 2, 16, ramp(periodFrames, 2)))
		readN(t, s, 8)
		lines = rec.all()
		if len(lines) != 4 || !strings.Contains(lines[3], "c_mono.flac") || !strings.Contains(lines[3], "44100") {
			t.Fatalf("logged %q, want a fourth line for the rate mismatch", lines)
		}

		// A mono file replaced by a stereo one plays from the next pass, and a
		// clean pass makes the file forget its reason: the same mismatch is
		// reported again when it returns.
		writeFile(t, dir, "c_mono.flac", flacBytes(t, testRate, 2, 16, noise(periodFrames, 2)))
		if got := readN(t, s, 8); !bytes.Contains(got, noise(periodFrames, 2)) {
			t.Fatal("the replaced file never played")
		}
		if n := len(rec.all()); n != 4 {
			t.Fatalf("%d lines after the file started to play, want 4", n)
		}
		writeFile(t, dir, "c_mono.flac", flacBytes(t, 44100, 2, 16, ramp(periodFrames, 2)))
		readN(t, s, 8)
		lines = rec.all()
		if len(lines) != 5 || !strings.Contains(lines[4], "44100") {
			t.Fatalf("logged %q, want the rate mismatch reported again after a clean pass", lines)
		}

		// A file absent for a pass is forgotten, so it is reported again on return.
		bad := filepath.Join(dir, "d_bad.flac")
		if err := os.Rename(bad, bad+".gone"); err != nil {
			t.Fatal(err)
		}
		readN(t, s, 8)
		if err := os.Rename(bad+".gone", bad); err != nil {
			t.Fatal(err)
		}
		readN(t, s, 8)
		lines = rec.all()
		if len(lines) != 6 || !strings.Contains(lines[5], "d_bad.flac") {
			t.Fatalf("logged %q, want d_bad.flac reported again after it returned", lines)
		}
	})
}

func TestOpenSkipsToFirstPlayable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a_empty.flac", flacBytes(t, testRate, 1, 16, nil))
	writeFile(t, dir, "b_rate.flac", flacBytes(t, 44100, 2, 16, ramp(periodFrames, 2)))
	cPCM := ramp(periodFrames*2, 2)
	c := writeFile(t, dir, "c_stereo.flac", flacBytes(t, testRate, 2, 16, cPCM))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(dir, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		if rate, ch := s.Negotiated(); rate != testRate || ch != 2 {
			t.Fatalf("Negotiated = (%d, %d), want (%d, 2): the empty mono file must not fix the channels", rate, ch, testRate)
		}
		if got := readN(t, s, 2); !bytes.Equal(got, cPCM) {
			t.Fatal("the first period is not the first playable file")
		}
		lines := rec.all()
		if len(lines) != 2 || !strings.Contains(lines[0], "a_empty.flac") || !strings.Contains(lines[1], "b_rate.flac") {
			t.Fatalf("logged %q, want one line each for a_empty.flac and b_rate.flac", lines)
		}

		// With nothing playable, Open lists every skip and logs nothing.
		if err := os.Remove(c); err != nil {
			t.Fatal(err)
		}
		var rec2 recorder
		s2, err := Open(cfgFor(dir, &rec2))
		if err == nil {
			closeQuiet(s2)
			t.Fatal("Open succeeded with no playable file")
		}
		var na *NoAudioError
		if !errors.As(err, &na) || len(na.Skipped) != 2 ||
			na.Skipped[0].Class != SkipUndecodable || na.Skipped[1].Class != SkipRate {
			t.Fatalf("err = %v (%+v), want *NoAudioError with an undecodable and a rate skip", err, na)
		}
		if containsPath(err, dir) {
			t.Fatalf("error text carries a path: %v", err)
		}
		if lines := rec2.all(); len(lines) != 0 {
			t.Fatalf("failed Open logged %q, want nothing", lines)
		}
	})
}
