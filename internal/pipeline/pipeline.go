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
	Duration uint32 // payload length in RTP ticks: PCM = frames in this payload; Opus = 960
	// Pos is the stream position (audio.Period.Pos) of the payload's first
	// sample, in RTP ticks (frames at the capture rate, which is the RTP clock in
	// both modes). The writer derives the RTP timestamp from it, so audio lost
	// upstream or in a subscriber's queue leaves a timestamp gap of exactly its
	// length.
	Pos uint64
	// Captured is the wall clock when this payload's first sample was captured,
	// back-dated from its period's stamp (which marks the period's end) by
	// sample position. That is the instant the packet's RTP timestamp names,
	// and the RTCP sender report pairs the two (not send time), so TCP
	// backpressure never skews the receiver's clock recovery.
	Captured time.Time
	// Session is the play session the stage produced this frame for (see
	// Gate), so the feed can drop a frame that a teardown and the next PLAY
	// overtook while it was being encoded. Zero means untagged (a nil gate fed
	// untagged periods).
	Session uint64
}

// Stage consumes capture periods from src and emits Frames until src ends
// (io.EOF, returned as nil) or emit returns an error.
//
// gate gates the work: it is consulted once per period, right after the period
// is read, and a period read while it reports inactive is drained but not
// packetized or encoded, since its frames would be discarded downstream anyway.
// A stateful stage (Opus) starts from a clean state whenever the session
// changes, so a stretch of playing gets an encoder with no history or partial
// frame from an earlier stretch, even when the last client's teardown and the
// next stretch's first PLAY both land between two period reads and the stage
// never sees the stream idle. The session advances only when the first client
// joins an idle stream: a client joining or leaving while others play leaves it
// alone, so the running encoder is never touched and every client shares one
// encode.
// A period tagged with another session than the gate reports (audio.Period.
// Session, set by the fan-out) was queued for an earlier stretch of playing and
// is dropped unencoded, and every frame carries the session it was produced for
// (Frame.Session), so the feed can drop one that a teardown and the next PLAY
// overtook while it was being encoded (rtspserver.Feed checks it on Push). A
// frame is stamped with the capture time of its first sample, derived from its
// period's capture time (audio.Period.Captured) when the period carries one; an
// Opus frame that spans periods takes the stamp of the period holding its first
// sample.
// Every frame also carries the stream position of its first sample
// (audio.Period.Pos), so audio the fan-out dropped before this stage shows as a
// gap in Pos; an Opus frame never spans such a gap, and the partial frame
// before it is discarded.
// A nil gate means always active: every period is encoded, in the session it
// carries (zero for an untagged one), so an ungated stage drops no period.
type Stage interface {
	Run(src audio.Source, gate Gate, emit func(Frame) error) error
}

// Gate reports whether any client is playing the stream a stage feeds, and which
// play session it is (rtspserver.Feed.Session). The session is per stretch with
// at least one client playing: it advances when the first client joins an idle
// stream, so a stage comparing it with the session it last encoded for sees
// each new stretch.
type Gate func() (active bool, session uint64)

// admit reports whether a period read now is to be encoded, and the play
// session it is encoded for. A period is dropped while the gate is closed, and
// when it was queued for another session than the gate reports (an untagged
// period, Session zero, belongs to any). A nil gate is always open and admits
// every period in the session it carries, so a stage given no gate encodes
// whatever it is fed, tagged or not. captured is the period's capture time
// (the end of the period), or now when the period carries none.
func (g Gate) admit(p *audio.Period) (ok bool, session uint64, captured time.Time) {
	if g == nil {
		session = p.Session
	} else {
		var on bool
		on, session = g()
		if !on || (p.Session != 0 && p.Session != session) {
			return false, 0, time.Time{}
		}
	}
	captured = p.Captured
	if captured.IsZero() {
		captured = time.Now()
	}
	return true, session, captured
}

