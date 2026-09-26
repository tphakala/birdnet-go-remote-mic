// Package audio adapts go-audio-capture into the appliance's Source interface
// and provides a hardware-free fake for tests.
package audio

import (
	"io"
	"time"
)

// Period is one capture period of interleaved S16LE PCM. Buf is owned by the
// receiver only until the next Read (single-consumer, the underlying buffer is
// reused).
type Period struct {
	Buf    []byte
	Frames int
	// Session is the play session of the stream a fan-out consumer's period
	// was sent for (see FanoutStream.Gate), so a stage can drop a period queued
	// for an earlier client rather than encode it for the next one. Zero means
	// untagged: a period straight from a capture, or a consumer with no gate.
	Session uint64
	// Captured is when the period was read from the capture, so a stage that
	// has fallen behind still stamps its frames with their capture time. The
	// fan-out sets it unless its upstream already did. Zero means untagged, and
	// the stage stamps its own read time.
	Captured time.Time
}

// Source delivers periods of S16LE PCM. Read blocks until a period is available
// and returns io.EOF (or a driver error) when the source ends.
type Source interface {
	Negotiated() (rate, channels int)
	Read() (Period, error)
	Close() error
}

// overrunCounter is implemented by a Source that can report the capture
// overruns it has recovered from, and by a wrapper that forwards the count.
type overrunCounter interface {
	Overruns() uint64
}

// Overruns returns src's cumulative count of recovered capture overruns (ALSA
// xruns). Each one is a gap where audio was lost, usually because the capture
// buffer filled before it was drained (go-audio-capture also counts a recovered
// system suspend); the capture recovers and keeps reading, so the count is the
// capture layer's only trace. A source that cannot count them (a fake, a fan-out consumer)
// reports zero.
func Overruns(src Source) uint64 {
	if c, ok := src.(overrunCounter); ok {
		return c.Overruns()
	}
	return 0
}

// NewFakeSource returns a Source that replays periods (each a slice of S16LE
// bytes) in order, then returns io.EOF. It is used by the pipeline and server
// tests to drive the send path without hardware.
func NewFakeSource(rate, channels int, periods [][]byte) Source {
	return &fakeSource{rate: rate, channels: channels, periods: periods}
}

type fakeSource struct {
	rate, channels int
	periods        [][]byte
	idx            int
}

func (f *fakeSource) Negotiated() (rate, channels int) { return f.rate, f.channels }

func (f *fakeSource) Read() (Period, error) {
	if f.idx >= len(f.periods) {
		return Period{}, io.EOF
	}
	p := f.periods[f.idx]
	f.idx++
	return Period{Buf: p, Frames: len(p) / (2 * f.channels)}, nil
}

func (f *fakeSource) Close() error { return nil }

// NewPeriodSource returns a Source that replays whole periods in order, then
// returns io.EOF. Unlike NewFakeSource it keeps every Period field as given,
// so a test can hand a consumer what a fan-out queues: periods carrying a play
// session and a capture time.
func NewPeriodSource(rate, channels int, periods []Period) Source {
	return &periodSource{rate: rate, channels: channels, periods: periods}
}

type periodSource struct {
	rate, channels int
	periods        []Period
}

func (s *periodSource) Negotiated() (rate, channels int) { return s.rate, s.channels }

func (s *periodSource) Read() (Period, error) {
	if len(s.periods) == 0 {
		return Period{}, io.EOF
	}
	p := s.periods[0]
	s.periods = s.periods[1:]
	return p, nil
}

func (s *periodSource) Close() error { return nil }
