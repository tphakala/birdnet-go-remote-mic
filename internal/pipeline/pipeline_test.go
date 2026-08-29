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

const opusFrameSamplesTest = 960

func TestSDPSpec(t *testing.T) {
	pcm := pipeline.SDPSpec(&config.Device{Name: "m", Mode: config.ModePCM}, 256000, 1)
	if pcm.EncodingName != "L16" || pcm.ClockRate != 256000 || pcm.Channels != 1 || pcm.PayloadType != 96 || pcm.Ptime != 20 {
		t.Errorf("PCM spec unexpected: %+v", pcm)
	}

	op := pipeline.SDPSpec(&config.Device{Name: "m", Mode: config.ModeOpus, Opus: config.Opus{Bitrate: 64000}}, 48000, 1)
	if op.EncodingName != "opus" || op.ClockRate != 48000 || op.Channels != 2 || op.PayloadType != 97 {
		t.Errorf("Opus spec unexpected: %+v", op)
	}
	if !strings.Contains(op.FMTP, "sprop-stereo=0") {
		t.Errorf("Opus fmtp missing sprop-stereo=0: %q", op.FMTP)
	}
	if !strings.Contains(op.FMTP, "maxaveragebitrate=64000") {
		t.Errorf("Opus fmtp missing maxaveragebitrate: %q", op.FMTP)
	}
}
