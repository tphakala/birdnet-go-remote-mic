// Package filesource plays an audio file, or a directory of them, in a loop as
// an audio.Source that stands in for a sound card, mainly for testing. It is
// its own clock: Read paces itself to real time, so nothing downstream paces.
//
// A directory plays its FLAC and Opus files in name order, listed again at the
// start of every pass so added and removed files are picked up. The rate is
// the device's and the channel count is the file's, both fixed at Open from
// the first file that produced audio; a file that does not match is skipped. Read never returns io.EOF: a
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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
)

// ErrClosed is returned by Read once the source is closed.
var ErrClosed = errors.New("filesource: closed")

// Config describes a file source.
type Config struct {
	// Path names the file to play, or a directory whose FLAC and Opus files
	// play in name order. A directory is not searched recursively.
	Path string
	// Rate is the device rate in Hz. A file at another rate is not played; there
	// is no resampling.
	Rate int
	// MinChannels and MaxChannels bound the file's channel count.
	MinChannels, MaxChannels int
	// Logf receives one line per skipped or failing file and reason. Nil means log.Printf.
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

// Source plays a file or a directory of files in a loop. Read and Overruns may run concurrently with
// Close; Read itself is for a single goroutine.
type Source struct {
	cfg          Config
	logf         func(format string, args ...any)
	channels     int
	periodFrames int
	dir          string // directory of the pass's entries

	// Reader goroutine only.
	buf       []byte // one period, reused
	primed    int    // bytes of buf already filled by Open
	dec       decoders
	cur       io.Reader // the open file's decoder, nil between files
	name      string    // base name of the current entry, for logs and skips
	entries   []string  // this pass's listing, in play order
	next      int       // index of the entry to try next
	inPass    bool      // entries is the current pass's listing
	passAudio bool      // this pass produced audio
	fileAudio bool      // the current file produced audio
	passSkips []Skip
	logged    map[string]string // entry name to the last logged reason
	pace      *pacer

	mu   sync.Mutex // guards file only
	file *os.File

	done chan struct{}
	once sync.Once
}

// Open validates cfg and opens the first file that qualifies. A file qualifies
// when its header passes the rate and channel checks and its first read
// returns audio; those bytes start the first period. The files skipped before
// it are logged once; when none qualifies Open returns them in a
// *NoAudioError and logs nothing.
func Open(cfg Config) (*Source, error) {
	if cfg.Rate <= 0 || cfg.MinChannels < 1 || cfg.MinChannels > cfg.MaxChannels {
		return nil, errors.New("filesource: invalid config")
	}
	s := &Source{
		cfg:          cfg,
		logf:         cfg.Logf,
		periodFrames: max(1, cfg.Rate/50),
		logged:       make(map[string]string),
		done:         make(chan struct{}),
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	s.pace = newPacer(cfg.Rate, s.periodFrames)
	if err := s.scan(); err != nil {
		return nil, fmt.Errorf("file source: %w", stripPath(err))
	}
	// Loop until a file fixed the geometry: its priming read may also have
	// ended it, which leaves s.cur nil with audio already primed.
	for s.channels == 0 {
		if s.next >= len(s.entries) {
			return nil, &NoAudioError{Skipped: s.passSkips}
		}
		if _, _, err := s.tryNext(); err != nil {
			return nil, err
		}
	}
	for _, sk := range s.passSkips {
		s.logOnce(sk.Name, sk.Reason)
	}
	return s, nil
}

// Negotiated returns the device rate and the file's channel count, both fixed
// for the source's lifetime.
func (s *Source) Negotiated() (rate, channels int) { return s.cfg.Rate, s.channels }

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
		if m > 0 {
			s.passAudio, s.fileAudio = true, true
		}
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

// nextFile tries the next entry of the pass, starting a pass when none is
// running. It returns nil with no file installed for an entry that was
// skipped, so Read calls it again. It reports a pass that produced no audio,
// whether the path is gone, empty or holds nothing playable.
func (s *Source) nextFile() error {
	if s.closed() {
		return ErrClosed
	}
	if !s.inPass {
		if err := s.scan(); err != nil {
			sk := Skip{Name: filepath.Base(s.cfg.Path), Reason: reasonOf(err), Class: SkipUnreadable}
			s.logOnce(sk.Name, sk.Reason)
			if s.closed() {
				return ErrClosed
			}
			return &NoAudioError{Skipped: []Skip{sk}}
		}
	}
	if s.next >= len(s.entries) {
		s.inPass = false
		if !s.passAudio {
			return &NoAudioError{Skipped: s.passSkips}
		}
		return nil
	}
	sk, skipped, err := s.tryNext()
	if err != nil {
		return err
	}
	if skipped {
		s.logOnce(sk.Name, sk.Reason)
	}
	return nil
}

// errNotPlayable marks a path that is neither a regular file nor a directory.
var errNotPlayable = errors.New("not a regular file or directory")

// errEntryGone marks an entry that vanished between the listing and its turn.
var errEntryGone = errors.New("entry gone")

// scan lists the pass: a directory's non-directory entries with a supported
// extension in name order, or the path itself when it is a regular file (its
// own extension then decides, so an unsupported one is reported as a skip).
// It forgets the logged reasons of files no longer listed. Its errors are
// raw, for the caller to strip.
func (s *Source) scan() error {
	fi, err := os.Stat(s.cfg.Path)
	if err != nil {
		return err
	}
	var names []string
	switch {
	case fi.IsDir():
		des, err := os.ReadDir(s.cfg.Path)
		if err != nil {
			return err
		}
		s.dir = s.cfg.Path
		for _, de := range des {
			if !de.IsDir() && supported(de.Name()) {
				names = append(names, de.Name())
			}
		}
	case fi.Mode().IsRegular():
		s.dir = filepath.Dir(s.cfg.Path)
		names = []string{filepath.Base(s.cfg.Path)}
	default:
		return errNotPlayable
	}
	maps.DeleteFunc(s.logged, func(name, _ string) bool { return !slices.Contains(names, name) })
	s.entries, s.next, s.inPass = names, 0, true
	s.passAudio, s.passSkips = false, nil
	return nil
}

// tryNext tries the pass's next entry and records its Skip in passSkips when
// it was not played. An entry that vanished is neither played nor reported.
func (s *Source) tryNext() (sk Skip, skipped bool, err error) {
	s.name = s.entries[s.next]
	s.next++
	sk, played, err := s.tryOpen()
	switch {
	case errors.Is(err, errEntryGone):
		return Skip{}, false, nil
	case err != nil:
		return Skip{}, false, err
	case played:
		return Skip{}, false, nil
	}
	s.passSkips = append(s.passSkips, sk)
	return sk, true, nil
}

// tryOpen opens the current entry and installs its decoder as s.cur. It
// returns played false with the Skip when the file cannot be played, errEntryGone
// when it no longer exists, and ErrClosed when Close ran meanwhile. On the first call (channels still unknown) it fixes the
// geometry from the file and keeps its first read as the start of the first
// period.
func (s *Source) tryOpen() (Skip, bool, error) {
	skip := func(c SkipClass, reason string) (Skip, bool, error) {
		s.dropFile()
		return Skip{Name: s.name, Reason: reason, Class: c}, false, nil
	}
	// An entry can be a FIFO, and opening one without a writer blocks before
	// the file is installed, where Close cannot reach it.
	path := filepath.Join(s.dir, s.name)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Skip{}, false, errEntryGone
	}
	if err != nil {
		return Skip{Name: s.name, Reason: reasonOf(err), Class: SkipUnreadable}, false, nil
	}
	if !fi.Mode().IsRegular() {
		return Skip{Name: s.name, Reason: "not a regular file", Class: SkipUnreadable}, false, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Skip{}, false, errEntryGone
	}
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
	s.cur, s.fileAudio = r, false
	if s.channels != 0 {
		return Skip{}, true, nil
	}

	// First file: it must produce audio before it fixes the geometry.
	buf := make([]byte, s.periodFrames*channels*2)
	m, err := r.Read(buf)
	if s.closed() {
		s.dropFile()
		return Skip{}, false, ErrClosed
	}
	if m == 0 {
		return skip(SkipUndecodable, noAudioReason(err))
	}
	s.channels, s.buf = channels, buf
	s.primed, s.passAudio, s.fileAudio = m, true, true
	if err != nil {
		s.endFile(err)
	}
	return Skip{}, true, nil
}

// endFile closes the current file after the decoder returned err. A file that
// produced no audio is a skip of this pass. Otherwise a clean end forgets the
// file's logged reason, and any other error is logged once per reason.
func (s *Source) endFile(err error) {
	switch {
	case !s.fileAudio:
		reason := noAudioReason(err)
		s.passSkips = append(s.passSkips, Skip{Name: s.name, Reason: reason, Class: SkipUndecodable})
		s.logOnce(s.name, reason)
	case errors.Is(err, io.EOF):
		delete(s.logged, s.name)
	default:
		s.logOnce(s.name, reasonOf(err))
	}
	s.dropFile()
}

// dropFile closes the current file and forgets its decoder.
func (s *Source) dropFile() {
	s.closeFile()
	s.cur = nil
}

// logOnce logs reason for name unless it is the reason last logged for name.
// Logf is the caller's callback and runs on the capture pump, so a panic in it
// is recovered here: playback and the file cleanup after the log go on.
func (s *Source) logOnce(name, reason string) {
	if s.logged[name] == reason {
		return
	}
	s.logged[name] = reason
	defer func() {
		if r := recover(); r != nil {
			log.Printf("filesource: %s: logger panicked: %v", name, r)
		}
	}()
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

// noAudioReason names why a file ended without audio: the decoder's error, or
// "no audio" for a clean end.
func noAudioReason(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return "no audio"
	}
	return reasonOf(err)
}
