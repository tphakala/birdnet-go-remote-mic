// Package audio adapts go-audio-capture into the appliance's Source interface
// and provides a hardware-free fake for tests.
package audio

import "io"

// Period is one capture period of interleaved S16LE PCM. Buf is owned by the
// receiver only until the next Read (single-consumer, the underlying buffer is
// reused).
type Period struct {
	Buf    []byte
	Frames int
}

// Source delivers periods of S16LE PCM. Read blocks until a period is available
// and returns io.EOF (or a driver error) when the source ends.
type Source interface {
	Negotiated() (rate, channels int)
	Read() (Period, error)
	Close() error
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
