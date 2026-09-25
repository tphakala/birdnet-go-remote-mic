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

// silence returns n periods of 960 mono frames, one Opus frame each.
func silence(n int) [][]byte {
	periods := make([][]byte, n)
	for i := range periods {
		periods[i] = make([]byte, 960*2)
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
