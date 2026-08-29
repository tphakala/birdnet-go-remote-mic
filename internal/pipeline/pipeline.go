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

// NewOpus returns an Opus encode stage. It requires 48 kHz mono capture and
// emits one Opus packet per 960-sample frame; a trailing partial frame at
// teardown is dropped.
func NewOpus(cfg config.Opus) Stage { return &opusStage{bitrate: cfg.Bitrate} }

func (o *opusStage) Run(src audio.Source, emit func(Frame) error) error {
	rate, ch := src.Negotiated()
	if rate != 48000 || ch != 1 {
		return fmt.Errorf("pipeline: opus requires 48000 Hz mono, got %d Hz %d ch", rate, ch)
	}
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: 1, Bitrate: o.bitrate})
	if err != nil {
		return err
	}

	acc := make([]int16, 0, opusFrameSamples) // reused accumulator
	encBuf := make([]byte, 4000)              // one Opus packet fits easily

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
			if len(acc) < opusFrameSamples {
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

// SDPSpec builds the SDP write spec the server serializes at DESCRIBE time. rate
// and channels are the negotiated capture values (used for the L16 rtpmap);
// Opus is always advertised as opus/48000/2 per RFC 7587 with sprop-stereo=0 for
// the mono source.
func SDPSpec(cfg *config.Config, rate, channels int) sdp.WriteSpec {
	if cfg.Mode == config.ModeOpus {
		fmtp := "sprop-stereo=0"
		if cfg.Opus.Bitrate > 0 {
			fmtp += ";maxaveragebitrate=" + strconv.Itoa(cfg.Opus.Bitrate)
		}
		return sdp.WriteSpec{
			Name:         cfg.Name,
			PayloadType:  97,
			EncodingName: "opus",
			ClockRate:    48000,
			Channels:     2,
			Control:      "trackID=0",
			FMTP:         fmtp,
		}
	}
	return sdp.WriteSpec{
		Name:         cfg.Name,
		PayloadType:  96,
		EncodingName: "L16",
		ClockRate:    rate,
		Channels:     channels,
		Control:      "trackID=0",
		Ptime:        20,
	}
}
