package filesource

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	flacpcm "github.com/tphakala/go-flac/pcm"
	"github.com/tphakala/go-opus/oggopus"
)

const testRate = 48000

// period is one 20 ms period at testRate.
const (
	periodFrames = testRate / 50
	periodDur    = 20 * time.Millisecond
)

func cfgFor(path string, rec *recorder) Config {
	return Config{Path: path, Rate: testRate, MinChannels: 1, MaxChannels: 8, Logf: rec.logf}
}

func TestReadLoopsFileGaplessly(t *testing.T) {
	t.Parallel()
	frames := periodFrames*5/2 + 7 // 2.5 periods plus a few frames: the wrap lands mid-period
	s16 := ramp(frames, 2)
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 24, s16To24(s16)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		if rate, ch := s.Negotiated(); rate != testRate || ch != 2 {
			t.Fatalf("Negotiated = (%d, %d), want (%d, 2)", rate, ch, testRate)
		}
		const periods = 7
		got := readN(t, s, periods)
		want := bytes.Repeat(s16, periods*periodFrames*4/len(s16)+1)[:periods*periodFrames*4]
		if !bytes.Equal(got, want) {
			t.Fatalf("looped output differs from the repeated source (len got %d, want %d)", len(got), len(want))
		}
	})
}

func TestOpenRejectsUnplayableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stereo := ramp(periodFrames*2, 2)
	mono := ramp(periodFrames*2, 1)
	files := map[string]string{
		"rate.flac":    writeFile(t, dir, "rate.flac", flacBytes(t, 44100, 2, 16, stereo)),
		"mono.flac":    writeFile(t, dir, "mono.flac", flacBytes(t, testRate, 1, 16, mono)),
		"stereo.flac":  writeFile(t, dir, "stereo.flac", flacBytes(t, testRate, 2, 16, stereo)),
		"garbage.flac": writeFile(t, dir, "garbage.flac", []byte("this is not a flac stream at all, just text")),
		"empty.flac":   writeFile(t, dir, "empty.flac", flacBytes(t, testRate, 2, 16, nil)),
		"a.opus":       writeFile(t, dir, "a.opus", opusBytes(t, 48000, 2, stereo)),
		"garbage.opus": writeFile(t, dir, "garbage.opus", []byte("this is not an ogg stream at all, just text")),
		"notes.txt":    writeFile(t, dir, "notes.txt", []byte("hello")),
	}
	tests := []struct {
		name    string
		path    string
		rate    int
		minCh   int
		maxCh   int
		class   SkipClass
		wantErr error // a plain error instead of *NoAudioError
		anyErr  bool
	}{
		{name: "rate mismatch", path: files["rate.flac"], rate: testRate, minCh: 1, maxCh: 8, class: SkipRate},
		{name: "fewer channels than min", path: files["mono.flac"], rate: testRate, minCh: 2, maxCh: 8, class: SkipChannels},
		{name: "more channels than max", path: files["stereo.flac"], rate: testRate, minCh: 1, maxCh: 1, class: SkipChannels},
		{name: "garbage flac", path: files["garbage.flac"], rate: testRate, minCh: 1, maxCh: 8, class: SkipUndecodable},
		{name: "header-valid flac with no audio", path: files["empty.flac"], rate: testRate, minCh: 1, maxCh: 8, class: SkipUndecodable},
		{name: "opus at 44.1 kHz device rate", path: files["a.opus"], rate: 44100, minCh: 1, maxCh: 8, class: SkipRate},
		{name: "garbage opus", path: files["garbage.opus"], rate: testRate, minCh: 1, maxCh: 8, class: SkipUndecodable},
		{name: "unsupported extension", path: files["notes.txt"], rate: testRate, minCh: 1, maxCh: 8, class: SkipUnsupported},
		{name: "missing path", path: filepath.Join(dir, "missing.flac"), rate: testRate, minCh: 1, maxCh: 8, wantErr: fs.ErrNotExist},
		{name: "directory", path: dir, rate: testRate, minCh: 1, maxCh: 8, anyErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec recorder
			s, err := Open(Config{Path: tc.path, Rate: tc.rate, MinChannels: tc.minCh, MaxChannels: tc.maxCh, Logf: rec.logf})
			if err == nil {
				closeQuiet(s)
				t.Fatal("Open succeeded, want an error")
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("Open returned io.EOF: %v", err)
			}
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.anyErr:
			default:
				var na *NoAudioError
				if !errors.As(err, &na) {
					t.Fatalf("err = %T %v, want *NoAudioError", err, err)
				}
				if len(na.Skipped) != 1 || na.Skipped[0].Class != tc.class {
					t.Fatalf("skipped = %+v, want one skip of class %v", na.Skipped, tc.class)
				}
			}
			if containsPath(err, dir) {
				t.Fatalf("error text carries a path: %v", err)
			}
			if lines := rec.all(); len(lines) != 0 {
				t.Fatalf("failed Open logged %q, want nothing", lines)
			}
		})
	}
}

