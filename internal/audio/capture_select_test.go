//go:build linux

package audio

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"

	capture "github.com/tphakala/go-audio-capture"
)

// interleave builds one interleaved S16LE period. sample(frame, channel) supplies
// the value for each slot so a test can assert exactly which channel landed where.
func interleave(frames, channels int, sample func(f, c int) int16) []byte {
	buf := make([]byte, frames*channels*2)
	for f := range frames {
		for c := range channels {
			binary.LittleEndian.PutUint16(buf[(f*channels+c)*2:], uint16(sample(f, c)))
		}
	}
	return buf
}

// readSamples decodes an interleaved S16LE period into [frame][channel] samples.
func readSamples(buf []byte, frames, channels int) [][]int16 {
	out := make([][]int16, frames)
	for f := range frames {
		out[f] = make([]int16, channels)
		for c := range channels {
			out[f][c] = int16(binary.LittleEndian.Uint16(buf[(f*channels+c)*2:]))
		}
	}
	return out
}

func TestSelectingSourceExtractsChannels(t *testing.T) {
	t.Parallel()
	// Distinct per-channel signal so a misrouted channel is obvious: channel c,
	// frame f -> c*100 + f.
	val := func(f, c int) int16 { return int16(c*100 + f) }

	cases := []struct {
		name      string
		srcCh     int
		selection []int // 1-based
		wantCols  []int // 0-based source channels expected in the output, in order
	}{
		{"mono from stereo channel 1", 2, []int{1}, []int{0}},
		{"mono from stereo channel 2", 2, []int{2}, []int{1}},
		{"single channel from quad", 4, []int{3}, []int{2}},
		{"non-contiguous pair from quad", 4, []int{1, 3}, []int{0, 2}},
		{"reordered selection stays in selection order", 4, []int{4, 1}, []int{3, 0}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const frames = 3
			period := interleave(frames, tt.srcCh, val)
			inner := NewFakeSource(48000, tt.srcCh, [][]byte{period})
			s := newSelectingSource(inner, tt.srcCh, tt.selection)

			_, gotCh := s.Negotiated()
			if gotCh != len(tt.selection) {
				t.Fatalf("Negotiated channels = %d, want %d", gotCh, len(tt.selection))
			}
			p, err := s.Read()
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if p.Frames != frames {
				t.Fatalf("Frames = %d, want %d", p.Frames, frames)
			}
			got := readSamples(p.Buf, frames, len(tt.wantCols))
			for f := range frames {
				for j, srcC := range tt.wantCols {
					if want := val(f, srcC); got[f][j] != want {
						t.Errorf("frame %d out-channel %d = %d, want source channel %d value %d", f, j, got[f][j], srcC, want)
					}
				}
			}
		})
	}
}

func TestSelectingSourceReusesBufferAcrossReads(t *testing.T) {
	t.Parallel()
	// Two periods of DIFFERENT frame counts through one selecting source: the
	// second Read must return the second period's data, not a stale slice of the
	// first, and the grow/reuse of the internal buffer must not corrupt content.
	val := func(base int) func(f, c int) int16 {
		return func(f, c int) int16 { return int16(base + c*100 + f) }
	}
	p1 := interleave(2, 4, val(0))
	p2 := interleave(5, 4, val(1000)) // more frames, forces a grow
	inner := NewFakeSource(48000, 4, [][]byte{p1, p2})
	s := newSelectingSource(inner, 4, []int{1, 3}) // extract channels 0 and 2

	got1, err := s.Read()
	if err != nil {
		t.Fatalf("Read 1: %v", err)
	}
	r1 := readSamples(got1.Buf, 2, 2)
	for f := range 2 {
		if r1[f][0] != val(0)(f, 0) || r1[f][1] != val(0)(f, 2) {
			t.Fatalf("period 1 frame %d = %v, want [%d %d]", f, r1[f], val(0)(f, 0), val(0)(f, 2))
		}
	}
	got2, err := s.Read()
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if got2.Frames != 5 {
		t.Fatalf("period 2 frames = %d, want 5", got2.Frames)
	}
	r2 := readSamples(got2.Buf, 5, 2)
	for f := range 5 {
		if r2[f][0] != val(1000)(f, 0) || r2[f][1] != val(1000)(f, 2) {
			t.Fatalf("period 2 frame %d = %v (stale-buffer regression?)", f, r2[f])
		}
	}
}

// closeTrackingSource records whether Close was called, to prove selectingSource
// delegates Close to its inner source (a dropped Close leaks the capture handle).
type closeTrackingSource struct {
	Source
	closed bool
}

func (c *closeTrackingSource) Close() error { c.closed = true; return nil }

func TestSelectingSourceClosesInner(t *testing.T) {
	t.Parallel()
	inner := &closeTrackingSource{Source: NewFakeSource(48000, 4, nil)}
	s := newSelectingSource(inner, 4, []int{1}) // non-passthrough wrapper
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Error("selectingSource.Close did not close the inner source")
	}
}

func TestSelectingSourcePassthroughWhenSelectingAll(t *testing.T) {
	t.Parallel()
	inner := NewFakeSource(48000, 2, [][]byte{interleave(1, 2, func(_, c int) int16 { return int16(c) })})
	// Selecting exactly channels 1..N in order must return the inner source
	// unwrapped so the hot path adds no copy.
	if got := newSelectingSource(inner, 2, []int{1, 2}); got != inner {
		t.Errorf("newSelectingSource([1,2]) = %T, want the inner source unwrapped", got)
	}
}

func TestSelectingSourcePropagatesEOF(t *testing.T) {
	t.Parallel()
	inner := NewFakeSource(48000, 2, nil)
	s := newSelectingSource(inner, 2, []int{1})
	if _, err := s.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("Read err = %v, want io.EOF", err)
	}
}

func TestResolveOpenChannels(t *testing.T) {
	// accepts models a device that opens only the channel counts in the set.
	accepts := func(set ...int) func(string, int, capture.Format) (capture.RateSupport, error) {
		ok := map[int]bool{}
		for _, c := range set {
			ok[c] = true
		}
		return func(_ string, ch int, _ capture.Format) (capture.RateSupport, error) {
			if ok[ch] {
				return capture.RateSupport{Rates: []int{48000}}, nil
			}
			return capture.RateSupport{}, &capture.BadFormatError{Channels: ch}
		}
	}

	cases := []struct {
		name      string
		accepts   []int
		selection []int
		want      int
	}{
		{"mono-capable opens one channel for a mono selection", []int{1, 2}, []int{1}, 1},
		{"stereo-only rounds a mono selection up to stereo", []int{2}, []int{1}, 2},
		{"stereo selection opens stereo", []int{1, 2}, []int{1, 2}, 2},
		{"quad device rounds a channel-3 selection up to quad", []int{2, 4}, []int{1, 3}, 4},
		{"no probeable count falls back to max selected", []int{}, []int{2}, 2},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			restore := swapSupportedRates(accepts(tt.accepts...))
			defer restore()
			if got := ResolveOpenChannels("hw:9,9", tt.selection); got != tt.want {
				t.Errorf("ResolveOpenChannels(%v) = %d, want %d", tt.selection, got, tt.want)
			}
		})
	}

	// A wider capability set does not affect the answer for a selection that a
	// smaller count already covers.
	restore := swapSupportedRates(accepts(1, 2, 4, 6, 8))
	defer restore()
	if got := ResolveOpenChannels("hw:9,9", []int{1, 2}); got != 2 {
		t.Errorf("ResolveOpenChannels([1,2]) = %d, want 2", got)
	}
}
