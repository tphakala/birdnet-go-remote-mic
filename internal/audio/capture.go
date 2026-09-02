//go:build linux

package audio

import (
	"errors"
	"fmt"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// captureStream is the subset of *capture.Stream the appliance uses; the seam
// lets tests substitute a stub with no hardware.
type captureStream interface {
	Negotiated() capture.Config
	Start() error
	Read(buf []byte) (int, error)
	Close() error
}

// openStream is a package var so tests can inject a fake capture layer.
var openStream = func(cfg capture.Config) (captureStream, error) {
	s, err := capture.Open(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

type captureSource struct {
	s          captureStream
	rate       int
	channels   int
	frameBytes int
	buf        []byte
}

// captureFormat maps a validated config format string to the go-audio-capture
// Format of the STREAM OUTPUT. config.Validate already rejects anything but
// "s16", so this is a belt-and-braces guard on the pipeline's output contract:
// the whole send path (L16 packetization, SDP byte math, Opus encode) is
// S16-only, and this fails loud the moment a new stream format is added to
// validation without wiring the rest of the pipeline. It is distinct from the
// hardware CAPTURE format, which OpenCaptureAt negotiates (S16 or S32) and
// downconverts to S16 so this output contract always holds.
func captureFormat(format string) (capture.Format, error) {
	switch format {
	case "s16", "":
		return capture.FormatS16LE, nil
	default:
		return 0, fmt.Errorf("audio: unsupported stream format %q (only s16 is supported)", format)
	}
}

// captureFormats is the order OpenCaptureAt negotiates the hardware capture
// format in: S16LE first (the common case, no conversion needed), then S32LE as
// the fallback for 24/32-bit-only interfaces (e.g. the ZOOM AMS-24). The stream
// output stays S16 either way; an S32 capture is downconverted.
var captureFormats = []capture.Format{capture.FormatS16LE, capture.FormatS32LE}

// openNegotiate opens the device at the requested rate, trying each capture
// format in captureFormats order and returning the first that opens along with
// the format it opened with. A failure to open one format does not stop the
// next: a device can accept the requested rate only in the wider S32 format, so
// even a *BadRateError from S16 must not short-circuit the S32 attempt. When no
// format opens it returns the most informative error, preferring a *BadRateError
// (which carries the supported rate range) over a raw driver error.
func openNegotiate(device string, rate, channels int) (captureStream, capture.Format, error) {
	var chosenErr error
	for _, f := range captureFormats {
		s, err := openStream(capture.Config{
			Device:   device,
			Rate:     rate,
			Channels: channels,
			Format:   f,
		})
		if err == nil {
			return s, f, nil
		}
		chosenErr = preferRateError(chosenErr, err)
	}
	return nil, 0, chosenErr
}

// preferRateError keeps whichever of two open errors is more useful to report:
// a *BadRateError (it names the supported rate window) wins over a raw driver
// error, so a caller sees the honest-rate diagnostic rather than a bare EINVAL.
// When both attempts are *BadRateError the FIRST is kept, so the surfaced range
// is the preferred format's (S16, tried first) rather than the narrower fallback
// format's.
func preferRateError(existing, next error) error {
	if existing == nil {
		return next
	}
	var bre *capture.BadRateError
	if errors.As(existing, &bre) {
		return existing
	}
	if errors.As(next, &bre) {
		return next
	}
	return existing
}

// maxProbeChannels caps how many capture channels ResolveOpenChannels will probe
// for. It is sourced from the config channel cap so the two cannot drift.
const maxProbeChannels = config.MaxChannels

// ResolveOpenChannels returns the channel COUNT to open the device at so every
// channel in the 1-based selection is captured. ALSA opens a contiguous channel
// count starting at channel 0, so streaming channel N requires opening at least N
// channels; a device that cannot open exactly that count (a stereo-only interface
// asked for a single channel, say) is opened at the smallest supported count above
// it, and the selecting source drops the surplus. It probes counts from
// max(selection) up to maxProbeChannels with the non-blocking refine capability
// query (any capture format accepting the count wins). If none probes cleanly it
// falls back to max(selection) so the real open surfaces an honest error rather
// than this guessing. A device that supports the exact count is the common case
// and returns on the first probe.
func ResolveOpenChannels(device string, selection []int) int {
	maxSel := maxSelected(selection)
	for count := maxSel; count <= maxProbeChannels; count++ {
		for _, f := range captureFormats {
			if _, err := supportedRatesFn(device, count, f); err == nil {
				return count
			}
		}
	}
	return maxSel
}

// maxSelected returns the highest channel number in a 1-based selection, or 1 for
// an empty selection so the caller opens at least one channel.
func maxSelected(selection []int) int {
	m := 1
	for _, c := range selection {
		if c > m {
			m = c
		}
	}
	return m
}

// OpenCapture opens and starts a capture stream for dev, resolving the open
// channel count (ResolveOpenChannels) itself for callers that do not. The
// appliance resolves per open attempt and calls OpenCaptureAt directly, so this
// convenience wrapper has no production caller today; it is kept for callers and
// tests that just want "open dev" without managing the count. See OpenCaptureAt.
func OpenCapture(dev *config.Device) (Source, error) {
	return OpenCaptureAt(dev, ResolveOpenChannels(dev.Device, dev.Channels))
}

// OpenCaptureAt opens and starts a capture stream for dev at openCh hardware
// channels, which the caller resolved (ResolveOpenChannels) so the rate probe,
// the busy gate and the open all agree on one count without re-probing. It
// negotiates the hardware capture format (S16LE preferred, S32LE fallback) and,
// for an S32 device, wraps the stream so every period is downconverted to
// S16LE. It then wraps the stream to deliver only dev.Channels (the 1-based
// selection), so the device is opened at whatever contiguous channel count
// covers the selection but the pipeline sees exactly the selected channels. It
// enforces the honest-rate policy: go-audio-capture already fails a rate it
// cannot deliver exactly, and OpenCaptureAt double-checks the negotiated rate
// matches the request. The caller's read goroutine should runtime.LockOSThread
// so the capture loop is not descheduled mid-period.
func OpenCaptureAt(dev *config.Device, openCh int) (Source, error) {
	// dev.Format is the stream OUTPUT format; guard it (S16-only) before touching
	// hardware. The capture format is negotiated separately below.
	if _, err := captureFormat(dev.Format); err != nil {
		return nil, err
	}
	maxSel := maxSelected(dev.Channels)
	if openCh < maxSel {
		return nil, fmt.Errorf("audio: open channel count %d does not cover the selection (needs channel %d)", openCh, maxSel)
	}
	s, format, err := openNegotiate(dev.Device, dev.Rate, openCh)
	if err != nil {
		return nil, err
	}
	n := s.Negotiated()
	if n.Rate != dev.Rate {
		_ = s.Close()
		return nil, fmt.Errorf("audio: negotiated rate %d Hz does not match requested %d Hz", n.Rate, dev.Rate)
	}
	// Honesty check on the channel count, mirroring the rate check: the selecting
	// source extracts channel indices up to max(selection) from the negotiated
	// buffer, so a device that delivered fewer channels than we opened at would
	// make the strided copy read out of bounds. ALSA pins the channel count
	// exactly (or fails the open), so this cannot trigger on the linux backend
	// today; the guard turns any future surprise into an honest error instead of a
	// panic in the capture pump.
	if n.Channels < maxSel {
		_ = s.Close()
		return nil, fmt.Errorf("audio: device negotiated %d channels but the selection needs channel %d", n.Channels, maxSel)
	}
	if err := s.Start(); err != nil {
		_ = s.Close()
		return nil, err
	}
	var base Source
	switch format {
	case capture.FormatS16LE:
		frameBytes := n.Channels * 2 // S16LE
		base = &captureSource{
			s:          s,
			rate:       n.Rate,
			channels:   n.Channels,
			frameBytes: frameBytes,
			buf:        make([]byte, n.PeriodFrames*frameBytes),
		}
	case capture.FormatS32LE:
		base = &convertingSource{
			s:        s,
			rate:     n.Rate,
			channels: n.Channels,
			in:       make([]byte, n.PeriodFrames*n.Channels*4), // S32LE
			out:      make([]byte, n.PeriodFrames*n.Channels*2), // S16LE
		}
	default:
		_ = s.Close()
		return nil, fmt.Errorf("audio: negotiated unsupported capture format %v", format)
	}
	return newSelectingSource(base, n.Channels, dev.Channels), nil
}

// selectingSource wraps an interleaved S16LE Source opened at srcChannels and
// delivers only the selected channels, interleaved in selection order. This is
// how a device opened at a contiguous channel count (from channel 0) serves an
// arbitrary subset: a strided copy per frame extracts the wanted channels, so a
// stereo-only interface can serve a single-channel stream (and thus Opus). Like
// the sources it wraps it is single-consumer; the returned buffer is valid only
// until the next Read.
type selectingSource struct {
	inner Source
	rate  int
	srcCh int   // channels the inner source delivers (the open count)
	idx   []int // 0-based source channel indices to keep, in selection order
	out   []byte
}

// newSelectingSource wraps inner to extract the 1-based selection from an
// srcChannels-wide interleaved S16 stream. When the selection is exactly channels
// 1..srcChannels in order the extraction is a no-op, so inner is returned
// unwrapped and the hot path stays a plain passthrough.
func newSelectingSource(inner Source, srcChannels int, selection []int) Source {
	idx := make([]int, len(selection))
	for i, c := range selection {
		idx[i] = c - 1
	}
	if len(idx) == srcChannels {
		passthrough := true
		for i, v := range idx {
			if v != i {
				passthrough = false
				break
			}
		}
		if passthrough {
			return inner
		}
	}
	rate, _ := inner.Negotiated()
	return &selectingSource{inner: inner, rate: rate, srcCh: srcChannels, idx: idx}
}

func (s *selectingSource) Negotiated() (rate, channels int) { return s.rate, len(s.idx) }

func (s *selectingSource) Read() (Period, error) {
	p, err := s.inner.Read()
	if err != nil {
		return Period{}, err
	}
	outCh := len(s.idx)
	need := p.Frames * outCh * 2 // S16LE
	if cap(s.out) < need {
		s.out = make([]byte, need)
	}
	out := s.out[:need]
	srcFrameBytes := s.srcCh * 2
	for f := 0; f < p.Frames; f++ {
		base := f * srcFrameBytes
		o := f * outCh * 2
		for j, ci := range s.idx {
			so := base + ci*2
			out[o+j*2] = p.Buf[so]
			out[o+j*2+1] = p.Buf[so+1]
		}
	}
	return Period{Buf: out, Frames: p.Frames}, nil
}

func (s *selectingSource) Close() error { return s.inner.Close() }

func (c *captureSource) Negotiated() (rate, channels int) { return c.rate, c.channels }

func (c *captureSource) Read() (Period, error) {
	n, err := c.s.Read(c.buf)
	if err != nil {
		return Period{}, err
	}
	return Period{Buf: c.buf[:n*c.frameBytes], Frames: n}, nil
}

func (c *captureSource) Close() error { return c.s.Close() }

// convertingSource wraps an S32LE capture stream and delivers S16LE periods, so
// a 24/32-bit-only device looks like any other S16 Source to the pipeline. It
// keeps two reused buffers: in receives the raw S32 period from the stream, out
// holds its S16 reduction. Like captureSource it is single-consumer; out is
// valid only until the next Read.
type convertingSource struct {
	s        captureStream
	rate     int
	channels int
	in       []byte // S32LE read buffer
	out      []byte // S16LE output buffer
}

func (c *convertingSource) Negotiated() (rate, channels int) { return c.rate, c.channels }

func (c *convertingSource) Read() (Period, error) {
	n, err := c.s.Read(c.in)
	if err != nil {
		return Period{}, err
	}
	nbytes := downconvertS32ToS16(c.out, c.in[:n*c.channels*4])
	return Period{Buf: c.out[:nbytes], Frames: n}, nil
}

func (c *convertingSource) Close() error { return c.s.Close() }