func TestReadFailsWhenLoopFindsNoAudio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeFile(t, dir, "a.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames+40, 2)))
	mono := writeFile(t, dir, "new.tmp", flacBytes(t, testRate, 1, 16, ramp(periodFrames*3, 1)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		readN(t, s, 1)
		if err := os.Rename(mono, path); err != nil {
			t.Fatal(err)
		}
		// The open descriptor still plays the 40 remaining frames, then the pass
		// ends and the replacement (mono) is skipped.
		_, err = s.Read()
		var na *NoAudioError
		if !errors.As(err, &na) {
			t.Fatalf("Read = %v, want *NoAudioError", err)
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("Read returned io.EOF-like error: %v", err)
		}
		if len(na.Skipped) != 1 || na.Skipped[0].Class != SkipChannels {
			t.Fatalf("skipped = %+v, want one channel-count skip", na.Skipped)
		}
		if lines := rec.all(); len(lines) != 1 || !strings.Contains(lines[0], "1 channels") {
			t.Fatalf("logged %q, want one line naming the channel mismatch", lines)
		}
	})
}

func TestReadLogsDecodeErrorOnce(t *testing.T) {
	t.Parallel()
	const frames = 4096 * 6
	full := flacBytes(t, testRate, 2, 16, noise(frames, 2))
	cut := full[:len(full)*2/3]
	prefix, err := io.ReadAll(mustFlacReader(t, cut))
	if err == nil || len(prefix) == 0 || len(prefix)%4 != 0 {
		t.Fatalf("truncated fixture decodes to %d bytes, err %v; want whole frames and an error", len(prefix), err)
	}
	path := writeFile(t, t.TempDir(), "a.flac", cut)
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		periodBytes := periodFrames * 4
		n := 3*len(prefix)/periodBytes + 2
		got := readN(t, s, n)
		want := bytes.Repeat(prefix, n*periodBytes/len(prefix)+1)[:n*periodBytes]
		if !bytes.Equal(got, want) {
			t.Fatal("output is not the decodable prefix repeated once per pass")
		}
		if lines := rec.all(); len(lines) != 1 {
			t.Fatalf("logged %d lines %q over three passes, want 1", len(lines), lines)
		}
	})
}

func TestReadSurvivesPanickingLogger(t *testing.T) {
	t.Parallel()
	const frames = 4096 * 6
	full := flacBytes(t, testRate, 2, 16, noise(frames, 2))
	cut := full[:len(full)*2/3]
	prefix, err := io.ReadAll(mustFlacReader(t, cut))
	if err == nil || len(prefix) == 0 {
		t.Fatalf("truncated fixture decodes to %d bytes, err %v; want audio and an error", len(prefix), err)
	}
	path := writeFile(t, t.TempDir(), "a.flac", cut)
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		cfg := Config{Path: path, Rate: testRate, MinChannels: 1, MaxChannels: 8, Logf: func(string, ...any) {
			calls++
			panic("logger failed")
		}}
		s, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		periodBytes := periodFrames * 4
		n := 2*len(prefix)/periodBytes + 2
		got := readN(t, s, n)
		want := bytes.Repeat(prefix, n*periodBytes/len(prefix)+1)[:n*periodBytes]
		if !bytes.Equal(got, want) {
			t.Fatal("output after a panicking logger is not the decodable prefix repeated once per pass")
		}
		if calls != 1 {
			t.Fatalf("logger called %d times over two passes, want 1", calls)
		}
	})
}

