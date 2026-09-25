package audio

import "testing"

// loopSource returns the same period forever, so a benchmark times only the
// wrapper under test.
type loopSource struct {
	rate, ch int
	p        Period
}

func (l *loopSource) Negotiated() (rate, channels int) { return l.rate, l.ch }
func (l *loopSource) Read() (Period, error)            { return l.p, nil }
func (l *loopSource) Close() error                     { return nil }

// benchSelect times selectingSource.Read for one channel selection. It fails
// when the selection takes the passthrough path, so the strided copy is what
// gets measured.
func benchSelect(b *testing.B, srcCh, frames int, sel []int) {
	b.Helper()
	buf := make([]byte, frames*srcCh*2)
	for i := range buf {
		buf[i] = byte(i)
	}
	src := NewSelectingSource(&loopSource{rate: 48000, ch: srcCh, p: Period{Buf: buf, Frames: frames}}, srcCh, sel)
	if _, ok := src.(*selectingSource); !ok {
		b.Fatal("selection took the passthrough path")
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := src.Read(); err != nil {
			b.Fatal(err)
		}
	}
}

// The selections that dominate in practice (one channel of a stereo card, at
// 48 kHz and at the 384 kHz ultrasonic rate), plus wider ones for comparison.
func BenchmarkSelectingSourceStereoToMono(b *testing.B)     { benchSelect(b, 2, 960, []int{2}) }
func BenchmarkSelectingSourceStereoToMono384k(b *testing.B) { benchSelect(b, 2, 7680, []int{1}) }
func BenchmarkSelectingSourceStereoSwap(b *testing.B)       { benchSelect(b, 2, 960, []int{2, 1}) }
func BenchmarkSelectingSourceEightToTwo(b *testing.B)       { benchSelect(b, 8, 960, []int{3, 4}) }
func BenchmarkSelectingSourceEightReversed(b *testing.B) {
	benchSelect(b, 8, 960, []int{8, 7, 6, 5, 4, 3, 2, 1})
}
