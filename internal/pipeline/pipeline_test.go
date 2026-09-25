package pipeline_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/tphakala/go-opus/opus"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

func TestPCMStageRoundTrip(t *testing.T) {
	t.Parallel()
	const rate, ch = 48000, 1
	periodFrames := rate / 50 // 960

	mk := func(start int) []byte {
		b := make([]byte, periodFrames*2)
		for i := range periodFrames {
			binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(start+i)))
		}
		return b
	}
	p0, p1 := mk(0), mk(1000)
	src := audio.NewFakeSource(rate, ch, [][]byte{p0, p1})

	var got []byte
	var totalDur uint32
	err := pipeline.NewPCM().Run(src, nil, func(f pipeline.Frame) error {
		le := make([]byte, len(f.Payload))
		for i := 0; i+1 < len(f.Payload); i += 2 {
			binary.LittleEndian.PutUint16(le[i:i+2], binary.BigEndian.Uint16(f.Payload[i:i+2]))
		}
		got = append(got, le...)
		totalDur += f.Duration
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := append(append([]byte{}, p0...), p1...)
	if !bytes.Equal(got, want) {
		t.Error("reassembled PCM differs from source")
	}
	if totalDur != uint32(2*periodFrames) {
		t.Errorf("duration sum = %d, want %d", totalDur, 2*periodFrames)
	}
}

func TestPCMStagePayloadCap(t *testing.T) {
	t.Parallel()
	const rate, ch = 384000, 2
	frameBytes := 2 * ch
	period := make([]byte, 8000*frameBytes) // 32000 bytes, above the 15360 cap
	src := audio.NewFakeSource(rate, ch, [][]byte{period})

	count := 0
	err := pipeline.NewPCM().Run(src, nil, func(f pipeline.Frame) error {
		count++
		if len(f.Payload)%frameBytes != 0 {
			t.Errorf("payload len %d not frame-aligned", len(f.Payload))
		}
		if len(f.Payload) > 15360 {
			t.Errorf("payload len %d exceeds cap 15360", len(f.Payload))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count < 2 {
		t.Errorf("expected the oversized period to split into multiple payloads, got %d", count)
	}
}

func TestOpusStageFraming(t *testing.T) {
	t.Parallel()
	const rate, ch = 48000, 1
	// Four 480-sample periods => 1920 samples => two 960-sample Opus frames.
	periods := make([][]byte, 4)
	for k := range periods {
		b := make([]byte, 480*2)
		for i := range 480 {
			v := int16(8000 * math.Sin(2*math.Pi*440*float64(k*480+i)/rate))
			binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
		}
		periods[k] = b
	}
	src := audio.NewFakeSource(rate, ch, periods)

	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pcm := make([]int16, opusFrameSamplesTest)
	frames := 0
	err = pipeline.NewOpus(config.Opus{Bitrate: 64000}).Run(src, nil, func(f pipeline.Frame) error {
		frames++
		if f.Duration != 960 {
			t.Errorf("frame %d duration = %d, want 960", frames, f.Duration)
		}
		n, derr := dec.Decode(f.Payload, pcm)
		if derr != nil {
			t.Fatalf("Decode: %v", derr)
		}
		if n != 960 {
			t.Errorf("decoded %d samples, want 960", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if frames != 2 {
		t.Errorf("emitted %d frames, want 2", frames)
	}
}

func TestOpusStageStereo(t *testing.T) {
	t.Parallel()
	const rate, ch = 48000, 2
	// Four 480-frame stereo periods => 1920 frames => two 960-frame Opus frames.
	// Each frame carries opusFrameSamplesTest*ch interleaved samples.
	periods := make([][]byte, 4)
	for k := range periods {
		b := make([]byte, 480*ch*2) // 480 frames, 2 channels, S16LE
		for i := range 480 {
			// Distinct tones per channel (left louder than right); the per-channel
			// energy asserted after the loop pins interleaved stereo, not a doubled
			// mono, and catches a dropped or swapped channel.
			l := int16(8000 * math.Sin(2*math.Pi*440*float64(k*480+i)/rate))
			r := int16(6000 * math.Sin(2*math.Pi*660*float64(k*480+i)/rate))
			binary.LittleEndian.PutUint16(b[(i*ch+0)*2:], uint16(l))
			binary.LittleEndian.PutUint16(b[(i*ch+1)*2:], uint16(r))
		}
		periods[k] = b
	}
	src := audio.NewFakeSource(rate, ch, periods)

	dec, err := opus.NewDecoder(48000, ch)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pcm := make([]int16, opusFrameSamplesTest*ch)
	frames := 0
	var sumL2, sumR2 float64 // decoded per-channel energy (left vs right)
	err = pipeline.NewOpus(config.Opus{Bitrate: 96000}).Run(src, nil, func(f pipeline.Frame) error {
		frames++
		if f.Duration != 960 {
			t.Errorf("frame %d duration = %d, want 960", frames, f.Duration)
		}
		n, derr := dec.Decode(f.Payload, pcm)
		if derr != nil {
			t.Fatalf("Decode: %v", derr)
		}
		if n != 960 {
			t.Errorf("decoded %d samples per channel, want 960", n)
		}
		for i := 0; i < n; i++ {
			l, r := float64(pcm[i*2]), float64(pcm[i*2+1])
			sumL2 += l * l
			sumR2 += r * r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if frames != 2 {
		t.Errorf("emitted %d frames, want 2", frames)
	}
	// The channels carry different tones (left ~8000, right ~6000), so a correct
	// interleaved-stereo encode decodes to two non-trivial channels with the left
	// clearly louder. A mono-collapsed encode (identical channels), a dropped
	// channel, or a swapped pair would fail left energy > right energy > 0.
	if sumR2 <= 0 {
		t.Errorf("right channel is silent (sumR2=%v): channel dropped or collapsed", sumR2)
	}
	if sumL2 <= sumR2 {
		t.Errorf("left energy %v should exceed right %v: interleaved stereo not preserved", sumL2, sumR2)
	}
}

func TestOpusStageRejectsTooManyChannels(t *testing.T) {
	t.Parallel()
	// The config layer already forbids more than two Opus channels; the stage
	// guards independently, so a 3-channel source is rejected, not encoded.
	src := audio.NewFakeSource(48000, 3, [][]byte{make([]byte, 960*3*2)})
	err := pipeline.NewOpus(config.Opus{Bitrate: 96000}).Run(src, nil, func(pipeline.Frame) error { return nil })
	if err == nil {
		t.Fatal("Run accepted a 3-channel source, want an error")
	}
	// Pin the stage's own guard rather than only go-opus rejecting 3 channels: the
	// guard names the 1-or-2 constraint, which the encoder-construction error does not.
	if !strings.Contains(err.Error(), "1 or 2 channels") {
		t.Errorf("error %q should name the stage's 1-or-2-channel guard", err)
	}
}

const opusFrameSamplesTest = 960

// gateStep is one gate answer: whether a client plays, and its session.
type gateStep struct {
	on      bool
	session uint64
}

// gateSteps returns a gate that answers from steps, one entry per call, and a
// pointer to the call count. A call past the steps fails the test: the Stage
// contract is exactly one gate call per period read.
func gateSteps(t *testing.T, steps ...gateStep) (gate pipeline.Gate, calls *int) {
	t.Helper()
	n := 0
	return func() (bool, uint64) {
		if n >= len(steps) {
			t.Errorf("gate called %d times, want one call per period (%d)", n+1, len(steps))
			return false, 0
		}
		s := steps[n]
		n++
		return s.on, s.session
	}, &n
}

// gateSeq returns a gate that answers from pattern as a real feed would: each
// run of true is one play session, numbered from 1 as ChanSource numbers them.
func gateSeq(t *testing.T, pattern ...bool) (gate pipeline.Gate, calls *int) {
	t.Helper()
	steps := make([]gateStep, len(pattern))
	var session uint64
	for i, on := range pattern {
		if on && (i == 0 || !pattern[i-1]) {
			session++
		}
		steps[i] = gateStep{on: on, session: session}
	}
	return gateSteps(t, steps...)
}

// tonePCM returns the given number of frames of interleaved S16LE PCM at 48
// kHz with ch channels, a different sweeping tone per channel, so a dropped,
// shifted or swapped sample changes the encoded packets.
func tonePCM(frames, ch int) []byte {
	b := make([]byte, frames*ch*2)
	for i := range frames {
		n := float64(i)
		for c := range ch {
			v := int16(7000 * math.Sin(2*math.Pi*(300+150*float64(c)+n/40)*n/48000))
			binary.LittleEndian.PutUint16(b[(i*ch+c)*2:], uint16(v))
		}
	}
	return b
}

// splitPeriods cuts pcm (ch channels) into periods holding the given number of
// frames each; a short tail becomes the last period.
func splitPeriods(pcm []byte, frames, ch int) [][]byte {
	size := frames * ch * 2
	var out [][]byte
	for len(pcm) > 0 {
		n := min(size, len(pcm))
		out = append(out, pcm[:n])
		pcm = pcm[n:]
	}
	return out
}

// referenceOpus encodes pcm with a freshly built encoder in consecutive whole
// 960-frame chunks, dropping a partial tail, exactly as a stage fed the same
// audio must.
func referenceOpus(t *testing.T, cfg config.Opus, pcm []byte, ch int) [][]byte {
	t.Helper()
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: ch, Bitrate: cfg.EffectiveBitrate(ch)})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	frame := make([]int16, opusFrameSamplesTest*ch)
	buf := make([]byte, 4000)
	chunk := opusFrameSamplesTest * ch * 2
	out := make([][]byte, 0, len(pcm)/chunk)
	for ; len(pcm) >= chunk; pcm = pcm[chunk:] {
		for i := range frame {
			frame[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		}
		n, err := enc.Encode(frame, buf)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		out = append(out, append([]byte(nil), buf[:n]...))
	}
	return out
}

// runOpus runs an Opus stage over periods and returns copies of the payloads.
func runOpus(t *testing.T, cfg config.Opus, periods [][]byte, ch int, gate pipeline.Gate) [][]byte {
	t.Helper()
	var out [][]byte
	err := pipeline.NewOpus(cfg).Run(audio.NewFakeSource(48000, ch, periods), gate, func(f pipeline.Frame) error {
		out = append(out, append([]byte(nil), f.Payload...))
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

// TestOpusStageArbitraryPeriodSizes pins the accumulate loop on the periods
// real capture delivers, whose size the driver negotiates: a period can end
// mid-frame, carry the rest of one frame and the start of the next, or hold
// several whole frames. For each size the stage must emit exactly what a fresh
// encoder emits for the same audio cut into 960-frame chunks.
func TestOpusStageArbitraryPeriodSizes(t *testing.T) {
	t.Parallel()
	cfg := config.Opus{Bitrate: 64000}
	for _, ch := range []int{1, 2} {
		for _, frames := range []int{441, 1024, 1500, 2881} {
			pcm := tonePCM(6*frames, ch)
			want := referenceOpus(t, cfg, pcm, ch)
			got := runOpus(t, cfg, splitPeriods(pcm, frames, ch), ch, nil)
			if len(got) != len(want) {
				t.Errorf("%d ch, %d-frame periods: emitted %d frames, want %d", ch, frames, len(got), len(want))
				continue
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Errorf("%d ch, %d-frame periods: frame %d differs from a fresh encoder's", ch, frames, i)
					break
				}
			}
		}
	}
}

// TestOpusStageStartsIdleThenPlays pins the usual path for a real client: the
// stream starts with no client, so the first periods are drained unencoded, and
// once a client plays the stage emits exactly what a fresh encoder emits for the
// audio from that point on. Stereo, with periods that straddle frame
// boundaries, so the channel interleave survives the idle stretch too.
func TestOpusStageStartsIdleThenPlays(t *testing.T) {
	t.Parallel()
	const ch, frames = 2, 1024
	cfg := config.Opus{Bitrate: 96000}
	periods := splitPeriods(tonePCM(8*frames, ch), frames, ch)
	active, calls := gateSeq(t, false, false, true, true, true, true, true, true)

	got := runOpus(t, cfg, periods, ch, active)
	if *calls != len(periods) {
		t.Errorf("active gate called %d times, want %d (once per period)", *calls, len(periods))
	}
	var played []byte
	for _, p := range periods[2:] {
		played = append(played, p...)
	}
	want := referenceOpus(t, cfg, played, ch)
	if len(got) != len(want) {
		t.Fatalf("emitted %d frames, want %d (only the audio after the client started)", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d after the client started differs from a fresh encoder's", i)
		}
	}
}

// TestPCMStageSkipsInactivePeriods pins the idle gate on the L16 path: a period
// read while the gate is closed is drained (the source still reaches EOF) but
// emits nothing, and the periods read while it is open pass through intact.
func TestPCMStageSkipsInactivePeriods(t *testing.T) {
	t.Parallel()
	const rate, ch = 48000, 1
	periods := make([][]byte, 4)
	for k := range periods {
		b := make([]byte, 960*2)
		for i := range 960 {
			binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(k*1000+i)))
		}
		periods[k] = b
	}
	active, calls := gateSeq(t, false, true, false, true)

	var got []byte
	err := pipeline.NewPCM().Run(audio.NewFakeSource(rate, ch, periods), active, func(f pipeline.Frame) error {
		for i := 0; i+1 < len(f.Payload); i += 2 {
			got = binary.LittleEndian.AppendUint16(got, binary.BigEndian.Uint16(f.Payload[i:i+2]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if *calls != len(periods) {
		t.Errorf("active gate called %d times, want %d (once per period)", *calls, len(periods))
	}
	want := append(append([]byte{}, periods[1]...), periods[3]...)
	if !bytes.Equal(got, want) {
		t.Errorf("emitted %d bytes, want exactly the two active periods (%d bytes)", len(got), len(want))
	}
}

// TestOpusStageIdleEmitsNothing pins that a stream nobody plays encodes nothing:
// with the gate closed throughout, every period is drained and no frame is
// emitted.
func TestOpusStageIdleEmitsNothing(t *testing.T) {
	t.Parallel()
	periods := splitPeriods(tonePCM(8*480, 1), 480, 1)
	active, calls := gateSeq(t, false, false, false, false, false, false, false, false)
	frames := 0
	err := pipeline.NewOpus(config.Opus{Bitrate: 64000}).Run(audio.NewFakeSource(48000, 1, periods), active, func(pipeline.Frame) error {
		frames++
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if frames != 0 {
		t.Errorf("emitted %d frames with the gate closed, want 0", frames)
	}
	if *calls != len(periods) {
		t.Errorf("active gate called %d times, want %d (every period drained)", *calls, len(periods))
	}
}

// TestStagesIgnoreEmptyPeriodWhileActive pins what an active stage does with
// an empty period (a zero-frame read): nothing. The fan-out no longer sends
// one, but a Source may still return one, and a stage must not treat it as a
// frame boundary. Each stage's output must equal its output for the real
// periods alone.
func TestStagesIgnoreEmptyPeriodWhileActive(t *testing.T) {
	t.Parallel()
	t.Run("pcm", func(t *testing.T) {
		t.Parallel()
		const rate, ch, frames = 48000, 1, 960
		audioPeriods := splitPeriods(tonePCM(2*frames, ch), frames, ch)
		periods := [][]byte{audioPeriods[0], nil, audioPeriods[1]}
		var got []byte
		var dur uint32
		err := pipeline.NewPCM().Run(audio.NewFakeSource(rate, ch, periods), nil, func(f pipeline.Frame) error {
			if len(f.Payload) == 0 || f.Duration == 0 {
				t.Errorf("got an empty frame (%d bytes, duration %d), want none", len(f.Payload), f.Duration)
			}
			for i := 0; i+1 < len(f.Payload); i += 2 {
				got = binary.LittleEndian.AppendUint16(got, binary.BigEndian.Uint16(f.Payload[i:]))
			}
			dur += f.Duration
			return nil
		})
		if err != nil {
			t.Fatalf("Run: got error %v, want none", err)
		}
		if want := append(append([]byte{}, audioPeriods[0]...), audioPeriods[1]...); !bytes.Equal(got, want) {
			t.Errorf("got %d bytes of PCM, want exactly the %d bytes of the real periods", len(got), len(want))
		}
		if dur != 2*frames {
			t.Errorf("got total duration %d, want %d", dur, 2*frames)
		}
	})
	t.Run("opus", func(t *testing.T) {
		t.Parallel()
		// 480-sample periods, two per 960-sample frame, with an empty period in
		// the middle of each frame: a stage that treated it as a boundary would
		// encode a partial frame.
		cfg := config.Opus{Bitrate: 64000}
		audioPeriods := splitPeriods(tonePCM(4*480, 1), 480, 1)
		periods := [][]byte{audioPeriods[0], nil, audioPeriods[1], audioPeriods[2], nil, audioPeriods[3]}
		got := runOpus(t, cfg, periods, 1, nil)
		var all []byte
		for _, p := range audioPeriods {
			all = append(all, p...)
		}
		want := referenceOpus(t, cfg, all, 1)
		if len(got) != len(want) {
			t.Fatalf("got %d frames, want %d (the real periods alone)", len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Errorf("frame %d differs from the frame for the real periods alone", i)
			}
		}
	})
}

// TestOpusStageResumesWithFreshEncoder pins the resume contract: after an idle
// stretch, the stage emits exactly what a freshly built encoder emits for the
// same post-activation PCM. That fails if the encoder is not reset (its history
// from before the gap shapes the packets) or if the partial frame accumulated
// before the gap is kept (the frame boundaries shift).
func TestOpusStageResumesWithFreshEncoder(t *testing.T) {
	t.Parallel()
	// 480-sample periods: two make one 960-sample frame. Periods 0-2 are active
	// (one full frame plus a half frame left in the accumulator), 3-4 idle, and
	// 5-10 active again (three frames).
	periods := splitPeriods(tonePCM(11*480, 1), 480, 1)
	pattern := []bool{true, true, true, false, false, true, true, true, true, true, true}
	cfg := config.Opus{Bitrate: 64000}

	active, _ := gateSeq(t, pattern...)
	got := runOpus(t, cfg, periods, 1, active)
	if len(got) != 4 {
		t.Fatalf("emitted %d frames, want 4 (1 before the gap, 3 after)", len(got))
	}
	var resumed []byte
	for _, p := range periods[5:] {
		resumed = append(resumed, p...)
	}
	want := referenceOpus(t, cfg, resumed, 1)
	if len(want) != 3 {
		t.Fatalf("reference run emitted %d frames, want 3", len(want))
	}
	for i, w := range want {
		if !bytes.Equal(got[1+i], w) {
			t.Errorf("frame %d after resume differs from a fresh encoder's frame %d", i+1, i+1)
		}
	}
}

// TestOpusStageNewSessionWithoutIdleGetsFreshEncoder pins the splice case: a
// teardown and the next client's PLAY can both land between two period reads,
// so the stage never sees the stream idle, only the session change. The new
// client must still get exactly what a freshly built encoder emits for the
// audio from its first period, with neither the old client's encoder history
// nor its half-filled frame.
func TestOpusStageNewSessionWithoutIdleGetsFreshEncoder(t *testing.T) {
	t.Parallel()
	// 480-sample periods: two make one 960-sample frame. Periods 0-2 play in
	// session 1 (one full frame plus a half frame left in the accumulator) and
	// periods 3-8 in session 2 (three frames).
	periods := splitPeriods(tonePCM(9*480, 1), 480, 1)
	steps := make([]gateStep, len(periods))
	for i := range steps {
		steps[i] = gateStep{on: true, session: 1}
		if i >= 3 {
			steps[i].session = 2
		}
	}
	cfg := config.Opus{Bitrate: 64000}

	gate, calls := gateSteps(t, steps...)
	got := runOpus(t, cfg, periods, 1, gate)
	if *calls != len(periods) {
		t.Errorf("gate called %d times, want %d (once per period)", *calls, len(periods))
	}
	if len(got) != 4 {
		t.Fatalf("emitted %d frames, want 4 (1 in the first session, 3 in the second)", len(got))
	}
	var second []byte
	for _, p := range periods[3:] {
		second = append(second, p...)
	}
	want := referenceOpus(t, cfg, second, 1)
	for i, w := range want {
		if !bytes.Equal(got[1+i], w) {
			t.Errorf("frame %d of the second session differs from a fresh encoder's frame %d", i+1, i+1)
		}
	}
}

func TestSDPSpec(t *testing.T) {
	t.Parallel()
	pcm := pipeline.SDPSpec(&config.Stream{Mode: config.ModePCM}, "m", 256000, 1)
	if pcm.EncodingName != "L16" || pcm.ClockRate != 256000 || pcm.Channels != 1 || pcm.PayloadType != 96 || pcm.Ptime != 20 {
		t.Errorf("PCM spec unexpected: %+v", pcm)
	}

	op := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Opus: config.Opus{Bitrate: 64000}}, "m", 48000, 1)
	if op.EncodingName != "opus" || op.ClockRate != 48000 || op.Channels != 2 || op.PayloadType != 97 {
		t.Errorf("Opus spec unexpected: %+v", op)
	}
	if !strings.Contains(op.FMTP, "sprop-stereo=0") {
		t.Errorf("Opus fmtp missing sprop-stereo=0: %q", op.FMTP)
	}
	if !strings.Contains(op.FMTP, "maxaveragebitrate=64000") {
		t.Errorf("Opus fmtp missing maxaveragebitrate: %q", op.FMTP)
	}

	// A two-channel selection is signalled with sprop-stereo=1 while the rtpmap
	// stays opus/48000/2 (RFC 7587).
	stereo := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Channels: []int{1, 2}}, "s", 48000, 2)
	if stereo.Channels != 2 || !strings.Contains(stereo.FMTP, "sprop-stereo=1") {
		t.Errorf("stereo Opus spec should carry sprop-stereo=1: %+v", stereo)
	}
	if strings.Contains(stereo.FMTP, "sprop-stereo=0") {
		t.Errorf("stereo Opus fmtp must not contain sprop-stereo=0: %q", stereo.FMTP)
	}
}

// TestPayloadTypeAndCodecName pins the centralized mode mappings and, crucially,
// asserts that SDPSpec derives its payload type and encoding name from the same
// helpers the RTP writer wiring and mDNS advertisement use, so a DESCRIBE can
// never announce a payload type or codec the stream does not actually send.
func TestPayloadTypeAndCodecName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode    config.Mode
		payload int
		codec   string
	}{
		{config.ModePCM, 96, "L16"},
		{config.ModeOpus, 97, "opus"},
	} {
		if got := pipeline.PayloadType(tc.mode); got != tc.payload {
			t.Errorf("PayloadType(%q) = %d, want %d", tc.mode, got, tc.payload)
		}
		if got := pipeline.CodecName(tc.mode); got != tc.codec {
			t.Errorf("CodecName(%q) = %q, want %q", tc.mode, got, tc.codec)
		}
		spec := pipeline.SDPSpec(&config.Stream{Mode: tc.mode}, "m", 48000, 1)
		if spec.PayloadType != pipeline.PayloadType(tc.mode) {
			t.Errorf("SDPSpec payload %d != PayloadType %d for %q", spec.PayloadType, pipeline.PayloadType(tc.mode), tc.mode)
		}
		if spec.EncodingName != pipeline.CodecName(tc.mode) {
			t.Errorf("SDPSpec encoding %q != CodecName %q for %q", spec.EncodingName, pipeline.CodecName(tc.mode), tc.mode)
		}
	}
}

// TestSDPSpecOpusDefaultBitrate asserts a stream with no configured bitrate
// advertises the per-channel default (128 kbps per channel) rather than none.
func TestSDPSpecOpusDefaultBitrate(t *testing.T) {
	t.Parallel()
	mono := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Channels: []int{1}}, "m", 48000, 1)
	if !strings.Contains(mono.FMTP, "maxaveragebitrate=128000") {
		t.Errorf("mono fmtp = %q, want maxaveragebitrate=128000", mono.FMTP)
	}
	stereo := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Channels: []int{1, 2}}, "m", 48000, 2)
	if !strings.Contains(stereo.FMTP, "maxaveragebitrate=256000") {
		t.Errorf("stereo fmtp = %q, want maxaveragebitrate=256000", stereo.FMTP)
	}
}
