package audio

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type recordingObserver struct {
	periods [][]byte
}

func (r *recordingObserver) Observe(pcm []byte) {
	r.periods = append(r.periods, pcm)
}

func TestMeteredSourcePassesPeriodsToObserver(t *testing.T) {
	p0 := []byte{1, 2, 3, 4}
	p1 := []byte{5, 6}
	obs := &recordingObserver{}
	src := NewMeteredSource(NewFakeSource(48000, 1, [][]byte{p0, p1}), obs)

	rate, ch := src.Negotiated()
	if rate != 48000 || ch != 1 {
		t.Fatalf("negotiated = %d/%d, want 48000/1", rate, ch)
	}

	got0, err := src.Read()
	if err != nil {
		t.Fatalf("read 0: %v", err)
	}
	if !bytes.Equal(got0.Buf, p0) {
		t.Errorf("read 0 buf = %v, want %v", got0.Buf, p0)
	}
	if _, err := src.Read(); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if _, err := src.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read 2 err = %v, want EOF", err)
	}

	// EOF period is not observed; only the two real periods are.
	if len(obs.periods) != 2 {
		t.Fatalf("observed %d periods, want 2", len(obs.periods))
	}
	if !bytes.Equal(obs.periods[0], p0) || !bytes.Equal(obs.periods[1], p1) {
		t.Errorf("observed periods wrong: %v", obs.periods)
	}
}

func TestMeteredSourceClosePropagates(t *testing.T) {
	src := NewMeteredSource(NewFakeSource(48000, 1, nil), &recordingObserver{})
	if err := src.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
