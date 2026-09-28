package rtspserver

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	rtsp "github.com/tphakala/go-audio-stream/rtsp"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

var testSDP = []byte("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns= \r\nc=IN IP4 0.0.0.0\r\nt=0 0\r\n" +
	"m=audio 0 RTP/AVP 96\r\na=rtpmap:96 L16/256000/1\r\na=control:trackID=0\r\n")

const testPath = "/stream"

func baseURL(addr string) string  { return "rtsp://" + addr + testPath }
func trackURL(addr string) string { return baseURL(addr) + "/trackID=0" }

func defaultTrack() *Track {
	return &Track{Path: testPath, SDP: testSDP, PayloadType: 96, Feed: NewFeed()}
}

//nolint:gocritic // test helper; Config by value is fine.
func startServer(t *testing.T, cfg Config, tracks ...*Track) (string, *Server) {
	t.Helper()
	srv := New(cfg, tracks...)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serveConn(context.Background(), conn)
		}
	}()
	return ln.Addr().String(), srv
}

type client struct {
	conn net.Conn
	buf  []byte
	tmp  []byte
	cseq int
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &client{conn: conn, tmp: make([]byte, 4096)}
}

func (c *client) do(t *testing.T, method, rawURL string, hdr rtsp.Header) *rtsp.Response {
	t.Helper()
	c.cseq++
	data, err := rtsp.MarshalRequest(&rtsp.Request{Method: method, URL: rawURL, CSeq: c.cseq, Header: hdr})
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := c.conn.Write(data); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	for {
		if resp, n, perr := rtsp.ParseResponse(c.buf); perr == nil {
			c.buf = c.buf[n:]
			return resp
		} else if !errors.Is(perr, rtsp.ErrIncomplete) {
			t.Fatalf("parse response: %v", perr)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		m, rerr := c.conn.Read(c.tmp)
		if m > 0 {
			c.buf = append(c.buf, c.tmp[:m]...)
		}
		if rerr != nil {
			t.Fatalf("read response: %v", rerr)
		}
	}
}

func tcpTransport(pair string) rtsp.Header {
	h := rtsp.Header{}
	h.Set("Transport", "RTP/AVP/TCP;unicast;interleaved="+pair)
	return h
}

func TestControlHappyPath(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	base := baseURL(addr)
	c := dial(t, addr)

	r := c.do(t, "OPTIONS", base, nil)
	if r.StatusCode != 200 {
		t.Fatalf("OPTIONS = %d", r.StatusCode)
	}
	if !strings.Contains(r.Header.Get("Public"), "GET_PARAMETER") {
		t.Errorf("Public header missing GET_PARAMETER: %q", r.Header.Get("Public"))
	}

	r = c.do(t, "DESCRIBE", base, nil)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/sdp" {
		t.Fatalf("DESCRIBE = %d, content-type %q", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if !bytes.Equal(r.Body, testSDP) {
		t.Error("DESCRIBE body is not the configured SDP")
	}
	if !strings.HasSuffix(r.Header.Get("Content-Base"), "/stream/") {
		t.Errorf("Content-Base = %q", r.Header.Get("Content-Base"))
	}

	r = c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if r.StatusCode != 200 {
		t.Fatalf("SETUP = %d", r.StatusCode)
	}
	if !strings.Contains(r.Header.Get("Transport"), "interleaved=0-1") {
		t.Errorf("SETUP Transport = %q", r.Header.Get("Transport"))
	}
	if !strings.Contains(r.Header.Get("Session"), "timeout=60") {
		t.Errorf("SETUP Session = %q", r.Header.Get("Session"))
	}
	sessID, _, _ := strings.Cut(r.Header.Get("Session"), ";")
	if sessID == "" {
		t.Fatal("SETUP returned no session id")
	}

	sess := rtsp.Header{}
	sess.Set("Session", sessID)
	r = c.do(t, "PLAY", base, sess)
	if r.StatusCode != 200 {
		t.Fatalf("PLAY = %d", r.StatusCode)
	}
	if r.Header.Get("Range") != "npt=0.000-" {
		t.Errorf("PLAY Range = %q", r.Header.Get("Range"))
	}
	info := r.Header.Get("RTP-Info")
	if !strings.Contains(info, "seq=") || !strings.Contains(info, "rtptime=") || !strings.Contains(info, "trackID=0") {
		t.Errorf("PLAY RTP-Info = %q", info)
	}

	r = c.do(t, "GET_PARAMETER", base, sess)
	if r.StatusCode != 200 {
		t.Errorf("GET_PARAMETER = %d", r.StatusCode)
	}

	r = c.do(t, "TEARDOWN", base, sess)
	if r.StatusCode != 200 {
		t.Errorf("TEARDOWN = %d", r.StatusCode)
	}
}

func TestSetupEchoesClientChannels(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	c := dial(t, addr)
	r := c.do(t, "SETUP", trackURL(addr), tcpTransport("2-3"))
	if r.StatusCode != 200 {
		t.Fatalf("SETUP = %d", r.StatusCode)
	}
	if !strings.Contains(r.Header.Get("Transport"), "interleaved=2-3") {
		t.Errorf("Transport did not echo client channels: %q", r.Header.Get("Transport"))
	}
}

func TestSetupRejectsUDP(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	c := dial(t, addr)
	h := rtsp.Header{}
	h.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	r := c.do(t, "SETUP", trackURL(addr), h)
	if r.StatusCode != 461 {
		t.Errorf("UDP SETUP = %d, want 461", r.StatusCode)
	}
}

// TestTwoClientsPlayOnePath pins the multi-consumer contract: two connections
// SETUP and PLAY the same path and both receive the frames pushed to it.
func TestTwoClientsPlayOnePath(t *testing.T) {
	track := defaultTrack()
	feed := track.Feed.(*Feed) //nolint:forcetypeassert // defaultTrack builds a *Feed
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour}, track)

	c1, c2 := dial(t, addr), dial(t, addr)
	setupAndPlay(t, c1, addr)
	setupAndPlay(t, c2, addr)
	waitFor(t, func() bool { return feed.Clients() == 2 }, 2*time.Second, "both clients to subscribe")

	feed.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: time.Now()})
	for i, c := range []*client{c1, c2} {
		if !firstByteIs(t, c.conn, '$', 2*time.Second) {
			t.Errorf("client %d received no RTP frame", i+1)
		}
	}
}

