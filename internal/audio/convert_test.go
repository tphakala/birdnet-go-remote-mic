package audio

import (
	"encoding/binary"
	"testing"
)

func TestDownconvertS32ToS16(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    int32
		want int16
	}{
		{"positive", 0x12345678, 0x1234},
		{"max16", 0x7FFF0000, 0x7FFF},
		{"minus_one", -1, -1},
		{"most_negative", -0x80000000, -0x8000},
		{"small_positive_truncates", 0x00008000, 0x0000},
		// 0xFFFE1234: sign preserved AND nonzero low 16 bits (0x1234) dropped.
		{"negative_high_bits_truncate", -126412, -2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := make([]byte, 4)
			binary.LittleEndian.PutUint32(src, uint32(tc.v))
			dst := make([]byte, 2)
			n := downconvertS32ToS16(dst, src)
			if n != 2 {
				t.Fatalf("wrote %d bytes, want 2", n)
			}
			got := int16(binary.LittleEndian.Uint16(dst))
			if got != tc.want {
				t.Errorf("downconvert(%#08x) = %#04x, want %#04x", uint32(tc.v), uint16(got), uint16(tc.want))
			}
		})
	}
}

func TestDownconvertS32ToS16Interleaved(t *testing.T) {
	t.Parallel()
	// Two stereo frames (4 samples) verify the strided copy keeps channel order.
	samples := []int32{0x11112222, 0x33334444, 0x55556666, 0x7777_0000}
	src := make([]byte, len(samples)*4)
	for i, s := range samples {
		binary.LittleEndian.PutUint32(src[i*4:], uint32(s))
	}
	dst := make([]byte, len(samples)*2)
	n := downconvertS32ToS16(dst, src)
	if n != len(samples)*2 {
		t.Fatalf("wrote %d bytes, want %d", n, len(samples)*2)
	}
	want := []int16{0x1111, 0x3333, 0x5555, 0x7777}
	for i, w := range want {
		got := int16(binary.LittleEndian.Uint16(dst[i*2:]))
		if got != w {
			t.Errorf("sample %d = %#04x, want %#04x", i, uint16(got), uint16(w))
		}
	}
}

func TestDownconvertS32ToS16IgnoresPartialSample(t *testing.T) {
	t.Parallel()
	// 6 bytes = one whole 4-byte sample plus a 2-byte remainder that must be
	// dropped rather than read out of bounds.
	src := []byte{0x00, 0x00, 0x34, 0x12, 0xAA, 0xBB}
	dst := make([]byte, 4)
	n := downconvertS32ToS16(dst, src)
	if n != 2 {
		t.Fatalf("wrote %d bytes, want 2 (partial sample ignored)", n)
	}
	if got := int16(binary.LittleEndian.Uint16(dst)); got != 0x1234 {
		t.Errorf("got %#04x, want 0x1234", uint16(got))
	}
}

// FuzzDownconvertS32ToS16 pins the whole input domain to the exact reference the
// doc comment claims: each output sample must equal int16(int32(sample) >> 16).
func FuzzDownconvertS32ToS16(f *testing.F) {
	f.Add([]byte{0x78, 0x56, 0x34, 0x12})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0x00, 0x00, 0x00, 0x80, 0x01, 0x02, 0x03, 0x04})
	f.Add([]byte{0x11, 0x22, 0x33}) // partial sample
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, src []byte) {
		samples := len(src) / 4
		dst := make([]byte, samples*2)
		n := downconvertS32ToS16(dst, src)
		if n != samples*2 {
			t.Fatalf("wrote %d bytes, want %d", n, samples*2)
		}
		for i := 0; i < samples; i++ {
			v := int32(binary.LittleEndian.Uint32(src[i*4:]))
			want := int16(v >> 16)
			got := int16(binary.LittleEndian.Uint16(dst[i*2:]))
			if got != want {
				t.Fatalf("sample %d: got %d, want int16(%d>>16)=%d", i, got, v, want)
			}
		}
	})
}

func BenchmarkDownconvertS32ToS16(b *testing.B) {
	// One 20 ms period at 48 kHz mono S32 (960 frames).
	src := make([]byte, 960*4)
	dst := make([]byte, 960*2)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		downconvertS32ToS16(dst, src)
	}
}
