// Package filesource plays an audio file in a loop as an audio.Source that
// stands in for a sound card, mainly for testing. It is its own clock: Read
// paces itself to real time, so nothing downstream paces.
//
// The rate is the device's and the channel count is the file's, both fixed at
// Open from the first file that produced audio. Read never returns io.EOF: a
// file source that cannot produce audio fails with a *NoAudioError, so the
// fan-out treats it as a dead device and the appliance retries it. Errors from
// this package carry no file paths.
package filesource

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
)

// ErrClosed is returned by Read once the source is closed.
var ErrClosed = errors.New("filesource: closed")

// Config describes a file source.
type Config struct {
	// Path names the file to play.
	Path string
	// Rate is the device rate in Hz. A file at another rate is not played; there
	// is no resampling.
	Rate int
	// MinChannels and MaxChannels bound the file's channel count.
	MinChannels, MaxChannels int
	// Logf receives one line per skipped or failing file. Nil means log.Printf.
	Logf func(format string, args ...any)
}

// SkipClass says why a file was not played.
type SkipClass int

// Skip classes.
const (
	SkipRate        SkipClass = iota // sample rate differs from the device rate
	SkipChannels                     // channel count unusable
	SkipUndecodable                  // header error, or no audio
	SkipUnreadable                   // the file could not be opened
	SkipUnsupported                  // no decoder for the extension
	numSkipClasses
)

func (c SkipClass) String() string {
	switch c {
	case SkipRate:
		return "sample rate differs"
	case SkipChannels:
		return "channel count unusable"
	case SkipUndecodable:
		return "undecodable"
	case SkipUnreadable:
		return "unreadable"
	case SkipUnsupported:
		return "unsupported format"
	case numSkipClasses:
	}
	return "unknown"
}

// Skip is one file that was not played.
type Skip struct {
	Name   string // base name of the file
	Reason string
	Class  SkipClass
}

// NoAudioError reports that a pass produced no audio. Its text names reason
// classes and counts only; Skipped carries the per-file detail for local
// output such as serve --check.
type NoAudioError struct {
	Skipped []Skip
}

func (e *NoAudioError) Error() string {
	var counts [numSkipClasses]int
	for _, s := range e.Skipped {
		counts[s.Class]++
	}
	var parts []string
	for c := range numSkipClasses {
		if counts[c] > 0 {
			parts = append(parts, fmt.Sprintf("%d %v", counts[c], c))
		}
	}
	if len(parts) == 0 {
		return "filesource: no playable audio"
	}
	return "filesource: no playable audio (" + strings.Join(parts, ", ") + ")"
}

// Source plays a file in a loop. Read and Overruns may run concurrently with
// Close; Read itself is for a single goroutine.
type Source struct {
	cfg          Config
	logf         func(format string, args ...any)
	rate         int
	channels     int
	periodFrames int
	name         string // base name of the file, for logs and skips

	// Reader goroutine only.
	buf       []byte // one period, reused
	primed    int    // bytes of buf already filled by Open
	dec       decoders
	cur       io.Reader // the open file's decoder, nil between files
	passBytes int       // bytes this pass produced
	passDone  bool      // this pass has opened its file
	passSkips []Skip
	logged    map[string]string // entry name to the last logged reason
	pace      *pacer

	mu   sync.Mutex // guards file only
	file *os.File

	done chan struct{}
	once sync.Once
}