func TestSetupDuringPlayRejected(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	base := baseURL(addr)
	c := dial(t, addr)

	r := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if r.StatusCode != 200 {
		t.Fatalf("SETUP = %d", r.StatusCode)
	}
	sessID, _, _ := strings.Cut(r.Header.Get("Session"), ";")
	sess := rtsp.Header{}
	sess.Set("Session", sessID)
	if r := c.do(t, "PLAY", base, sess); r.StatusCode != 200 {
		t.Fatalf("PLAY = %d", r.StatusCode)
	}

	// A re-SETUP on the already-playing session must be rejected (RFC 2326 A.1)
	// rather than re-randomizing startSeq/startTS under the running writer.
	if r := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1")); r.StatusCode != 455 {
		t.Errorf("SETUP during PLAY = %d, want 455", r.StatusCode)
	}
}

func TestUnknownPath404(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	c := dial(t, addr)
	if r := c.do(t, "DESCRIBE", "rtsp://"+addr+"/wrong", nil); r.StatusCode != 404 {
		t.Errorf("DESCRIBE /wrong = %d, want 404", r.StatusCode)
	}
}

func TestPlayWithoutSession(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	c := dial(t, addr)
	if r := c.do(t, "PLAY", baseURL(addr), nil); r.StatusCode != 454 {
		t.Errorf("PLAY without SETUP = %d, want 454", r.StatusCode)
	}
}

func TestUnknownMethod(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	c := dial(t, addr)
	// RECORD has a ClassifyStream-recognized prefix but is not implemented.
	if r := c.do(t, "RECORD", baseURL(addr), nil); r.StatusCode != 501 {
		t.Errorf("RECORD = %d, want 501", r.StatusCode)
	}
}

func TestKeepaliveRefreshesDeadline(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 300 * time.Millisecond}, defaultTrack())
	base := baseURL(addr)
	c := dial(t, addr)

	// Keepalive at half the timeout keeps the connection alive.
	for i := range 3 {
		time.Sleep(150 * time.Millisecond)
		if r := c.do(t, "GET_PARAMETER", base, nil); r.StatusCode != 200 {
			t.Fatalf("keepalive %d = %d", i, r.StatusCode)
		}
	}
	// Going silent past the timeout closes the connection.
	time.Sleep(600 * time.Millisecond)
	_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.conn.Read(make([]byte, 1)); err == nil {
		t.Error("connection stayed open past the idle timeout")
	}
}

func TestTracksRouteIndependently(t *testing.T) {
	sdpB := bytes.Replace(testSDP, []byte("L16/256000/1"), []byte("L16/48000/1"), 1)
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second},
		&Track{Path: "/a", SDP: testSDP, PayloadType: 96, Feed: NewFeed()},
		&Track{Path: "/b", SDP: sdpB, PayloadType: 96, Feed: NewFeed()},
	)
	c := dial(t, addr)
	ra := c.do(t, "DESCRIBE", "rtsp://"+addr+"/a", nil)
	if ra.StatusCode != 200 || !bytes.Equal(ra.Body, testSDP) {
		t.Fatalf("DESCRIBE /a = %d, body match %v", ra.StatusCode, bytes.Equal(ra.Body, testSDP))
	}
	if !strings.HasSuffix(ra.Header.Get("Content-Base"), "/a/") {
		t.Errorf("Content-Base for /a = %q", ra.Header.Get("Content-Base"))
	}
	rb := c.do(t, "DESCRIBE", "rtsp://"+addr+"/b", nil) // DESCRIBE is stateless: same conn may probe both
	if rb.StatusCode != 200 || !bytes.Equal(rb.Body, sdpB) {
		t.Fatalf("DESCRIBE /b = %d, body match %v", rb.StatusCode, bytes.Equal(rb.Body, sdpB))
	}
}

