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
	// Pos is the stream position of the period's first frame, counted in frames
	// at the capture rate from the start of the fan-out's capture. The fan-out
	// counts every period it read, including ones it sent no consumer (idle) or
	// dropped for a full queue, so a gap in Pos between two periods a stage reads
	// is exactly the audio lost between them. Zero for a period straight from a
	// capture; NewFakeSource numbers its periods the same way, and NewPeriodSource
	// keeps it as given.
	Pos uint64
	// Session is the play session of the stream a fan-out consumer's period
	// was sent for (see FanoutStream.Gate), so a stage can drop a period queued
	// for an earlier play session rather than encode it for the current one.
	// Zero means untagged: a period straight from a capture, or a consumer with
	// no gate.
	Session uint64
	// Captured is when the period was read from the capture, so a stage that
	// has fallen behind still stamps its frames with their capture time. The
	// fan-out sets it unless its upstream already did. It marks the end of the
	// period (taken when its read returns), so a stage back-dates it by sample
	// position to stamp each frame. Zero means untagged, and the stage stamps its
	// own read time.
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
// capture layer's only trace. A source that cannot count them (a fake, a
// fan-out consumer) reports zero.
func Overruns(src Source) uint64 {
	if c, ok := src.(overrunCounter); ok {
		return c.Overruns()
	}
	return 0
}

// Geometry is a requested capture period geometry. The zero value asks for the
// library default (20 ms x 4 on Linux); non-zero values are targets the driver
// may move to the nearest geometry it accepts, so read the granted values back
// with NegotiatedGeometry.
type Geometry struct {
	PeriodFrames int // frames per period
	Periods      int // periods in the ring buffer
}

// geometryReporter is implemented by a Source that knows the capture period
// geometry its driver granted.
type geometryReporter interface {
	NegotiatedGeometry() Geometry
}

// NegotiatedGeometry returns the period geometry the driver granted src, and
// false for a source that cannot report one (a fake, a wrapper). It reads the
// stream's negotiated config, so call it once per open, not per period.
func NegotiatedGeometry(src Source) (Geometry, bool) {
	if r, ok := src.(geometryReporter); ok {
		return r.NegotiatedGeometry(), true
	}
	return Geometry{}, false
}

// NewFakeSource returns a Source that replays periods (each a slice of S16LE
// bytes) in order, then returns io.EOF. It is used by the pipeline and server
// tests to drive the send path without hardware. Each period's Pos is the
// frames replayed before it.
func NewFakeSource(rate, channels int, periods [][]byte) Source {
	return &fakeSource{rate: rate, channels: channels, periods: periods}
}

type fakeSource struct {
	rate, channels int
	periods        [][]byte
	idx            int
	pos            uint64 // Period.Pos of the next period
}

func (f *fakeSource) Negotiated() (rate, channels int) { return f.rate, f.channels }

func (f *fakeSource) Read() (Period, error) {
	if f.idx >= len(f.periods) {
		return Period{}, io.EOF
	}
	p := f.periods[f.idx]
	f.idx++
	frames := len(p) / (2 * f.channels)
	pos := f.pos
	f.pos += uint64(frames)
	return Period{Buf: p, Frames: frames, Pos: pos}, nil
}

func (f *fakeSource) Close() error { return nil }

// NewPeriodSource returns a Source that replays whole periods in order, then
// returns io.EOF. Unlike NewFakeSource it keeps every Period field as given,
// so a test can hand a consumer what a fan-out queues: periods carrying a play
// session, a capture time and a stream position.
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