func mustFlacReader(t *testing.T, b []byte) io.Reader {
	t.Helper()
	d, err := flacpcm.NewDecoder(bytes.NewReader(b), flacpcm.WithConvertTo(16), flacpcm.SkipMD5Verification())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOpusLoops(t *testing.T) {
	t.Parallel()
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("%dch", channels), func(t *testing.T) {
			t.Parallel()
			// A 24 kHz input rate: only OutputSampleRate (always 48 kHz) is right.
			enc := opusBytes(t, 24000, channels, ramp(24000, channels))
			want, _, err := oggopus.DecodeInterleaved(bytes.NewReader(enc))
			if err != nil {
				t.Fatal(err)
			}
			path := writeFile(t, t.TempDir(), "a.opus", enc)
			synctest.Test(t, func(t *testing.T) {
				var rec recorder
				s, err := Open(cfgFor(path, &rec))
				if err != nil {
					t.Fatal(err)
				}
				defer closeQuiet(s)
				if rate, ch := s.Negotiated(); rate != 48000 || ch != channels {
					t.Fatalf("Negotiated = (%d, %d), want (48000, %d)", rate, ch, channels)
				}
				periodBytes := periodFrames * channels * 2
				n := 2*len(want)/periodBytes + 1
				got := readN(t, s, n)
				if !bytes.Equal(got[:2*len(want)], bytes.Repeat(want, 2)) {
					t.Fatalf("two passes differ from twice the decoded file (%d bytes each)", len(want))
				}
			})
		})
	}
}

func TestReadPacesToWallClock(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, ramp(testRate, 2)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		start := time.Now()
		for i := 1; i <= 50; i++ {
			p, err := s.Read()
			if err != nil {
				t.Fatal(err)
			}
			if want := start.Add(time.Duration(i) * periodDur); !p.Captured.Equal(want) {
				t.Fatalf("period %d Captured = %v, want %v", i, p.Captured.Sub(start), want.Sub(start))
			}
		}
		if got, want := time.Since(start), 50*periodDur; got != want {
			t.Fatalf("50 periods took %v, want %v", got, want)
		}
	})
}

func TestReadCatchUpPolicy(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, ramp(testRate, 2)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		start := time.Now()
		var end time.Time // end of the last period read, on the source's own timeline
		read := func() {
			t.Helper()
			p, err := s.Read()
			if err != nil {
				t.Fatal(err)
			}
			end = p.Captured
		}
		wantNow := func(d time.Duration) {
			t.Helper()
			if got := time.Since(start); got != d {
				t.Fatalf("elapsed %v, want %v", got, d)
			}
		}
		for range 3 {
			read()
		}
		wantNow(3 * periodDur)

		// Exactly 4 completed periods (4..7) wait: all are served at once, each
		// stamped with its own end.
		time.Sleep(4 * periodDur)
		for i := 4; i <= 7; i++ {
			read()
			wantNow(7 * periodDur)
			if want := start.Add(time.Duration(i) * periodDur); !end.Equal(want) {
				t.Fatalf("catch-up period %d Captured = %v, want %v", i, end.Sub(start), want.Sub(start))
			}
		}
		if got := s.Overruns(); got != 0 {
			t.Fatalf("Overruns = %d after a 4-period stall, want 0", got)
		}
		read() // the 5th waits for real time
		wantNow(8 * periodDur)

		// 5 completed periods (9..13) wait: the source re-anchors, counts an
		// overrun and serves the next period one period later.
		time.Sleep(5 * periodDur)
		wantNow(13 * periodDur)
		read()
		wantNow(14 * periodDur)
		if got := s.Overruns(); got != 1 {
			t.Fatalf("Overruns = %d after a 5-period stall, want 1", got)
		}
		if want := start.Add(14 * periodDur); !end.Equal(want) {
			t.Fatalf("re-anchored period Captured = %v, want %v", end.Sub(start), want.Sub(start))
		}
		for i := 15; i <= 20; i++ {
			read()
			wantNow(time.Duration(i) * periodDur)
		}
	})
}

