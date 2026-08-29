package rtspserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	rtsp "github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
	"github.com/tphakala/go-opus/opus"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// sineSource builds a fake capture source of `periods` mono S16LE periods and
// returns it alongside the concatenated source PCM.
func sineSource(periods, periodFrames, rate int) (source audio.Source, pcm []byte) {
	all := make([]byte, 0, periods*periodFrames*2)
	src := make([][]byte, periods)
	n := 0
	for k := range periods {
		b := make([]byte, periodFrames*2)
		for i := range periodFrames {
			v := int16(10000 * math.Sin(2*math.Pi*1000*float64(n)/float64(rate)))
			binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
			n++
		}
		src[k] = b
		all = append(all, b...)
	}
	return audio.NewFakeSource(rate, 1, src), all
}

// serveWith starts a server bound to a random port, feeding it from frames, and
// returns the listen address.
//
//nolint:gocritic // test helper; Config by value is fine.
func serveWith(t *testing.T, cfg Config, tr *Track) string {
	t.Helper()
	srv := New(cfg, tr)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serveConn(ctx, conn)
		}
	}()
	return ln.Addr().String()
}

func TestEndToEndAgainstIngestClientL16(t *testing.T) {
	const rate, periodFrames, periods = 48000, 960, 50
	fakeSrc, wantPCM := sineSource(periods, periodFrames, rate)

	frames := NewChanSource(128)
	spec := pipeline.SDPSpec(&config.Device{Name: "test", Mode: config.ModePCM}, rate, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	addr := serveWith(t, Config{SRInterval: 50 * time.Millisecond, Timeout: 30 * time.Second},
		&Track{Path: "/stream", SDP: sdpBytes, PayloadType: 96, Frames: frames})

	var mu sync.Mutex
	var got []byte
	var pts []time.Duration
	done := make(chan struct{})
	var once sync.Once
	client, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:     "rtsp://" + addr + "/stream",
		Timeout: 5 * time.Second,
		OnFrame: func(fr audiostream.Frame) {
			mu.Lock()
			got = append(got, fr.Data...)
			pts = append(pts, fr.PTS)
			if len(got) >= len(wantPCM) {
				once.Do(func() { close(done) })
			}
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := drivePlay(t, client); err != nil {
		t.Fatalf("play handshake: %v", err)
	}

	// Push only after PLAY: delivery is gated on activation, and the fake
	// source is finite, so pumping earlier would discard every period.
	go func() {
		_ = pipeline.NewPCM(1).Run(fakeSrc, func(f pipeline.Frame) error {
			frames.Push(f)
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the ingest client to receive the audio")
	}

	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(got[:len(wantPCM)], wantPCM) {
		t.Error("reassembled PCM is not bit-identical to the source")
	}
	for i := 1; i < len(pts); i++ {
		if pts[i] <= pts[i-1] {
			t.Fatalf("PTS not monotonic at frame %d: %v <= %v", i, pts[i], pts[i-1])
		}
	}
	valid := false
	for _, ts := range client.Stats().Tracks {
		if ts.SenderClock.Valid {
			valid = true
		}
	}
	if !valid {
		t.Error("no valid SenderClock: the RTCP sender report was not delivered")
	}
}

func TestEndToEndAgainstIngestClientOpus(t *testing.T) {
	const rate, periodFrames, periods = 48000, 960, 40
	fakeSrc, _ := sineSource(periods, periodFrames, rate)

	frames := NewChanSource(128)
	spec := pipeline.SDPSpec(&config.Device{Name: "test", Mode: config.ModeOpus, Opus: config.Opus{Bitrate: 64000}}, rate, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	addr := serveWith(t, Config{SRInterval: time.Hour, Timeout: 30 * time.Second},
		&Track{Path: "/stream", SDP: sdpBytes, PayloadType: 97, Frames: frames})

	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pcm := make([]int16, 960)

	var mu sync.Mutex
	decoded := 0
	done := make(chan struct{})
	var once sync.Once
	client, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:     "rtsp://" + addr + "/stream",
		Timeout: 5 * time.Second,
		OnFrame: func(fr audiostream.Frame) {
			mu.Lock()
			defer mu.Unlock()
			n, derr := dec.Decode(fr.Data, pcm)
			if derr != nil {
				t.Errorf("decode opus frame: %v", derr)
			} else if n != 960 {
				t.Errorf("decoded %d samples, want 960", n)
			}
			decoded++
			if decoded >= 20 {
				once.Do(func() { close(done) })
			}
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := drivePlay(t, client); err != nil {
		t.Fatalf("play handshake: %v", err)
	}

	// Push only after PLAY: delivery is gated on activation, and the fake
	// source is finite, so pumping earlier would discard every period.
	go func() {
		_ = pipeline.NewOpus(config.Opus{Bitrate: 64000}).Run(fakeSrc, func(f pipeline.Frame) error {
			frames.Push(f)
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Opus frames")
	}
}

// drivePlay runs the client's DESCRIBE/SETUP/PLAY after Dial (which only does
// OPTIONS), selecting the first described track.
func drivePlay(t *testing.T, client *rtsp.Client) error {
	t.Helper()
	tracks, err := client.Describe(context.Background())
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		return errNoTracks
	}
	if err := client.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		return err
	}
	return client.Play(context.Background())
}

var errNoTracks = errors.New("no tracks described")

func parseCleanStream(t *testing.T, data []byte) {
	t.Helper()
	off := 0
	for off < len(data) {
		switch rtsp.ClassifyStream(data[off:]) {
		case rtsp.FrameInterleaved:
			_, n, err := rtsp.ParseInterleaved(data[off:])
			if err != nil {
				t.Fatalf("torn interleaved frame at offset %d: %v", off, err)
			}
			off += n
		case rtsp.FrameResponse:
			_, n, err := rtsp.ParseResponse(data[off:])
			if err != nil {
				t.Fatalf("torn response at offset %d: %v", off, err)
			}
			off += n
		default:
			t.Fatalf("unexpected bytes at offset %d", off)
		}
	}
}

func TestWriterInterleavesResponsesAtomically(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	var mu sync.Mutex
	var collected []byte
	go func() {
		tmp := make([]byte, 4096)
		for {
			_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := clientConn.Read(tmp)
			if n > 0 {
				mu.Lock()
				collected = append(collected, tmp[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	frames := NewChanSource(256)
	frames.SetActive(true)
	for range 100 {
		frames.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: time.Now()})
	}
	track := &Track{Path: "/stream", PayloadType: 96, Frames: frames}
	srv := New(Config{SRInterval: time.Hour}, track)
	ctx, cancel := context.WithCancel(context.Background())
	cs := &connSession{srv: srv, track: track, conn: serverConn, ctx: ctx, cancel: cancel, rtpCh: 0, rtcpCh: 1, startSeq: 1}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); cs.runWriter() }()
	for i := range 50 {
		cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: i})
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	_ = serverConn.Close()
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	_ = clientConn.Close()

	mu.Lock()
	data := append([]byte(nil), collected...)
	mu.Unlock()
	parseCleanStream(t, data)
}

func TestChanSourceBackpressure(t *testing.T) {
	c := NewChanSource(2)
	c.SetActive(true)
	f := pipeline.Frame{Payload: []byte{1, 2}, Duration: 1}
	if !c.Push(f) {
		t.Fatal("first push should succeed")
	}
	if !c.Push(f) {
		t.Fatal("second push should succeed")
	}
	if c.Push(f) {
		t.Error("third push should fail when the buffer is full")
	}
}

func TestWriterTearsDownOnWriteError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	_ = clientConn.Close() // writes on serverConn now fail

	frames := NewChanSource(4)
	frames.SetActive(true)
	frames.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: time.Now()})
	track := &Track{Path: "/stream", PayloadType: 96, Frames: frames}
	srv := New(Config{SRInterval: time.Hour}, track)
	ctx, cancel := context.WithCancel(context.Background())
	cs := &connSession{srv: srv, track: track, conn: serverConn, ctx: ctx, cancel: cancel, rtpCh: 0, rtcpCh: 1}

	done := make(chan struct{})
	go func() { cs.runWriter(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not tear down on a write error")
	}
	if ctx.Err() == nil {
		t.Error("teardown did not cancel the connection context")
	}
}
