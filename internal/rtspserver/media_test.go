package rtspserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	rtsp "github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
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
	ctx, cancel := context.WithCancel(t.Context())
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

	frames := NewFeed()
	spec := pipeline.SDPSpec(&config.Stream{Mode: config.ModePCM}, "test", rate, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	addr := serveWith(t, Config{SRInterval: 50 * time.Millisecond, Timeout: 30 * time.Second},
		&Track{Path: testPath, SDP: sdpBytes, PayloadType: 96, Feed: frames})

	var mu sync.Mutex
	var got []byte
	var pts []time.Duration
	done := make(chan struct{})
	var once sync.Once
	client, err := rtsp.Dial(t.Context(), rtsp.Config{
		URL:     "rtsp://" + addr + testPath,
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

	// Push only after PLAY: a frame pushed with no subscriber is discarded, and the fake
	// source is finite, so pumping earlier would discard every period. The stage
	// is gated on the feed exactly as the appliance wires it, so this also proves
	// PLAY opens the encode gate.
	stageErr := make(chan error, 1)
	go func() {
		stageErr <- pipeline.NewPCM().Run(fakeSrc, frames.Session, func(f pipeline.Frame) error {
			frames.Push(f)
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the ingest client to receive the audio")
	}

	select {
	case err := <-stageErr:
		if err != nil {
			t.Errorf("stage Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stage to finish")
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

	frames := NewFeed()
	spec := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Opus: config.Opus{Bitrate: 64000}}, "test", rate, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	addr := serveWith(t, Config{SRInterval: time.Hour, Timeout: 30 * time.Second},
		&Track{Path: testPath, SDP: sdpBytes, PayloadType: 97, Feed: frames})

	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pcm := make([]int16, 960)

	var mu sync.Mutex
	decoded := 0
	done := make(chan struct{})
	var once sync.Once
	client, err := rtsp.Dial(t.Context(), rtsp.Config{
		URL:     "rtsp://" + addr + testPath,
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

	// Push only after PLAY: a frame pushed with no subscriber is discarded, and the fake
	// source is finite, so pumping earlier would discard every period. The stage
	// is gated on the feed exactly as the appliance wires it.
	stageErr := make(chan error, 1)
	go func() {
		stageErr <- pipeline.NewOpus(config.Opus{Bitrate: 64000}).Run(fakeSrc, frames.Session, func(f pipeline.Frame) error {
			frames.Push(f)
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Opus frames")
	}

	select {
	case err := <-stageErr:
		if err != nil {
			t.Errorf("stage Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stage to finish")
	}
}

// drivePlay runs the client's DESCRIBE/SETUP/PLAY after Dial (which only does
// OPTIONS), selecting the first described track.
func drivePlay(t *testing.T, client *rtsp.Client) error {
	t.Helper()
	tracks, err := client.Describe(t.Context())
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		return errNoTracks
	}
	if err := client.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		return err
	}
	return client.Play(t.Context())
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

	frames := NewFeed()
	sub := mustSubscribe(t, frames)
	for range subQueueDepth {
		frames.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: time.Now()})
	}
	track := &Track{Path: testPath, PayloadType: 96, Feed: frames}
	srv := New(Config{SRInterval: time.Hour}, track)
	ctx, cancel := context.WithCancel(t.Context())
	cs := &connSession{srv: srv, track: track, conn: serverConn, ctx: ctx, cancel: cancel, rtpCh: 0, rtcpCh: 1, startSeq: 1, writerDone: make(chan struct{})}

	var wg sync.WaitGroup
	wg.Go(func() { cs.runWriter(sub) })
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

// TestWriterSenderReportPairsCaptureWithPacketTimestamp pins that a sender
// report names the capture time of the packet's first sample together with that
// packet's own RTP timestamp, not the timestamp advanced past it.
func TestWriterSenderReportPairsCaptureWithPacketTimestamp(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	const startTS = 123456
	frames := NewFeed()
	sub := mustSubscribe(t, frames)
	cap0 := time.Unix(1700000000, 123456789)
	frames.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: cap0})
	frames.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Pos: 160, Captured: cap0.Add(20 * time.Millisecond)})
	track := &Track{Path: testPath, PayloadType: 96, Feed: frames}
	srv := New(Config{SRInterval: time.Hour}, track)
	ctx, cancel := context.WithCancel(t.Context())
	cs := &connSession{srv: srv, track: track, conn: serverConn, ctx: ctx, cancel: cancel, rtpCh: 0, rtcpCh: 1, startSeq: 1, startTS: startTS, writerDone: make(chan struct{})}

	var wg sync.WaitGroup
	wg.Go(func() { cs.runWriter(sub) })
	defer func() {
		cancel()
		_ = clientConn.Close()
		wg.Wait()
	}()

	var pkt *rtp.Packet
	var sr *rtp.SenderReport
	var data []byte
	tmp := make([]byte, 4096)
	for pkt == nil || sr == nil {
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := clientConn.Read(tmp)
		if err != nil {
			t.Fatalf("read: %v (packet %v, sender report %v)", err, pkt != nil, sr != nil)
		}
		data = append(data, tmp[:n]...)
		for {
			f, used, perr := rtsp.ParseInterleaved(data)
			if perr != nil {
				break
			}
			data = data[used:]
			switch f.Channel {
			case 0:
				if pkt == nil {
					p, err := rtp.ParsePacket(f.Payload)
					if err != nil {
						t.Fatalf("ParsePacket: %v", err)
					}
					pkt = &p
				}
			case 1:
				pkts, err := rtp.ParseCompound(f.Payload)
				if err != nil {
					t.Fatalf("ParseCompound: %v", err)
				}
				if len(pkts) > 0 && sr == nil {
					sr = &pkts[0]
				}
			}
		}
	}
	if pkt.Header.Timestamp != startTS || sr.RTPTimestamp != startTS {
		t.Errorf("got packet timestamp %d and SR timestamp %d, want both %d", pkt.Header.Timestamp, sr.RTPTimestamp, startTS)
	}
	if want := rtp.NTPFromTime(cap0); sr.NTPTimestamp != want {
		t.Errorf("got SR NTP %d, want %d (first frame's capture time)", sr.NTPTimestamp, want)
	}
}

// TestWriterTimestampsLeaveGapForDroppedFrames pins that frames a subscriber's
// full queue dropped leave a gap in the RTP timestamps of exactly their length,
// counted from the first frame the connection sent, while sequence numbers stay
// consecutive and each sender report still pairs a packet's own timestamp with
// its capture time.
func TestWriterTimestampsLeaveGapForDroppedFrames(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	const (
		startTS = 123456
		base    = 1 << 33 // a stream position that is not zero and wider than 32 bits
		step    = 160
		lost    = 5 // frames dropped on top of a full queue
		after   = 3 // frames pushed once the writer has made room
	)
	cap0 := time.Unix(1700000000, 123456789)
	mk := func(i int) pipeline.Frame {
		return pipeline.Frame{
			Payload: make([]byte, 320), Duration: step, Pos: base + uint64(i)*step,
			Captured: cap0.Add(time.Duration(i) * 20 * time.Millisecond),
		}
	}
	frames := NewFeed()
	sub := mustSubscribe(t, frames)
	for i := range subQueueDepth {
		if frames.Push(mk(i)) != 0 {
			t.Fatalf("frame %d dropped with the queue not yet full", i)
		}
	}
	for i := range lost {
		if got := frames.Push(mk(subQueueDepth + i)); got != 1 {
			t.Fatalf("frame %d: got %d drops, want 1 (queue full)", subQueueDepth+i, got)
		}
	}
	track := &Track{Path: testPath, PayloadType: 96, Feed: frames}
	srv := New(Config{SRInterval: time.Nanosecond}, track)
	ctx, cancel := context.WithCancel(t.Context())
	cs := &connSession{srv: srv, track: track, conn: serverConn, ctx: ctx, cancel: cancel, rtpCh: 0, rtcpCh: 1, startSeq: 1, startTS: startTS, writerDone: make(chan struct{})}

	var wg sync.WaitGroup
	wg.Go(func() { cs.runWriter(sub) })
	defer func() {
		cancel()
		_ = clientConn.Close()
		wg.Wait()
	}()

	// Frames sent: the queued ones, then the ones pushed after the gap.
	sent := make([]int, 0, subQueueDepth+after)
	for i := range subQueueDepth {
		sent = append(sent, i)
	}
	for i := range after {
		sent = append(sent, subQueueDepth+lost+i)
	}
	var pkts []rtp.Header
	var srs []rtp.SenderReport
	var data []byte
	tmp := make([]byte, 4096)
	pushed := false
	for len(pkts) < len(sent) || len(srs) < len(sent) {
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := clientConn.Read(tmp)
		if err != nil {
			t.Fatalf("read: %v (%d packets, %d sender reports)", err, len(pkts), len(srs))
		}
		data = append(data, tmp[:n]...)
		for {
			f, used, perr := rtsp.ParseInterleaved(data)
			if perr != nil {
				break
			}
			data = data[used:]
			switch f.Channel {
			case 0:
				p, err := rtp.ParsePacket(f.Payload)
				if err != nil {
					t.Fatalf("ParsePacket: %v", err)
				}
				pkts = append(pkts, p.Header)
			case 1:
				cp, err := rtp.ParseCompound(f.Payload)
				if err != nil {
					t.Fatalf("ParseCompound: %v", err)
				}
				if len(cp) > 0 {
					srs = append(srs, cp[0])
				}
			}
		}
		// The writer has taken at least as many frames as were read, so the queue
		// has room for the post-gap frames.
		if len(pkts) >= after && !pushed {
			pushed = true
			for i := range after {
				if frames.Push(mk(subQueueDepth+lost+i)) != 0 {
					t.Fatalf("post-gap frame %d dropped", i)
				}
			}
		}
	}
	for k, i := range sent {
		h := pkts[k]
		if want := uint32(startTS + i*step); h.Timestamp != want {
			t.Errorf("packet %d (frame %d): got timestamp %d, want %d", k, i, h.Timestamp, want)
		}
		if want := pkts[0].SequenceNumber + uint16(k); h.SequenceNumber != want {
			t.Errorf("packet %d: got seq %d, want %d (consecutive across the gap)", k, h.SequenceNumber, want)
		}
		if sr := srs[k]; sr.RTPTimestamp != h.Timestamp || sr.NTPTimestamp != rtp.NTPFromTime(mk(i).Captured) {
			t.Errorf("sender report %d: got RTP %d NTP %d, want RTP %d NTP %d", k, sr.RTPTimestamp, sr.NTPTimestamp, h.Timestamp, rtp.NTPFromTime(mk(i).Captured))
		}
	}
}

func TestWriterTearsDownOnWriteError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	_ = clientConn.Close() // writes on serverConn now fail

	frames := NewFeed()
	sub := mustSubscribe(t, frames)
	frames.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: time.Now()})
	track := &Track{Path: testPath, PayloadType: 96, Feed: frames}
	srv := New(Config{SRInterval: time.Hour}, track)
	ctx, cancel := context.WithCancel(t.Context())
	cs := &connSession{srv: srv, track: track, conn: serverConn, ctx: ctx, cancel: cancel, rtpCh: 0, rtcpCh: 1, writerDone: make(chan struct{})}

	done := make(chan struct{})
	go func() { cs.runWriter(sub); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not tear down on a write error")
	}
	if ctx.Err() == nil {
		t.Error("teardown did not cancel the connection context")
	}
}

