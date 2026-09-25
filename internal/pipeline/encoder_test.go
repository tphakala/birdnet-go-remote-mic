package pipeline

import (
	"testing"

	"github.com/tphakala/go-opus/opus"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
)

// countingEncoder wraps the real encoder and counts the work done on it.
type countingEncoder struct {
	inner          opusEncoder
	encodes, reset int
}

func (c *countingEncoder) Encode(pcm []int16, buf []byte) (int, error) {
	c.encodes++
	return c.inner.Encode(pcm, buf)
}

func (c *countingEncoder) Reset() {
	c.reset++
	c.inner.Reset()
}

// countingStage returns an Opus stage whose encoder is counted in enc.
func countingStage(t *testing.T) (*opusStage, *countingEncoder) {
	t.Helper()
	enc := &countingEncoder{}
	return &opusStage{bitrate: 64000, newEncoder: func(cfg opus.EncoderConfig) (opusEncoder, error) {
		inner, err := newOpusEncoder(cfg)
		enc.inner = inner
		return enc, err
	}}, enc
}

// TestOpusStageEncoderBitrate pins the bitrate the encoder itself is built with,
// not only the SDP's maxaveragebitrate: an unset bitrate follows the default of
// 128 kbps per channel carried, and an explicit one is passed through.
func TestOpusStageEncoderBitrate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		bitrate, channels int
		want              int
	}{
		{"mono default", 0, 1, 128000},
		{"stereo default", 0, 2, 256000},
		{"explicit", 64000, 2, 64000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got opus.EncoderConfig
			st := &opusStage{bitrate: tc.bitrate, newEncoder: func(cfg opus.EncoderConfig) (opusEncoder, error) {
				got = cfg
				return newOpusEncoder(cfg)
			}}
			src := audio.NewFakeSource(48000, tc.channels, nil)
			if err := st.Run(src, func() (bool, uint64) { return false, 0 }, func(Frame) error { return nil }); err != nil {
				t.Fatalf("Run: got error %v, want none", err)
			}
			if got.Bitrate != tc.want || got.Channels != tc.channels {
				t.Errorf("encoder config: got %d bps %d ch, want %d bps %d ch", got.Bitrate, got.Channels, tc.want, tc.channels)
			}
		})
	}
}

// silence returns n periods of 960 mono frames, one Opus frame each.
func silence(n int) [][]byte {
	periods := make([][]byte, n)
	for i := range periods {
		periods[i] = make([]byte, opusFrameSamples*2)
	}
	return periods
}

// TestOpusStageIdleRunsNoEncode proves an unplayed stream costs no encode, not
// merely that it emits nothing: a stage that encoded and then discarded would
// pass an emit-only check while burning the CPU the gate exists to save.
func TestOpusStageIdleRunsNoEncode(t *testing.T) {
	t.Parallel()
	st, enc := countingStage(t)
	idle := func() (bool, uint64) { return false, 0 }
	if err := st.Run(audio.NewFakeSource(48000, 1, silence(8)), idle, func(Frame) error { return nil }); err != nil {
		t.Fatalf("Run: got error %v, want none", err)
	}
	if enc.encodes != 0 || enc.reset != 0 {
		t.Errorf("idle stream: got %d encodes and %d resets, want 0 and 0", enc.encodes, enc.reset)
	}
}

// TestOpusStageResetsOncePerSession pins the reset to the session change: one
// per client, not one per period, so a playing stream keeps its encoder state
// from frame to frame.
func TestOpusStageResetsOncePerSession(t *testing.T) {
	t.Parallel()
	st, enc := countingStage(t)
	sessions := []uint64{1, 1, 1, 2, 2, 2}
	n := 0
	gate := func() (bool, uint64) {
		if n >= len(sessions) {
			t.Errorf("gate called %d times, want one call per period (%d)", n+1, len(sessions))
			return false, 0
		}
		s := sessions[n]
		n++
		return true, s
	}
	if err := st.Run(audio.NewFakeSource(48000, 1, silence(len(sessions))), gate, func(Frame) error { return nil }); err != nil {
		t.Fatalf("Run: got error %v, want none", err)
	}
	if enc.encodes != len(sessions) {
		t.Errorf("got %d encodes, want %d (one per period)", enc.encodes, len(sessions))
	}
	if enc.reset != 2 {
		t.Errorf("got %d resets, want 2 (one per session)", enc.reset)
	}
}
