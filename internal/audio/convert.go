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
	d := dst[:samples*2] // length-bind once so the loop needs no per-write bounds check
	for i := 0; i < samples; i++ {
		s := src[i*4 : i*4+4] // one slice-bind proves s[2] and s[3] in bounds
		d[i*2] = s[2]
		d[i*2+1] = s[3]
	}
	return samples * 2
}
