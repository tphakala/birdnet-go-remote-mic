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
//
// gate gates the work: it is consulted once per period, right after the period
// is read, and a period read while it reports inactive is drained but not
// packetized or encoded, since its frames would be discarded downstream anyway.
// A stateful stage (Opus) starts from a clean state whenever the session
// changes, so each client's frames come from an encoder with no history or
// partial frame from an earlier client, even when a teardown and the next PLAY
// both land between two period reads and the stage never sees the stream idle.
// The reset is per period read, so two edges remain: a frame the stage is
// encoding while a teardown and the next PLAY both complete is still delivered
// to the new client, and periods already queued for a stage that had fallen
// behind are encoded for whichever session is active when it reads them.
// A nil gate means always active, in one session.
type Stage interface {
	Run(src audio.Source, gate Gate, emit func(Frame) error) error
}

// Gate reports whether a client is playing the stream a stage feeds, and which
// play session it is (rtspserver.ChanSource.Session). The session changes on
// every activation, so a stage comparing it with the session it last encoded
// for sees each new client.
type Gate func() (active bool, session uint64)

// open consults gate for the period just read; a nil gate is always open, in
// session 0.
func (g Gate) open() (active bool, session uint64) {
	if g == nil {
		return true, 0
	}
	return g()
}

// maxL16Payload caps an L16 RTP payload at 15360 bytes (20 ms of mono 384 kHz).
// The hard protocol ceiling is higher (65523: the interleaved frame length
// field covers the RTP packet, minus its 12-byte header), so this is a pacing
// choice, not a limit.
const maxL16Payload = 15360

// opusFrameSamples is one 20 ms Opus frame at 48 kHz.
const opusFrameSamples = 960

type pcmStage struct{}

// NewPCM returns an L16 passthrough stage that byte-swaps little-endian capture
// PCM into big-endian L16 RTP payloads. It takes the channel count from its
// source (Negotiated), so one stage serves any channel selection.
func NewPCM() Stage { return pcmStage{} }

func (pcmStage) Run(src audio.Source, gate Gate, emit func(Frame) error) error {
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
		// The packetizer keeps no stream state between periods, so a new session
		// needs no reset.
		if on, _ := gate.open(); !on {
			continue
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

// opusEncoder is the part of *opus.Encoder a stage uses, so a test can count
// the encoder's work rather than infer it from the emitted frames.
type opusEncoder interface {
	Encode(pcm []int16, buf []byte) (int, error)
	Reset()
}

type opusStage struct {
	bitrate int
	// newEncoder builds the stage's encoder; nil means newOpusEncoder.
	newEncoder func(opus.EncoderConfig) (opusEncoder, error)
}

// newOpusEncoder builds the go-opus encoder a production stage runs.
func newOpusEncoder(cfg opus.EncoderConfig) (opusEncoder, error) {
	return opus.NewEncoder(cfg)
}

// NewOpus returns an Opus encode stage. It requires 48 kHz capture with one or
// two channels (mono or stereo) and emits one Opus packet per 20 ms frame (960
// samples per channel); a trailing partial frame at teardown is dropped.
func NewOpus(cfg config.Opus) Stage { return &opusStage{bitrate: cfg.Bitrate} }

func (o *opusStage) Run(src audio.Source, gate Gate, emit func(Frame) error) error {
	rate, ch := src.Negotiated()
	if rate != 48000 || ch < 1 || ch > 2 {
		return fmt.Errorf("pipeline: opus requires 48000 Hz with 1 or 2 channels, got %d Hz %d ch", rate, ch)
	}
	bitrate := config.Opus{Bitrate: o.bitrate}.EffectiveBitrate(ch)
	newEncoder := o.newEncoder
	if newEncoder == nil {
		newEncoder = newOpusEncoder
	}
	// The encoder is built up front even though a stream usually starts with no
	// client, so a configuration the encoder rejects fails the stream at open
	// rather than later, at some client's PLAY.
	enc, err := newEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: ch, Bitrate: bitrate})
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
	// session is the play session the encoder state belongs to. A period of any
	// other session resets the encoder and drops the stale partial frame, so a
	// new client's stream starts exactly as a freshly built encoder's would. It
	// starts at 0, which a feed never reports while active, so the first client
	// resets the fresh encoder too; that is harmless. (A nil gate stays in
	// session 0 and never resets.)
	var session uint64

	for {
		period, err := src.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		on, s := gate.open()
		if !on {
			continue
		}
		if s != session {
			enc.Reset()
			acc = acc[:0]
			session = s
		}
		captured := time.Now()
		// Fill the accumulator up to the rest of the current frame per pass, so the
		// per-sample loop carries no frame-boundary branch.
		pcm := period.Buf
		for len(pcm) >= 2 {
			take := min(frameSamples-len(acc), len(pcm)/2)
			start := len(acc)
			acc = acc[:start+take]
			dst := acc[start:]
			for i := range dst {
				dst[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
			}
			pcm = pcm[take*2:]
			if len(acc) < frameSamples {
				break
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
