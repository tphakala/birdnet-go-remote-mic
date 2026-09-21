package audio

// downconvertS32ToS16 writes the S16LE reduction of the S32LE samples in src
// into dst and returns the number of bytes written (len(src) rounded down to a
// whole sample, halved). Both buffers are little-endian, so the top 16 bits of
// a 32-bit sample are its high two bytes: the reduction is a strided copy of
// bytes 2 and 3 of every 4-byte sample. That truncates the low 16 bits, exactly
// as an arithmetic int16(sample>>16) would, with no decode, sign-extension, or
// re-encode. A trailing partial sample (fewer than 4 bytes) is ignored. dst
// must have room for len(src)/2 bytes.
func downconvertS32ToS16(dst, src []byte) int {
	samples := len(src) / 4
	d := dst[:samples*2] // output is exactly samples*2 bytes
	for i := 0; i < samples; i++ {
		s := src[i*4 : i*4+4] // one slice-bind proves s[2] and s[3] in bounds
		d[i*2] = s[2]
		d[i*2+1] = s[3]
	}
	return samples * 2
}

// downconvertS24ToS16 writes the S16LE reduction of the 24-bit little-endian
// samples in src into dst and returns the number of bytes written. srcBytes is
// the width of one source sample and selects the layout: 3 for FormatS243LE (24
// bits packed in 3 bytes) or 4 for FormatS24LE (24 valid bits in the low 3 bytes
// of a 4-byte word). In BOTH layouts the sample's 24-bit value occupies the low
// 3 bytes, so its top 16 bits are bytes [1] and [2]; the reduction is a strided
// copy of those two, exactly as int16(sample24>>8) would compute, dropping the
// low 8 bits. For FormatS24LE this also drops byte [3], which is unreliable
// padding (zero on some devices, sign extension on others) that a consumer must
// not read; taking only bytes [1] and [2] keeps this correct regardless of what
// the device wrote there. A trailing partial sample is ignored. srcBytes must be
// 3 or 4, and dst must have room for (len(src)/srcBytes)*2 bytes.
func downconvertS24ToS16(dst, src []byte, srcBytes int) int {
	samples := len(src) / srcBytes
	d := dst[:samples*2] // output is exactly samples*2 bytes
	for i := 0; i < samples; i++ {
		s := src[i*srcBytes : i*srcBytes+3] // slice-bind proves s[1] and s[2] in bounds
		d[i*2] = s[1]
		d[i*2+1] = s[2]
	}
	return samples * 2
}

// downconvertS24LEToS16 is the FormatS24LE reduction: 4-byte-word samples whose
// low 3 bytes hold the 24-bit value. It has the uniform func(dst, src) reduction
// signature so a convertingSource can hold it alongside downconvertS32ToS16.
func downconvertS24LEToS16(dst, src []byte) int { return downconvertS24ToS16(dst, src, 4) }

// downconvertS243LEToS16 is the FormatS243LE reduction: 24-bit samples packed in
// 3 bytes, the native format of many USB Audio Class microphones.
func downconvertS243LEToS16(dst, src []byte) int { return downconvertS24ToS16(dst, src, 3) }
