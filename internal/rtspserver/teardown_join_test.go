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

// slowExitFeed is a FrameFeed whose subscription parks its writer in Next until
// the connection context is cancelled, then lingers before returning,
// simulating a writer that is still reading as teardown begins. It records when
// that read finally returns and whether the subscription was closed, so a test
// can prove the disconnect is reported only after both.
type slowExitFeed struct {
	linger  time.Duration
	started chan struct{}
	once    sync.Once
	exitAt  atomic.Int64 // UnixNano when Next returned after cancellation; 0 until then
	closed  atomic.Bool
}

func (f *slowExitFeed) Subscribe() (Subscription, error) { return f, nil }

func (f *slowExitFeed) Next(ctx context.Context) (pipeline.Frame, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	time.Sleep(f.linger)
	f.exitAt.Store(time.Now().UnixNano())
	return pipeline.Frame{}, ctx.Err()
}

func (f *slowExitFeed) Close() { f.closed.Store(true) }

// disconnectStamp is a Listener that records when ClientDisconnected ran and
// whether the feed's subscription was already closed at that moment.
type disconnectStamp struct {
	feed         *slowExitFeed
	at           atomic.Int64
	closedBefore atomic.Bool
}

func (d *disconnectStamp) ClientConnected(string, string) {}

func (d *disconnectStamp) ClientDisconnected(string, string, DisconnectReason) {
	d.closedBefore.Store(d.feed.closed.Load())
	d.at.Store(time.Now().UnixNano())
}

// TestServeConnJoinsWriterBeforeReportingDisconnect pins the teardown order: a
// played connection unsubscribes, then joins its writer goroutine, and only
// then reports the disconnect, so every goroutine has an owner that waits for
// it. The feed lingers in Next after the context is cancelled; the disconnect
// must not be reported until that read has returned.
func TestServeConnJoinsWriterBeforeReportingDisconnect(t *testing.T) {
	feed := &slowExitFeed{linger: 150 * time.Millisecond, started: make(chan struct{})}
	stamp := &disconnectStamp{feed: feed}
	spec := pipeline.SDPSpec(&config.Stream{Mode: config.ModePCM}, "teardown", 48000, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	track := &Track{Path: testPath, SDP: sdpBytes, PayloadType: 96, Feed: feed}
	addr := serveWith(t, Config{SRInterval: time.Hour, Timeout: 30 * time.Second, Listener: stamp}, track)

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

	// The writer is now parked in Next.
	select {
	case <-feed.started:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never reached the feed")
	}

	// Drop the client: serveConn tears down and cancels the per-conn context. The
	// feed lingers before its Next returns; the join must wait for it.
	_ = client.Close()

	waitFor(t, func() bool { return stamp.at.Load() != 0 }, 3*time.Second, "the disconnect to be reported")
	exitNano := feed.exitAt.Load()
	if exitNano == 0 {
		t.Fatal("the feed read never returned")
	}
	if exitAt, discAt := time.Unix(0, exitNano), time.Unix(0, stamp.at.Load()); discAt.Before(exitAt) {
		t.Fatalf("disconnect reported %v before the writer finished reading; the teardown did not join the writer", exitAt.Sub(discAt))
	}
	if !stamp.closedBefore.Load() {
		t.Error("disconnect reported before the subscription was closed")
	}
}