func TestSetupSecondTrackSameConnRejected(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second},
		&Track{Path: "/a", SDP: testSDP, PayloadType: 96, Feed: NewFeed()},
		&Track{Path: "/b", SDP: testSDP, PayloadType: 96, Feed: NewFeed()},
	)
	c := dial(t, addr)
	if r := c.do(t, "SETUP", "rtsp://"+addr+"/a/trackID=0", tcpTransport("0-1")); r.StatusCode != 200 {
		t.Fatalf("SETUP /a = %d", r.StatusCode)
	}
	if r := c.do(t, "SETUP", "rtsp://"+addr+"/b/trackID=0", tcpTransport("2-3")); r.StatusCode != 455 {
		t.Errorf("cross-track SETUP = %d, want 455", r.StatusCode)
	}
}

func TestSetupSameTrackOnSeveralConnections(t *testing.T) {
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second},
		&Track{Path: "/a", SDP: testSDP, PayloadType: 96, Feed: NewFeed()},
		&Track{Path: "/b", SDP: testSDP, PayloadType: 96, Feed: NewFeed()},
	)
	for i, path := range []string{"/a", "/b", "/a"} {
		c := dial(t, addr)
		if r := c.do(t, "SETUP", "rtsp://"+addr+path+"/trackID=0", tcpTransport("0-1")); r.StatusCode != 200 {
			t.Errorf("connection %d: SETUP %s = %d, want 200", i+1, path, r.StatusCode)
		}
	}
}

func TestRemovedTrack404(t *testing.T) {
	addr, srv := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())
	srv.RemoveTrack(testPath)
	c := dial(t, addr)
	if r := c.do(t, "DESCRIBE", baseURL(addr), nil); r.StatusCode != 404 {
		t.Errorf("DESCRIBE removed track = %d, want 404", r.StatusCode)
	}
}

// recordingListener captures the Listener callbacks the server makes, safe for
// the read goroutine to write and the test goroutine to read.
type recordingListener struct {
	mu    sync.Mutex
	conns []connEvent
	disc  []discEvent
}

type connEvent struct {
	path   string
	remote string
}

type discEvent struct {
	path   string
	remote string
	reason DisconnectReason
}

func (r *recordingListener) ClientConnected(path, remote string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns = append(r.conns, connEvent{path: path, remote: remote})
}

func (r *recordingListener) ClientDisconnected(path, remote string, reason DisconnectReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disc = append(r.disc, discEvent{path: path, remote: remote, reason: reason})
}

func (r *recordingListener) connectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *recordingListener) connects() []connEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]connEvent(nil), r.conns...)
}

func (r *recordingListener) disconnects() []discEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]discEvent(nil), r.disc...)
}

// waitFor polls cond until it holds or the timeout elapses, so a test can wait
// on an event delivered from the server's read goroutine without a fixed sleep.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// playingTrack is a track with a live frame source, so a PLAY starts the writer
// and the listener sees a connect.
func playingTrack() *Track {
	return &Track{Path: testPath, SDP: testSDP, PayloadType: 96, Feed: NewFeed()}
}

// setupAndPlay runs SETUP then PLAY on c and returns the session header, failing
// the test on any non-200.
func setupAndPlay(t *testing.T, c *client, addr string) rtsp.Header {
	t.Helper()
	setup := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if setup.StatusCode != 200 {
		t.Fatalf("SETUP = %d", setup.StatusCode)
	}
	sess := rtsp.Header{}
	sess.Set("Session", setup.Header.Get("Session"))
	if r := c.do(t, "PLAY", baseURL(addr), sess); r.StatusCode != 200 {
		t.Fatalf("PLAY = %d", r.StatusCode)
	}
	return sess
}

