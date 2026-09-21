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

// sext24 sign-extends a 24-bit two's-complement value (in the low 3 bytes of v)
// to int32, so a test can compute the reference int16(sample24>>8) the same way
// the hardware's 24-bit sample would be interpreted.
func sext24(b0, b1, b2 byte) int32 {
	v := int32(uint32(b0) | uint32(b1)<<8 | uint32(b2)<<16)
	if v&0x800000 != 0 {
		v |= ^int32(0xFFFFFF) // set the top 8 bits for a negative 24-bit value
	}
	return v
}

func TestDownconvertS243LEToS16(t *testing.T) {
	t.Parallel()
	// 3-byte-packed S24_3LE samples. want is int16(sext24(sample)>>8): the top 16
	// of the 24 bits, i.e. bytes [1] and [2] of each sample.
	cases := []struct {
		name       string
		b0, b1, b2 byte
		want       int16
	}{
		{"positive", 0x11, 0x22, 0x33, 0x3322},
		{"max_positive", 0xFF, 0xFF, 0x7F, 0x7FFF},
		{"minus_one", 0xFF, 0xFF, 0xFF, -1},
		{"most_negative", 0x00, 0x00, 0x80, -0x8000},
		{"low_byte_truncated", 0x99, 0x00, 0x00, 0x0000}, // low 8 bits dropped
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if ref := int16(sext24(tc.b0, tc.b1, tc.b2) >> 8); ref != tc.want {
				t.Fatalf("test case wrong: sext24 ref = %#04x, want %#04x", uint16(ref), uint16(tc.want))
			}
			src := []byte{tc.b0, tc.b1, tc.b2}
			dst := make([]byte, 2)
			n := downconvertS243LEToS16(dst, src)
			if n != 2 {
				t.Fatalf("wrote %d bytes, want 2", n)
			}
			if got := int16(binary.LittleEndian.Uint16(dst)); got != tc.want {
				t.Errorf("downconvert(% x) = %#04x, want %#04x", src, uint16(got), uint16(tc.want))
			}
		})
	}
}

func TestDownconvertS24LEToS16IgnoresPaddingByte(t *testing.T) {
	t.Parallel()
	// S24_LE is a 4-byte word whose 4th byte is unreliable padding (zero on some
	// devices, sign extension on others). The reduction must use bytes [1] and [2]
	// of the low-3-byte 24-bit value and IGNORE byte [3]: the same 24-bit value
	// with 0x00 and 0xFF padding must reduce identically. A naive int16(word>>16)
	// (bytes [2],[3]) would instead leak the padding byte and fail this.
	cases := []struct {
		name string
		word []byte
		want int16
	}{
		{"zero_padding", []byte{0x11, 0x22, 0x33, 0x00}, 0x3322},
		{"sign_ext_padding", []byte{0x11, 0x22, 0x33, 0xFF}, 0x3322},
		{"negative_zero_pad", []byte{0x00, 0x00, 0x80, 0x00}, -0x8000},
		{"negative_sign_ext_pad", []byte{0x00, 0x00, 0x80, 0xFF}, -0x8000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dst := make([]byte, 2)
			n := downconvertS24LEToS16(dst, tc.word)
			if n != 2 {
				t.Fatalf("wrote %d bytes, want 2", n)
			}
			if got := int16(binary.LittleEndian.Uint16(dst)); got != tc.want {
				t.Errorf("downconvert(% x) = %#04x, want %#04x (padding byte must be ignored)", tc.word, uint16(got), uint16(tc.want))
			}
		})
	}
}

func TestDownconvertS24ToS16Interleaved(t *testing.T) {
	t.Parallel()
	// Two stereo S24_3LE frames (4 samples) verify the strided copy keeps channel
	// order at a 3-byte stride.
	src := []byte{
		0x11, 0x22, 0x33, // ch0 frame0 -> 0x3322
		0x44, 0x55, 0x66, // ch1 frame0 -> 0x6655
		0xAA, 0xBB, 0xCC, // ch0 frame1 -> 0xCCBB
		0x00, 0x00, 0x7F, // ch1 frame1 -> 0x7F00
	}
	dst := make([]byte, 4*2)
	n := downconvertS243LEToS16(dst, src)
	if n != 4*2 {
		t.Fatalf("wrote %d bytes, want %d", n, 4*2)
	}
	want := []int16{0x3322, 0x6655, -0x3345 /* 0xCCBB */, 0x7F00}
	for i, w := range want {
		if got := int16(binary.LittleEndian.Uint16(dst[i*2:])); got != w {
			t.Errorf("sample %d = %#04x, want %#04x", i, uint16(got), uint16(w))
		}
	}
}

func TestDownconvertS24ToS16IgnoresPartialSample(t *testing.T) {
	t.Parallel()
	// 4 bytes at a 3-byte stride = one whole sample plus a 1-byte remainder that
	// must be dropped rather than read out of bounds.
	src := []byte{0x11, 0x22, 0x33, 0xAA}
	dst := make([]byte, 4)
	n := downconvertS243LEToS16(dst, src)
	if n != 2 {
		t.Fatalf("wrote %d bytes, want 2 (partial sample ignored)", n)
	}
	if got := int16(binary.LittleEndian.Uint16(dst)); got != 0x3322 {
		t.Errorf("got %#04x, want 0x3322", uint16(got))
	}
}

// FuzzDownconvertS24ToS16 pins both 24-bit layouts to the exact reference the doc
// comment claims: each output sample equals int16(sext24(low 3 bytes) >> 8),
// regardless of the source sample width (3 for S24_3LE, 4 for S24_LE) and,
// crucially, regardless of the 4th byte for the 4-byte layout.
func FuzzDownconvertS24ToS16(f *testing.F) {
	f.Add([]byte{0x78, 0x56, 0x34}, 3)
	f.Add([]byte{0xFF, 0xFF, 0xFF}, 3)
	f.Add([]byte{0x78, 0x56, 0x34, 0xFF}, 4)
	f.Add([]byte{0x00, 0x00, 0x80, 0x00, 0x01, 0x02, 0x03, 0x04}, 4)
	f.Add([]byte{0x11, 0x22}, 3) // partial sample
	f.Add([]byte{}, 4)
	f.Fuzz(func(t *testing.T, src []byte, srcBytes int) {
		if srcBytes != 3 && srcBytes != 4 {
			t.Skip() // production only ever calls with the two real sample widths
		}
		samples := len(src) / srcBytes
		dst := make([]byte, samples*2)
		n := downconvertS24ToS16(dst, src, srcBytes)
		if n != samples*2 {
			t.Fatalf("wrote %d bytes, want %d", n, samples*2)
		}
		for i := 0; i < samples; i++ {
			s := src[i*srcBytes : i*srcBytes+3]
			want := int16(sext24(s[0], s[1], s[2]) >> 8)
			got := int16(binary.LittleEndian.Uint16(dst[i*2:]))
			if got != want {
				t.Fatalf("sample %d (% x): got %d, want %d", i, s, got, want)
			}
		}
	})
}

func BenchmarkDownconvertS243LEToS16(b *testing.B) {
	// One 20 ms period at 48 kHz mono S24_3LE (960 frames).
	src := make([]byte, 960*3)
	dst := make([]byte, 960*2)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		downconvertS243LEToS16(dst, src)
	}
}