func TestCloseUnblocksRead(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, ramp(testRate, 2)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		res := make(chan error, 1)
		go func() {
			_, err := s.Read()
			res <- err
		}()
		synctest.Wait() // the reader is asleep on its first deadline
		if err := s.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		if err := <-res; !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked Read = %v, want ErrClosed", err)
		}
		if got := time.Since(start); got != 0 {
			t.Fatalf("Close took %v of fake time, want 0 (before the deadline)", got)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil", err)
		}
		if _, err := s.Read(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Read after Close = %v, want ErrClosed", err)
		}
	})
}

func TestReadDoesNotAllocate(t *testing.T) {
	// Not parallel: AllocsPerRun counts allocations process-wide.
	const frames = testRate * 5
	pcm := noise(frames, 2)
	for _, tc := range []struct {
		name string
		file string
		data []byte
	}{
		{"flac", "a.flac", flacBytes(t, testRate, 2, 16, pcm)},
		{"opus", "a.opus", opusBytes(t, 48000, 2, pcm)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), tc.file, tc.data)
			synctest.Test(t, func(t *testing.T) {
				var rec recorder
				s, err := Open(cfgFor(path, &rec))
				if err != nil {
					t.Fatal(err)
				}
				defer closeQuiet(s)
				readN(t, s, 10)
				got := testing.AllocsPerRun(100, func() {
					if _, err := s.Read(); err != nil {
						t.Fatal(err)
					}
				})
				if got != 0 {
					t.Fatalf("allocs per Read = %v, want 0", got)
				}
			})
		})
	}
}

func TestFramesDurationLong(t *testing.T) {
	t.Parallel()
	const year = 365 * 24 * 3600
	tests := []struct {
		name   string
		frames uint64
		rate   int
		want   time.Duration
	}{
		{"one year at 384 kHz", 384000 * year, 384000, year * time.Second},
		{"one frame at 11025 Hz", 1, 11025, 90702}, // 1e9/11025 = 90702.9 ns, floored
		{"one second plus one frame at 11025 Hz", 11026, 11025, time.Second + 90702},
		{"zero frames", 0, 48000, 0},
	}
	for _, tc := range tests {
		if got := framesDuration(tc.frames, tc.rate); got != tc.want {
			t.Errorf("%s: framesDuration = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOpenRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	// A real stereo file, so only the config check can reject these.
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames, 2)))
	for _, cfg := range []Config{
		{Path: path, Rate: 0, MinChannels: 1, MaxChannels: 2},
		{Path: path, Rate: testRate, MinChannels: 0, MaxChannels: 2},
		{Path: path, Rate: testRate, MinChannels: 3, MaxChannels: 2},
	} {
		s, err := Open(cfg)
		if s != nil {
			closeQuiet(s)
		}
		if err == nil || err.Error() != "filesource: invalid config" {
			t.Errorf("Open(%+v) error = %v, want the invalid config error", cfg, err)
		}
	}
}

func TestReadReportsRemovedFileAsUnreadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeFile(t, dir, "a.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames+40, 2)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		readN(t, s, 1)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		// The open descriptor plays out the last 40 frames; the next pass cannot
		// open the file and reports it as unreadable, never as a clean end.
		_, err = s.Read()
		var na *NoAudioError
		if !errors.As(err, &na) || len(na.Skipped) != 1 || na.Skipped[0].Class != SkipUnreadable {
			t.Fatalf("Read = %v, want *NoAudioError with one unreadable skip", err)
		}
		if containsPath(err, dir) || containsPath(errors.New(na.Skipped[0].Reason), dir) {
			t.Fatalf("error carries a path: %v / %q", err, na.Skipped[0].Reason)
		}
	})
}

