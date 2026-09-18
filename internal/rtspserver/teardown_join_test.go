package rtspserver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rtsp "github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// slowExitSource parks its writer in Next until the connection context is
// cancelled, then lingers before returning, simulating a writer that is still
// reading the shared frame source as teardown begins. It records when that read
// finally returns so a test can prove the track slot is not released until then.
type slowExitSource struct {
	linger  time.Duration
	started chan struct{}
	once    sync.Once
	exitAt  atomic.Int64 // UnixNano when Next returned after cancellation; 0 until then
	active  atomic.Bool
}

func (s *slowExitSource) Next(ctx context.Context) (pipeline.Frame, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	time.Sleep(s.linger)
	s.exitAt.Store(time.Now().UnixNano())
	return pipeline.Frame{}, ctx.Err()
}

func (s *slowExitSource) SetActive(active bool) { s.active.Store(active) }

// TestServeConnJoinsWriterBeforeReleasingSlot pins the teardown join (item 6): a
// played connection must not release its track slot until the writer goroutine has
// fully stopped reading the shared frame source, otherwise a new client could take
// the slot while the old writer is still parked in Next and steal one frame. The
// source lingers in Next after the context is cancelled; the slot must stay held
// across that linger and free only once the read has returned.
func TestServeConnJoinsWriterBeforeReleasingSlot(t *testing.T) {
	src := &slowExitSource{linger: 150 * time.Millisecond, started: make(chan struct{})}
	spec := pipeline.SDPSpec(&config.Device{Name: "teardown", Mode: config.ModePCM}, 48000, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	track := &Track{Path: testPath, SDP: sdpBytes, PayloadType: 96, Frames: src}
	addr := serveWith(t, Config{SRInterval: time.Hour, Timeout: 30 * time.Second}, track)

	client, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:     "rtsp://" + addr + testPath,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Close the client on any exit path, including an early t.Fatalf below, so a
	// failed handshake does not leak the connection until the listener teardown.
	t.Cleanup(func() { _ = client.Close() })
	if err := drivePlay(t, client); err != nil {
		t.Fatalf("play handshake: %v", err)
	}

	// The writer is now parked in Next, holding the slot.
	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never reached the frame source")
	}
	if !track.ClientConnected() {
		t.Fatal("slot not held after PLAY")
	}

	// Drop the client: serveConn tears down and cancels the per-conn context. The
	// source lingers before its Next returns; the join must hold the slot until then.
	_ = client.Close()

	var slotFreeAt time.Time
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !track.ClientConnected() {
			slotFreeAt = time.Now()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if slotFreeAt.IsZero() {
		t.Fatal("slot was never released after the client dropped")
	}
	exitNano := src.exitAt.Load()
	if exitNano == 0 {
		t.Fatal("the frame source read never returned")
	}
	if exitAt := time.Unix(0, exitNano); slotFreeAt.Before(exitAt) {
		t.Fatalf("slot released %v before the writer finished reading; the teardown did not join the writer", exitAt.Sub(slotFreeAt))
	}
}
