package filesource

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	flacpcm "github.com/tphakala/go-flac/pcm"
	"github.com/tphakala/go-opus/oggopus"
)

// errUnsupportedFormat marks a file whose extension no decoder claims.
var errUnsupportedFormat = errors.New("unsupported format")

// flacOptions apply to NewDecoder and to every Reset: options do not persist
// across a Reset. Output is always 16-bit, and the MD5 check is skipped (it
// hashes every frame, and a mismatch could only be reported after the audio
// has streamed); frame CRCs and the truncation check remain.
var flacOptions = []flacpcm.DecoderOption{flacpcm.WithConvertTo(16), flacpcm.SkipMD5Verification()}

// decoders holds at most one decoder per format, created on first use and
// reused with Reset for every later file or loop, so a pass allocates only at
// its file switches. Only the reader goroutine touches it.
type decoders struct {
	flac *flacpcm.Decoder
	opus *oggopus.Decoder
}

// supported reports whether a decoder claims name's extension; keep it in step
// with the switch in open.
func supported(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".flac", ".opus":
		return true
	default:
		return false
	}
}

// open points the decoder for name's format at f, parses its header and
// returns it with the stream's rate and channel count. The decoder yields
// S16LE interleaved frames.
func (d *decoders) open(name string, f *os.File) (r io.Reader, rate, channels int, err error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".flac":
		if d.flac == nil {
			d.flac, err = flacpcm.NewDecoder(f, flacOptions...)
		} else {
			err = d.flac.Reset(f, flacOptions...)
		}
		if err != nil {
			d.flac = nil // state after a failed header parse is not worth trusting
			return nil, 0, 0, err
		}
		info := d.flac.Info()
		return d.flac, info.SampleRate, info.Channels, nil
	case ".opus":
		if d.opus == nil {
			d.opus, err = oggopus.NewDecoder(f)
		} else {
			err = d.opus.Reset(f)
		}
		if err != nil {
			d.opus = nil
			return nil, 0, 0, err
		}
		info := d.opus.Info()
		return d.opus, int(info.OutputSampleRate), info.Channels, nil
	default:
		return nil, 0, 0, errUnsupportedFormat
	}
}
