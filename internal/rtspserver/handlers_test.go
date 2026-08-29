package rtspserver

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	rtsp "github.com/tphakala/go-audio-stream/rtsp"
)

var testSDP = []byte("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns= \r\nc=IN IP4 0.0.0.0\r\nt=0 0\r\n" +
	"m=audio 0 RTP/AVP 96\r\na=rtpmap:96 L16/256000/1\r\na=control:trackID=0\r\n")

const testPath = "/stream"

func baseURL(addr string) string  { return "rtsp://" + addr + testPath }
func trackURL(addr string) string { return baseURL(addr) + "/trackID=0" }

//nolint:gocritic // test helper; Config by value is fine.
func startServer(t *testing.T, cfg Config) string {
	t.Helper()
	srv := New(cfg, nil)
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
	return ln.Addr().String()
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
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
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
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
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
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
	c := dial(t, addr)
	h := rtsp.Header{}
	h.Set("Transport", "RTP/AVP;unicast;client_port=5000-5001")
	r := c.do(t, "SETUP", trackURL(addr), h)
	if r.StatusCode != 461 {
		t.Errorf("UDP SETUP = %d, want 461", r.StatusCode)
	}
}

func TestSecondSetupRejected(t *testing.T) {
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
	track := trackURL(addr)

	c1 := dial(t, addr)
	if r := c1.do(t, "SETUP", track, tcpTransport("0-1")); r.StatusCode != 200 {
		t.Fatalf("first SETUP = %d", r.StatusCode)
	}
	c2 := dial(t, addr)
	if r := c2.do(t, "SETUP", track, tcpTransport("0-1")); r.StatusCode != 453 {
		t.Errorf("second SETUP = %d, want 453", r.StatusCode)
	}
}

func TestUnknownPath404(t *testing.T) {
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
	c := dial(t, addr)
	if r := c.do(t, "DESCRIBE", "rtsp://"+addr+"/wrong", nil); r.StatusCode != 404 {
		t.Errorf("DESCRIBE /wrong = %d, want 404", r.StatusCode)
	}
}

func TestPlayWithoutSession(t *testing.T) {
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
	c := dial(t, addr)
	if r := c.do(t, "PLAY", baseURL(addr), nil); r.StatusCode != 454 {
		t.Errorf("PLAY without SETUP = %d, want 454", r.StatusCode)
	}
}

func TestUnknownMethod(t *testing.T) {
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 60 * time.Second})
	c := dial(t, addr)
	// RECORD has a ClassifyStream-recognized prefix but is not implemented.
	if r := c.do(t, "RECORD", baseURL(addr), nil); r.StatusCode != 501 {
		t.Errorf("RECORD = %d, want 501", r.StatusCode)
	}
}

func TestKeepaliveRefreshesDeadline(t *testing.T) {
	addr := startServer(t, Config{Path: testPath, SDP: testSDP, Timeout: 300 * time.Millisecond})
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
