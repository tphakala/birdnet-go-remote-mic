//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
)

// scriptedSource replays one fixed stereo period forever, with channel 1 loud
// for the first loudFrames frames it delivers and channel 2 loud after that.
type scriptedSource struct {
	rate, frames, loudFrames, sent int
}

func (s *scriptedSource) Negotiated() (rate, channels int) { return s.rate, 2 }
func (s *scriptedSource) Close() error                     { return nil }
func (s *scriptedSource) Read() (audio.Period, error) {
	buf := make([]byte, s.frames*2*2)
	for f := 0; f < s.frames; f++ {
		ch := 1 // loud channel, 0-based
		if s.sent+f < s.loudFrames {
			ch = 0
		}
		binary.LittleEndian.PutUint16(buf[(f*2+ch)*2:], uint16(int16(16000)))
	}
	s.sent += s.frames
	return audio.Period{Buf: buf, Frames: s.frames}, nil
}

// stalledSource never delivers a period until closed.
type stalledSource struct{ closed chan struct{} }

func (b *stalledSource) Negotiated() (rate, channels int) { return 48000, 2 }
func (b *stalledSource) Read() (audio.Period, error) {
	<-b.closed
	return audio.Period{}, errors.New("closed")
}
func (b *stalledSource) Close() error { close(b.closed); return nil }

func stubProbeCapture(t *testing.T, src audio.Source) {
	t.Helper()
	prevOpen, prevBusy := openProbeCapture, deviceInUse
	openProbeCapture = func(string, int, int) (audio.Source, error) { return src, nil }
	// No real hardware under test, so the busy check is always "free".
	deviceInUse = func(string, int) bool { return false }
	t.Cleanup(func() { openProbeCapture, deviceInUse = prevOpen, prevBusy })
}

// closeFlagSource records that Close was called, over a scriptedSource base.
type closeFlagSource struct {
	scriptedSource
	closed chan struct{}
}

func (s *closeFlagSource) Close() error { close(s.closed); return nil }

// TestProbeChannelLevelsSkipsSettle asserts audio inside the settle window is
// discarded: channel 1 is loud only while the capture settles, so the measured
// window finds channel 2 loud and channel 1 silent.
func TestProbeChannelLevelsSkipsSettle(t *testing.T) {
	const rate = 48000
	settleFrames := int(int64(rate) * int64(probeSettle) / int64(time.Second))
	stubProbeCapture(t, &scriptedSource{rate: rate, frames: 480, loudFrames: settleFrames})
	got, err := probeChannelLevels(context.Background(), "hw:9", rate, 2)
	if err != nil {
		t.Fatalf("probeChannelLevels: %v", err)
	}
	if len(got) != 2 || got[0] > -90 || got[1] < -10 {
		t.Fatalf("levels = %v, want channel 1 at the floor and channel 2 loud", got)
	}
}

// TestProbeChannelLevelsTimesOut asserts a device that never delivers audio is
// bounded by the context instead of hanging provisioning.
func TestProbeChannelLevelsTimesOut(t *testing.T) {
	stubProbeCapture(t, &stalledSource{closed: make(chan struct{})})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := probeChannelLevels(ctx, "hw:9", 48000, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe took %v after a 50 ms deadline", elapsed)
	}
}

// TestProbeChannelLevelsSkipsBusyDevice asserts a device another process holds
// exclusively is rejected before any blocking open.
func TestProbeChannelLevelsSkipsBusyDevice(t *testing.T) {
	prevOpen, prevBusy := openProbeCapture, deviceInUse
	opened := false
	openProbeCapture = func(string, int, int) (audio.Source, error) {
		opened = true
		return &scriptedSource{rate: 48000, frames: 480}, nil
	}
	deviceInUse = func(string, int) bool { return true }
	t.Cleanup(func() { openProbeCapture, deviceInUse = prevOpen, prevBusy })

	if _, err := probeChannelLevels(context.Background(), "hw:9", 48000, 2); err == nil {
		t.Fatal("probe on a busy device returned no error")
	}
	if opened {
		t.Error("probe opened a device reported busy")
	}
}

// TestProbeChannelLevelsBoundsBlockingOpen asserts a blocking ALSA open is
// bounded by ctx (the probe returns promptly), and a source that opens only
// after the deadline is closed rather than leaked.
func TestProbeChannelLevelsBoundsBlockingOpen(t *testing.T) {
	release := make(chan struct{})
	lateClosed := make(chan struct{})
	prevOpen, prevBusy := openProbeCapture, deviceInUse
	openProbeCapture = func(string, int, int) (audio.Source, error) {
		<-release // block the open until the test releases it
		return &closeFlagSource{scriptedSource: scriptedSource{rate: 48000, frames: 480}, closed: lateClosed}, nil
	}
	deviceInUse = func(string, int) bool { return false }
	t.Cleanup(func() { openProbeCapture, deviceInUse = prevOpen, prevBusy })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := probeChannelLevels(ctx, "hw:9", 48000, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe took %v after a 50 ms deadline; the blocking open was not bounded", elapsed)
	}
	// Let the open finish; the source it produces after the cancel must be closed.
	close(release)
	select {
	case <-lateClosed:
	case <-time.After(time.Second):
		t.Fatal("the source opened after cancellation was not closed")
	}
}

// TestProbeChannelLevelsConsumesWholePeriods asserts that with a period size that
// does not divide the settle+measure window, the probe consumes exactly the whole
// number of periods that covers it, and honors the settle boundary mid-period.
func TestProbeChannelLevelsConsumesWholePeriods(t *testing.T) {
	const rate = 48000
	const frames = 500
	settle := int(int64(rate) * int64(probeSettle) / int64(time.Second))
	measure := int(int64(rate) * int64(probeMeasure) / int64(time.Second))
	src := &scriptedSource{rate: rate, frames: frames, loudFrames: settle}
	stubProbeCapture(t, src)
	got, err := probeChannelLevels(context.Background(), "hw:9", rate, 2)
	if err != nil {
		t.Fatalf("probeChannelLevels: %v", err)
	}
	want := ((settle + measure + frames - 1) / frames) * frames
	if src.sent != want {
		t.Errorf("consumed %d frames, want %d (whole periods covering settle+measure=%d, period=%d)", src.sent, want, settle+measure, frames)
	}
	// settle (7200) is not a multiple of 500, so channel 1 stops being loud
	// mid-period; the measured window must still find channel 1 at the floor.
	if len(got) != 2 || got[0] > -80 || got[1] < -10 {
		t.Fatalf("levels = %v, want channel 1 at the floor and channel 2 loud", got)
	}
}
