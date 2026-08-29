package audio

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFakeSourceDelivers(t *testing.T) {
	p0 := []byte{1, 2, 3, 4} // 2 mono S16 frames
	p1 := []byte{5, 6}       // 1 mono S16 frame
	src := NewFakeSource(48000, 1, [][]byte{p0, p1})

	rate, ch := src.Negotiated()
	if rate != 48000 || ch != 1 {
		t.Errorf("Negotiated = %d, %d; want 48000, 1", rate, ch)
	}

	got0, err := src.Read()
	if err != nil || got0.Frames != 2 || !bytes.Equal(got0.Buf, p0) {
		t.Fatalf("Read 0 = %+v, %v", got0, err)
	}
	got1, err := src.Read()
	if err != nil || got1.Frames != 1 || !bytes.Equal(got1.Buf, p1) {
		t.Fatalf("Read 1 = %+v, %v", got1, err)
	}
	if _, err := src.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("Read after exhaustion = %v, want io.EOF", err)
	}
	if err := src.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}
