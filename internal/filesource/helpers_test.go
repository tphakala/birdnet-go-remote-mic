package filesource

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	flacpcm "github.com/tphakala/go-flac/pcm"
	"github.com/tphakala/go-opus/oggopus"
)

// recorder collects Logf lines so tests never touch the global logger.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// ramp returns frames of channels S16LE samples whose values never repeat
// within a few thousand frames and differ per channel, so a dropped, repeated
// or swapped sample shows up as a byte difference.
func ramp(frames, channels int) []byte {
	b := make([]byte, frames*channels*2)
	for i := range frames {
		for c := range channels {
			v := uint16(i*37 + c*5003 + 11)
			binary.LittleEndian.PutUint16(b[(i*channels+c)*2:], v)
		}
	}
	return b
}

// noise returns incompressible S16LE samples.
func noise(frames, channels int) []byte {
	b := make([]byte, frames*channels*2)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// s16To24 widens S16LE samples to packed 24-bit little-endian (s16 << 8).
func s16To24(s16 []byte) []byte {
	w := make([]byte, len(s16)/2*3)
	for i := range len(s16) / 2 {
		w[i*3+1] = s16[i*2]
		w[i*3+2] = s16[i*2+1]
	}
	return w
}

func flacBytes(t *testing.T, rate, channels, bits int, pcm []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := flacpcm.Config{SampleRate: rate, Channels: channels, BitDepth: bits}
	if err := flacpcm.EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("encode flac: %v", err)
	}
	return buf.Bytes()
}

func opusBytes(t *testing.T, inputRate, channels int, pcm []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := oggopus.Config{SampleRate: inputRate, Channels: channels}
	if err := oggopus.EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("encode opus: %v", err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readN reads n periods and returns their concatenated bytes.
func readN(t *testing.T, s *Source, n int) []byte {
	t.Helper()
	out := make([]byte, 0, n*periodFrames*s.channels*2)
	for i := range n {
		p, err := s.Read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		out = append(out, p.Buf[:p.Frames*s.channels*2]...)
	}
	return out
}

func repeat(b []byte, n int) []byte { return bytes.Repeat(b, n) }

func containsPath(err error, dir string) bool {
	return err != nil && strings.Contains(err.Error(), dir)
}

func closeQuiet(s *Source) { _ = s.Close() }