// Open validates cfg and opens the file. It accepts the file only when its
// header passes the rate and channel checks and its first read returns audio;
// those bytes start the first period. It logs nothing when it fails.
func Open(cfg Config) (*Source, error) {
	if cfg.Rate <= 0 || cfg.MinChannels < 1 || cfg.MinChannels > cfg.MaxChannels {
		return nil, errors.New("filesource: invalid config")
	}
	fi, err := os.Stat(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("file source: %w", stripPath(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("file source: not a regular file")
	}
	s := &Source{
		cfg:          cfg,
		logf:         cfg.Logf,
		periodFrames: max(1, cfg.Rate/50),
		name:         filepath.Base(cfg.Path),
		logged:       make(map[string]string),
		done:         make(chan struct{}),
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	s.pace = newPacer(cfg.Rate, s.periodFrames)
	skip, played, err := s.tryOpen()
	if err != nil {
		return nil, err
	}
	if !played {
		return nil, &NoAudioError{Skipped: []Skip{skip}}
	}
	s.passDone = true
	return s, nil
}

// Negotiated returns the device rate and the file's channel count, both fixed
// for the source's lifetime.
func (s *Source) Negotiated() (rate, channels int) { return s.rate, s.channels }

// Overruns counts the times a late reader made the source re-anchor.
func (s *Source) Overruns() uint64 { return s.pace.overruns.Load() }

// Close unblocks a Read, closes the open file and stops the pacing timer. It is
// idempotent.
func (s *Source) Close() error {
	s.once.Do(func() { close(s.done) })
	s.pace.timer.Stop()
	s.closeFile()
	return nil
}

func (s *Source) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Read returns the next period, looping the file forever. It blocks until the
// period has ended in real time. It never returns io.EOF.
func (s *Source) Read() (audio.Period, error) {
	if s.closed() {
		return audio.Period{}, ErrClosed
	}
	n := s.primed
	s.primed = 0
	for n < len(s.buf) {
		if s.cur == nil {
			if err := s.nextFile(); err != nil {
				return audio.Period{}, err
			}
			continue
		}
		m, err := s.cur.Read(s.buf[n:])
		n += m
		s.passBytes += m
		if err != nil {
			if s.closed() {
				return audio.Period{}, ErrClosed
			}
			s.endFile(err)
		}
	}
	captured, err := s.pace.wait(s.periodFrames, s.done)
	if err != nil {
		return audio.Period{}, err
	}
	return audio.Period{Buf: s.buf, Frames: s.periodFrames, Captured: captured}, nil
}

// nextFile opens the file for the next pass, or reports the end of a pass that
// produced no audio. A single file is its own pass.
func (s *Source) nextFile() error {
	if s.passDone {
		if s.passBytes == 0 {
			return &NoAudioError{Skipped: s.passSkips}
		}
		s.passBytes, s.passSkips, s.passDone = 0, nil, false
	}
	s.passDone = true
	skip, played, err := s.tryOpen()
	if err != nil {
		return err
	}
	if !played {
		s.passSkips = append(s.passSkips, skip)
		s.logOnce(skip.Name, skip.Reason)
	}
	return nil
}

// tryOpen opens the file and installs its decoder as s.cur. It returns played
// false with the Skip when the file cannot be played, and ErrClosed when Close
// ran meanwhile. On
// the first call (channels still unknown) it fixes the geometry from the file
// and keeps its first read as the start of the first period.
func (s *Source) tryOpen() (Skip, bool, error) {
	skip := func(c SkipClass, reason string) (Skip, bool, error) {
		s.closeFile()
		s.cur = nil
		return Skip{Name: s.name, Reason: reason, Class: c}, false, nil
	}
	f, err := os.Open(s.cfg.Path)
	if err != nil {
		return Skip{Name: s.name, Reason: reasonOf(err), Class: SkipUnreadable}, false, nil
	}
	// Install the file before parsing its header so Close can interrupt a slow
	// read; refuse it when Close already ran.
	s.mu.Lock()
	if s.closed() {
		s.mu.Unlock()
		_ = f.Close()
		return Skip{}, false, ErrClosed
	}
	s.file = f
	s.mu.Unlock()

	r, rate, channels, err := s.dec.open(s.name, f)
	if s.closed() {
		s.closeFile()
		return Skip{}, false, ErrClosed
	}
	switch {
	case errors.Is(err, errUnsupportedFormat):
		return skip(SkipUnsupported, err.Error())
	case err != nil:
		return skip(SkipUndecodable, reasonOf(err))
	case rate != s.cfg.Rate:
		return skip(SkipRate, fmt.Sprintf("sample rate %d Hz, device rate %d Hz", rate, s.cfg.Rate))
	case s.channels != 0 && channels != s.channels:
		return skip(SkipChannels, fmt.Sprintf("%d channels, the source plays %d", channels, s.channels))
	case channels < s.cfg.MinChannels || channels > s.cfg.MaxChannels:
		return skip(SkipChannels, fmt.Sprintf("%d channels, need %d to %d", channels, s.cfg.MinChannels, s.cfg.MaxChannels))
	}
	s.cur = r
	if s.channels != 0 {
		return Skip{}, true, nil
	}

	// First file: it must produce audio before it fixes the geometry.
	buf := make([]byte, s.periodFrames*channels*2)
	m, err := r.Read(buf)
	if s.closed() {
		s.closeFile()
		s.cur = nil
		return Skip{}, false, ErrClosed
	}
	if m == 0 {
		return skip(SkipUndecodable, "no audio")
	}
	s.rate, s.channels, s.buf = rate, channels, buf
	s.primed, s.passBytes = m, m
	if err != nil {
		s.endFile(err)
	}
	return Skip{}, true, nil
}

// endFile closes the current file after the decoder returned err. A clean end
// forgets the file's logged reason; any other error is logged once per reason.
func (s *Source) endFile(err error) {
	if errors.Is(err, io.EOF) {
		delete(s.logged, s.name)
	} else {
		s.logOnce(s.name, reasonOf(err))
	}
	s.closeFile()
	s.cur = nil
}

func (s *Source) logOnce(name, reason string) {
	if s.logged[name] == reason {
		return
	}
	s.logged[name] = reason
	s.logf("filesource: %s: %s", name, reason)
}

// closeFile closes the current file exactly once however it is reached: the
// pointer is taken and cleared under the mutex, the close runs outside it.
func (s *Source) closeFile() {
	s.mu.Lock()
	f := s.file
	s.file = nil
	s.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// stripPath drops the path from an *fs.PathError, so the text names the cause
// only and errors.Is still matches it.
func stripPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

func reasonOf(err error) string { return stripPath(err).Error() }