// maxL16Payload caps an L16 RTP payload at 15360 bytes (20 ms of mono 384 kHz).
// The hard protocol ceiling is higher (65523: the interleaved frame length
// field covers the RTP packet, minus its 12-byte header), so this is a pacing
// choice, not a limit.
const maxL16Payload = 15360

// opusFrameSamples is one 20 ms Opus frame at 48 kHz.
const opusFrameSamples = 960

// framesDur is the duration of frames sample instants at rate Hz. It returns 0
// for a non-positive rate, so a source that misreports its rate costs only the
// back-dating rather than a divide-by-zero panic on the capture goroutine.
func framesDur(frames, rate int) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(frames) * time.Second / time.Duration(rate)
}

type pcmStage struct{}

// NewPCM returns an L16 passthrough stage that byte-swaps little-endian capture
// PCM into big-endian L16 RTP payloads. It takes the channel count from its
// source (Negotiated), so one stage serves any channel selection.
func NewPCM() Stage { return pcmStage{} }

func (pcmStage) Run(src audio.Source, gate Gate, emit func(Frame) error) error {
	rate, ch := src.Negotiated()
	frameBytes := 2 * ch
	maxBytes := max(min((rate/50)*frameBytes, maxL16Payload), frameBytes) // 20 ms
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
		ok, session, captured := gate.admit(&period)
		if !ok {
			continue
		}
		// captured marks the end of the period; each payload is stamped with its own
		// first sample, so the stamp matches the RTP timestamp the writer pairs it
		// with.
		total := len(period.Buf) / frameBytes
		remaining := total // frames not yet emitted
		if _, err := pk.Split(period.Buf, func(payload []byte) error {
			n := len(payload) / frameBytes
			at := captured.Add(-framesDur(remaining, rate))
			pos := period.Pos + uint64(total-remaining)
			remaining -= n
			return emit(Frame{
				Payload:  payload,
				Duration: uint32(n),
				Pos:      pos,
				Captured: at,
				Session:  session,
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
	// session is the play session the encoder state belongs to. When the gate
	// reports another session, the encoder resets and drops the stale partial
	// frame (a period queued for another session never gets here), so a
	// new stretch of playing starts exactly as a freshly built encoder's would.
	// It starts at 0, which a feed never reports while active, so the first
	// client resets the fresh encoder too; that is harmless. (A nil gate follows the
	// periods' own tags, so untagged periods stay in session 0 and never reset.)
	var session uint64
	// frameStart is the capture time of the accumulator's first sample.
	var frameStart time.Time
	// framePos is the stream position of the accumulator's first sample, and
	// nextPos the position right after its last one.
	var framePos, nextPos uint64

	for {
		period, err := src.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		ok, s, captured := gate.admit(&period)
		if !ok {
			continue
		}
		if s != session {
			enc.Reset()
			acc = acc[:0]
			session = s
		} else if len(acc) > 0 && period.Pos != nextPos {
			// The fan-out dropped audio inside this frame. Discard the partial rather
			// than splice it to audio captured after the gap; keep the encoder, since
			// the decoder still holds the last frame it produced.
			acc = acc[:0]
		}
		nextPos = period.Pos + uint64(len(period.Buf)/2/ch)
		// Fill the accumulator up to the rest of the current frame per pass, so the
		// per-sample loop carries no frame-boundary branch.
		pcm := period.Buf
		for len(pcm) >= 2 {
			if len(acc) == 0 {
				// A frame starts at the first sample not yet consumed from this period,
				// which is as far before the period's end (captured) as the samples left.
				frameStart = captured.Add(-framesDur(len(pcm)/2/ch, rate))
				framePos = period.Pos + uint64((len(period.Buf)-len(pcm))/2/ch)
			}
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
			if err := emit(Frame{Payload: encBuf[:n], Duration: opusFrameSamples, Pos: framePos, Captured: frameStart, Session: session}); err != nil {
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
