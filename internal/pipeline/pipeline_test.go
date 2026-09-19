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
	err := pipeline.NewPCM(ch).Run(src, func(f pipeline.Frame) error {
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
	const rate, ch = 384000, 2
	frameBytes := 2 * ch
	period := make([]byte, 8000*frameBytes) // 32000 bytes, above the 15360 cap
	src := audio.NewFakeSource(rate, ch, [][]byte{period})

	count := 0
	err := pipeline.NewPCM(ch).Run(src, func(f pipeline.Frame) error {
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
	err = pipeline.NewOpus(config.Opus{Bitrate: 64000}).Run(src, func(f pipeline.Frame) error {
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
	err = pipeline.NewOpus(config.Opus{Bitrate: 96000}).Run(src, func(f pipeline.Frame) error {
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
	// The config layer already forbids more than two Opus channels; the stage
	// guards independently, so a 3-channel source is rejected, not encoded.
	src := audio.NewFakeSource(48000, 3, [][]byte{make([]byte, 960*3*2)})
	err := pipeline.NewOpus(config.Opus{Bitrate: 96000}).Run(src, func(pipeline.Frame) error { return nil })
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

func TestSDPSpec(t *testing.T) {
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
	mono := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Channels: []int{1}}, "m", 48000, 1)
	if !strings.Contains(mono.FMTP, "maxaveragebitrate=128000") {
		t.Errorf("mono fmtp = %q, want maxaveragebitrate=128000", mono.FMTP)
	}
	stereo := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Channels: []int{1, 2}}, "m", 48000, 2)
	if !strings.Contains(stereo.FMTP, "maxaveragebitrate=256000") {
		t.Errorf("stereo fmtp = %q, want maxaveragebitrate=256000", stereo.FMTP)
	}
}