func TestNoAudioErrorText(t *testing.T) {
	t.Parallel()
	if got, want := (&NoAudioError{}).Error(), "filesource: no playable audio"; got != want {
		t.Errorf("empty error = %q, want %q", got, want)
	}
	e := &NoAudioError{Skipped: []Skip{{Class: SkipRate}, {Class: SkipUnreadable}, {Class: SkipUnreadable}}}
	want := "filesource: no playable audio (1 sample rate differs, 2 unreadable)"
	if got := e.Error(); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestReadReportsFileWithoutAudio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeFile(t, dir, "a.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames+40, 2)))
	empty := writeFile(t, dir, "new.tmp", flacBytes(t, testRate, 2, 16, nil))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		readN(t, s, 1)
		if err := os.Rename(empty, path); err != nil {
			t.Fatal(err)
		}
		_, err = s.Read()
		var na *NoAudioError
		if !errors.As(err, &na) || len(na.Skipped) != 1 || na.Skipped[0].Class != SkipUndecodable {
			t.Fatalf("Read = %v, want *NoAudioError with one undecodable skip", err)
		}
		if got, want := err.Error(), "filesource: no playable audio (1 undecodable)"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
	})
}

func TestOpenReportsFirstReadError(t *testing.T) {
	t.Parallel()
	data := flacBytes(t, testRate, 2, 16, noise(100, 2))
	data[len(data)-1] ^= 0xff // the frame's CRC-16: the header parses, the first read fails
	path := writeFile(t, t.TempDir(), "a.flac", data)
	var rec recorder
	s, err := Open(cfgFor(path, &rec))
	if err == nil {
		closeQuiet(s)
		t.Fatal("Open succeeded on a file whose first frame is corrupt")
	}
	var na *NoAudioError
	if !errors.As(err, &na) || len(na.Skipped) != 1 {
		t.Fatalf("err = %v, want *NoAudioError with one skip", err)
	}
	if got := na.Skipped[0].Reason; got == "no audio" {
		t.Fatalf("skip reason = %q, want the decoder's error", got)
	}
}

func TestShortFileLoopsWithoutSkipping(t *testing.T) {
	t.Parallel()
	// Shorter than one period: the first read returns audio together with the
	// end of the file, and every pass is shorter than a period.
	s16 := ramp(100, 2)
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, s16))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		got := readN(t, s, 3)
		want := bytes.Repeat(s16, 3*periodFrames/100+1)[:3*periodFrames*4]
		if !bytes.Equal(got, want) {
			t.Fatal("short file does not loop gaplessly")
		}
		if lines := rec.all(); len(lines) != 0 {
			t.Fatalf("a playing file logged %q, want nothing", lines)
		}
	})
}

func TestReadPacesAtInexactPeriodRate(t *testing.T) {
	t.Parallel()
	// 11025 Hz: a 220-frame period is 19.954648... ms, so a per-period rounded
	// step would drift from the exact 11000 frames / 11025 Hz.
	const rate = 11025
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, rate, 1, 16, ramp(rate, 1)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(Config{Path: path, Rate: rate, MinChannels: 1, MaxChannels: 1, Logf: rec.logf})
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		start := time.Now()
		readN(t, s, 50)
		if got, want := time.Since(start), 997732426*time.Nanosecond; got != want {
			t.Fatalf("50 periods took %v, want %v", got, want)
		}
	})
}

func TestSlowStartIsNotLag(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, ramp(testRate, 2)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		time.Sleep(10 * periodDur) // a start slower than the 4-period ring
		first := time.Now()
		p, err := s.Read()
		if err != nil {
			t.Fatal(err)
		}
		if want := first.Add(periodDur); !p.Captured.Equal(want) {
			t.Fatalf("first Captured = %v after the read began, want %v", p.Captured.Sub(first), periodDur)
		}
		if got := s.Overruns(); got != 0 {
			t.Fatalf("Overruns = %d after a slow start, want 0", got)
		}
	})
}