func TestListenerConnectThenTeardown(t *testing.T) {
	rec := &recordingListener{}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, playingTrack())
	c := dial(t, addr)

	sess := setupAndPlay(t, c, addr)
	// The server sees the client's local address as the remote. Capture it before
	// any close so the connect and disconnect events can be checked against it.
	wantRemote := c.conn.LocalAddr().String()
	waitFor(t, func() bool { return rec.connectCount() == 1 }, 2*time.Second, "a connect on PLAY")
	if got := rec.connects()[0]; got.path != testPath || got.remote != wantRemote {
		t.Errorf("connect = %+v, want path %q remote %q", got, testPath, wantRemote)
	}

	if r := c.do(t, "TEARDOWN", baseURL(addr), sess); r.StatusCode != 200 {
		t.Fatalf("TEARDOWN = %d", r.StatusCode)
	}
	waitFor(t, func() bool { return len(rec.disconnects()) == 1 }, 2*time.Second, "a disconnect on TEARDOWN")
	d := rec.disconnects()[0]
	if d.path != testPath || d.remote != wantRemote {
		t.Errorf("disconnect = %+v, want path %q remote %q", d, testPath, wantRemote)
	}
	if d.reason != DisconnectTeardown {
		t.Errorf("disconnect reason = %v, want teardown", d.reason)
	}
}

func TestListenerDisconnectOnConnectionDrop(t *testing.T) {
	rec := &recordingListener{}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, playingTrack())
	c := dial(t, addr)

	setupAndPlay(t, c, addr)
	wantRemote := c.conn.LocalAddr().String() // capture before the drop closes the conn
	waitFor(t, func() bool { return rec.connectCount() == 1 }, 2*time.Second, "a connect on PLAY")
	if got := rec.connects()[0]; got.remote != wantRemote {
		t.Errorf("connect remote = %q, want %q", got.remote, wantRemote)
	}

	_ = c.conn.Close() // abrupt drop, no TEARDOWN
	waitFor(t, func() bool { return len(rec.disconnects()) == 1 }, 2*time.Second, "a disconnect on the dropped connection")
	d := rec.disconnects()[0]
	if d.reason != DisconnectReadError {
		t.Errorf("disconnect reason = %v, want read error", d.reason)
	}
	if d.remote != wantRemote {
		t.Errorf("disconnect remote = %q, want %q", d.remote, wantRemote)
	}
}

func TestListenerDisconnectOnEviction(t *testing.T) {
	rec := &recordingListener{}
	g := auth.NewGuard("")
	frames := NewFeed()
	track := &Track{Path: testPath, SDP: testSDP, PayloadType: 96, Feed: frames}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Auth: g, Listener: rec}, track)
	c := dial(t, addr)

	setupAndPlay(t, c, addr)
	wantRemote := c.conn.LocalAddr().String()
	waitFor(t, func() bool { return rec.connectCount() == 1 }, 2*time.Second, "a connect on PLAY")
	if got := rec.connects()[0]; got.remote != wantRemote {
		t.Errorf("connect remote = %q, want %q", got.remote, wantRemote)
	}

	// Feed audio continuously so the writer loops and checks shouldEvict.
	pumpFrames(t, frames)

	// Enabling a token evicts the open-access session proactively in the writer.
	g.Set(testAuthToken)
	waitFor(t, func() bool { return len(rec.disconnects()) == 1 }, 3*time.Second, "a disconnect on eviction")
	d := rec.disconnects()[0]
	if d.reason != DisconnectEvicted {
		t.Errorf("disconnect reason = %v, want evicted", d.reason)
	}
	if d.remote != wantRemote {
		t.Errorf("disconnect remote = %q, want %q", d.remote, wantRemote)
	}
}

func TestListenerSetupOnlyEmitsNothing(t *testing.T) {
	rec := &recordingListener{}
	track := playingTrack()
	feed := track.Feed.(*Feed) //nolint:forcetypeassert // playingTrack builds a *Feed
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, track)
	c := dial(t, addr)

	if r := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1")); r.StatusCode != 200 {
		t.Fatalf("SETUP = %d", r.StatusCode)
	}
	if got := feed.Clients(); got != 0 {
		t.Fatalf("Clients() = %d after a SETUP, want 0", got)
	}
	_ = c.conn.Close()

	// A second connection plays and drops after the first was closed. Its
	// disconnect is the only event either connection may produce, and seeing it
	// means the first connection's cleanup has had its turn.
	c2 := dial(t, addr)
	setupAndPlay(t, c2, addr)
	wantRemote := c2.conn.LocalAddr().String()
	waitFor(t, func() bool { return rec.connectCount() == 1 }, 2*time.Second, "the second connection's connect")
	_ = c2.conn.Close()
	waitFor(t, func() bool { return len(rec.disconnects()) >= 1 }, 2*time.Second, "the second connection's disconnect")
	if got := rec.connects(); len(got) != 1 || got[0].remote != wantRemote {
		t.Errorf("connects = %+v, want only the connection that played (%s)", got, wantRemote)
	}
	if got := rec.disconnects(); len(got) != 1 || got[0].remote != wantRemote {
		t.Errorf("disconnects = %+v, want only the connection that played (%s)", got, wantRemote)
	}
}
