// Package pipeline turns capture periods into RTP payload frames: L16
// passthrough for the ultrasonic path, or Opus encode for normal audio.
package pipeline

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/tphakala/go-audio-stream/packet/l16"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
	"github.com/tphakala/go-opus/opus"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// Frame is one RTP payload worth of media plus its duration in RTP ticks.
// Payload is only valid during the emit callback (the stage reuses its
// buffers); a consumer that retains it must copy it.
type Frame struct {
	Payload  []byte
	Duration uint32 // RTP timestamp increment: PCM = frames in this payload; Opus = 960
	// Captured is the wall clock when this media left the capture device. The
	// RTCP sender report maps this (not send time) to the RTP timestamp, so TCP
	// backpressure never skews the receiver's clock recovery.
	Captured time.Time
}

// Stage consumes capture periods from src and emits Frames until src ends
// (io.EOF, returned as nil) or emit returns an error.
type Stage interface {
	Run(src audio.Source, emit func(Frame) error) error
}

// maxL16Payload caps an L16 RTP payload at 15360 bytes (20 ms of mono 384 kHz).
// The hard protocol ceiling is higher (65523: the interleaved frame length
// field covers the RTP packet, minus its 12-byte header), so this is a pacing
// choice, not a limit.
const maxL16Payload = 15360

// opusFrameSamples is one 20 ms Opus frame at 48 kHz.
const opusFrameSamples = 960

type pcmStage struct {
	channels int
}

// NewPCM returns an L16 passthrough stage that byte-swaps little-endian capture
// PCM into big-endian L16 RTP payloads.
func NewPCM(channels int) Stage { return &pcmStage{channels: channels} }

func (p *pcmStage) Run(src audio.Source, emit func(Frame) error) error {
	rate, ch := src.Negotiated()
	frameBytes := 2 * ch
	maxBytes := (rate / 50) * frameBytes // 20 ms
	if maxBytes > maxL16Payload {
		maxBytes = maxL16Payload
	}
	if maxBytes < frameBytes {
		maxBytes = frameBytes
	}
	pk := &l16.Packetizer{Channels: ch, MaxBytes: maxBytes}

	for {
		period, err := src.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		captured := time.Now()
		if _, err := pk.Split(period.Buf, func(payload []byte) error {
			return emit(Frame{
				Payload:  payload,
				Duration: uint32(len(payload) / frameBytes),
				Captured: captured,
			})
		}); err != nil {
			return err
		}
	}
}

type opusStage struct {
	bitrate int
}

// NewOpus returns an Opus encode stage. It requires 48 kHz capture with one or
// two channels (mono or stereo) and emits one Opus packet per 20 ms frame (960
// samples per channel); a trailing partial frame at teardown is dropped.
func NewOpus(cfg config.Opus) Stage { return &opusStage{bitrate: cfg.Bitrate} }

func (o *opusStage) Run(src audio.Source, emit func(Frame) error) error {
	rate, ch := src.Negotiated()
	if rate != 48000 || ch < 1 || ch > 2 {
		return fmt.Errorf("pipeline: opus requires 48000 Hz with 1 or 2 channels, got %d Hz %d ch", rate, ch)
	}
	bitrate := config.Opus{Bitrate: o.bitrate}.EffectiveBitrate(ch)
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: ch, Bitrate: bitrate})
	if err != nil {
		return err
	}

	// One 20 ms Opus frame is opusFrameSamples per channel of interleaved PCM, so
	// the accumulator fills to opusFrameSamples*ch before each Encode (mono is the
	// ch==1 case). The RTP timestamp advances by opusFrameSamples (960) per frame:
	// the Opus RTP clock counts samples of a single channel at 48 kHz (RFC 7587),
	// so the per-frame increment is 960 regardless of the channel count.
	frameSamples := opusFrameSamples * ch // interleaved int16 per 20 ms frame
	acc := make([]int16, 0, frameSamples) // reused accumulator
	encBuf := make([]byte, 4000)          // one Opus packet fits easily

	for {
		period, err := src.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		captured := time.Now()
		samples := len(period.Buf) / 2
		for i := range samples {
			acc = append(acc, int16(binary.LittleEndian.Uint16(period.Buf[i*2:])))
			if len(acc) < frameSamples {
				continue
			}
			n, eerr := enc.Encode(acc, encBuf)
			if eerr != nil {
				return eerr
			}
			if err := emit(Frame{Payload: encBuf[:n], Duration: opusFrameSamples, Captured: captured}); err != nil {
				return err
			}
			acc = acc[:0]
		}
	}
}

// PayloadType returns the dynamic RTP payload type advertised for a stream mode:
// 97 for Opus, 96 for PCM L16. It is the single source of truth shared by the
// RTP writer wiring, the SDP, and the mDNS advertisement so the payload type
// cannot drift between what DESCRIBE announces and what the writer sends.
func PayloadType(mode config.Mode) int {
	if mode == config.ModeOpus {
		return 97
	}
	return 96
}

// CodecName returns the codec token for a stream mode: "opus" or "L16". It is
// the SDP encoding name and the mDNS TXT codec value, kept in one place so the
// two cannot describe the stream differently.
func CodecName(mode config.Mode) string {
	if mode == config.ModeOpus {
		return "opus"
	}
	return "L16"
}

// SDPSpec builds the SDP write spec the server serializes at DESCRIBE time for
// one stream. name is the device's DNS-SD/session name; rate and channels are
// the stream's own values (the selected channel count feeds the L16 rtpmap).
// Opus is always advertised as opus/48000/2 per RFC 7587, with sprop-stereo
// reflecting the selection: 1 for a two-channel (stereo) stream, 0 for mono.
func SDPSpec(s *config.Stream, name string, rate, channels int) sdp.WriteSpec {
	if s.Mode == config.ModeOpus {
		fmtp := "sprop-stereo=0"
		if len(s.Channels) == 2 {
			fmtp = "sprop-stereo=1"
		}
		fmtp += ";maxaveragebitrate=" + strconv.Itoa(s.Opus.EffectiveBitrate(len(s.Channels)))
		return sdp.WriteSpec{
			Name:         name,
			PayloadType:  PayloadType(s.Mode),
			EncodingName: CodecName(s.Mode),
			ClockRate:    48000,
			Channels:     2,
			Control:      "trackID=0",
			FMTP:         fmtp,
		}
	}
	return sdp.WriteSpec{
		Name:         name,
		PayloadType:  PayloadType(s.Mode),
		EncodingName: CodecName(s.Mode),
		ClockRate:    rate,
		Channels:     channels,
		Control:      "trackID=0",
		Ptime:        20,
	}
}