// pacedSource delays each Read, standing in for a sound card's clock so a
// second client can join while the stream is running.
type pacedSource struct {
	audio.Source
	every time.Duration
}

func (p pacedSource) Read() (audio.Period, error) {
	time.Sleep(p.every)
	return p.Source.Read()
}

// TestOpusLateJoinerDecodesWithoutResettingTheEncoder pins the multi-client
// Opus contract end to end with the real ingest client: a second client joining
// a running stream decodes it from its first packet, and the join leaves the
// play session (and so the shared encoder) alone.
func TestOpusLateJoinerDecodesWithoutResettingTheEncoder(t *testing.T) {
	const rate, periodFrames, periods = 48000, 960, 600
	base, _ := sineSource(periods, periodFrames, rate)
	src := pacedSource{Source: base, every: 2 * time.Millisecond}

	feed := NewFeed()
	spec := pipeline.SDPSpec(&config.Stream{Mode: config.ModeOpus, Opus: config.Opus{Bitrate: 64000}}, "test", rate, 1)
	sdpBytes, err := sdp.WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	addr := serveWith(t, Config{SRInterval: time.Hour, Timeout: 30 * time.Second},
		&Track{Path: testPath, SDP: sdpBytes, PayloadType: 97, Feed: feed})

	// newClient plays the stream and counts the Opus frames it decodes cleanly.
	newClient := func() (decoded *atomic.Int32) {
		t.Helper()
		dec, derr := opus.NewDecoder(48000, 1)
		if derr != nil {
			t.Fatalf("NewDecoder: %v", derr)
		}
		pcm := make([]int16, 960)
		decoded = new(atomic.Int32)
		var mu sync.Mutex
		client, cerr := rtsp.Dial(t.Context(), rtsp.Config{
			URL:     "rtsp://" + addr + testPath,
			Timeout: 5 * time.Second,
			OnFrame: func(fr audiostream.Frame) {
				mu.Lock()
				defer mu.Unlock()
				if n, e := dec.Decode(fr.Data, pcm); e != nil || n != 960 {
					t.Errorf("decode: %d samples, err %v, want 960 and nil", n, e)
					return
				}
				decoded.Add(1)
			},
		})
		if cerr != nil {
			t.Fatalf("Dial: %v", cerr)
		}
		t.Cleanup(func() { _ = client.Close() })
		if perr := drivePlay(t, client); perr != nil {
			t.Fatalf("play handshake: %v", perr)
		}
		return decoded
	}

	first := newClient()
	stageErr := make(chan error, 1)
	go func() {
		stageErr <- pipeline.NewOpus(config.Opus{Bitrate: 64000}).Run(src, feed.Session, func(f pipeline.Frame) error {
			feed.Push(f)
			return nil
		})
	}()
	waitFor(t, func() bool { return first.Load() >= 20 }, 10*time.Second, "the first client to decode 20 frames")

	_, sessionBefore := feed.Session()
	second := newClient()
	waitFor(t, func() bool { return second.Load() >= 20 }, 10*time.Second, "the late joiner to decode 20 frames")
	if _, after := feed.Session(); after != sessionBefore {
		t.Errorf("play session went from %d to %d when a second client joined, want it kept", sessionBefore, after)
	}
	if got := feed.Clients(); got != 2 {
		t.Errorf("Clients() = %d, want 2", got)
	}

	select {
	case err := <-stageErr:
		if err != nil {
			t.Errorf("stage Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the stage to finish")
	}
}