// Not parallel: it swaps the process-wide logger.
func TestOpenDefaultsLogfToStdLog(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	data := flacBytes(t, testRate, 2, 16, noise(4096*3, 2))
	data = data[:len(data)*2/3] // decodable frames, then a truncation error
	path := writeFile(t, t.TempDir(), "a.flac", data)
	synctest.Test(t, func(t *testing.T) {
		s, err := Open(Config{Path: path, Rate: testRate, MinChannels: 1, MaxChannels: 2})
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		readN(t, s, 2*len(data)/(periodFrames*4))
	})
	if !strings.Contains(buf.String(), "a.flac") {
		t.Fatalf("std logger saw %q, want the skip line naming a.flac", buf.String())
	}
}

func TestReadLogsAgainAfterACleanPass(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	full := flacBytes(t, testRate, 2, 16, noise(4096*6, 2))
	cut := full[:len(full)*2/3]
	prefix, _ := io.ReadAll(mustFlacReader(t, cut))
	path := writeFile(t, dir, "a.flac", cut)
	clean := writeFile(t, dir, "clean.tmp", flacBytes(t, testRate, 2, 16, noise(4096*2, 2)))
	truncated := writeFile(t, dir, "cut.tmp", cut)
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		periodBytes := periodFrames * 4
		// Pass 1 plays the truncated file; pass 2 plays the clean one.
		if err := os.Rename(clean, path); err != nil {
			t.Fatal(err)
		}
		readN(t, s, len(prefix)/periodBytes+1)
		if lines := rec.all(); len(lines) != 1 {
			t.Fatalf("logged %q after the first failing pass, want 1 line", lines)
		}
		// Pass 3 plays the truncated file again, after pass 2 ended cleanly.
		if err := os.Rename(truncated, path); err != nil {
			t.Fatal(err)
		}
		readN(t, s, (4096*2*4+len(prefix))/periodBytes+2)
		if lines := rec.all(); len(lines) != 2 {
			t.Fatalf("logged %q after fail, clean, fail, want 2 lines (the clean pass forgets the reason)", lines)
		}
	})
}

func TestReadReturnsErrClosedWhenClosedDuringSkipLog(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "a.flac", flacBytes(t, testRate, 2, 16, ramp(periodFrames+40, 2)))
	synctest.Test(t, func(t *testing.T) {
		var s *Source
		cfg := cfgFor(path, new(recorder))
		cfg.Logf = func(string, ...any) { closeQuiet(s) } // Close lands while a skip is being reported
		var err error
		if s, err = Open(cfg); err != nil {
			t.Fatal(err)
		}
		readN(t, s, 1)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Read(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Read = %v, want ErrClosed once Close has run", err)
		}
	})
}

func TestReadLogsEachNewReason(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	full := flacBytes(t, testRate, 2, 16, noise(4096*6, 2))
	path := writeFile(t, dir, "a.flac", full[:len(full)*2/3])
	mono := writeFile(t, dir, "mono.tmp", flacBytes(t, testRate, 1, 16, ramp(periodFrames*3, 1)))
	synctest.Test(t, func(t *testing.T) {
		var rec recorder
		s, err := Open(cfgFor(path, &rec))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuiet(s)
		// Pass 1 fails on truncation; pass 2 finds a channel-count mismatch, a
		// different reason for the same file with no clean pass between.
		if err := os.Rename(mono, path); err != nil {
			t.Fatal(err)
		}
		var na *NoAudioError
		for !errors.As(err, &na) {
			_, err = s.Read()
		}
		lines := rec.all()
		if len(lines) != 2 || !strings.Contains(lines[0], "truncated") || !strings.Contains(lines[1], "channels") {
			t.Fatalf("logged %q, want the truncation line then the channel mismatch line", lines)
		}
	})
}
