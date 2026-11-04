package mgmtserver

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedirectTarget(t *testing.T) {
	t.Parallel()
	leaf := &x509.Certificate{
		DNSNames: []string{"remote-mic", "remote-mic.local", "*.example.com"},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"), net.ParseIP("::1"),
			net.ParseIP("192.0.2.10"), net.ParseIP("fe80::1"),
		},
	}
	const (
		defaultLocal = "192.0.2.10:8443"
		localRoot    = "https://192.0.2.10:8443/"
		micRoot      = "https://remote-mic.local:8443/"
		spoofedGet   = "GET / HTTP/1.1\r\nHost: evil.example"
	)
	cases := []struct {
		name       string
		request    string // request line plus headers, without the final blank line
		local      string // local address, defaultLocal when empty
		noLeaf     bool
		want       string
		wantStatus int
	}{
		{
			name:       "allowlisted name keeps path and query",
			request:    "GET /api/v1/healthz?x=1 HTTP/1.1\r\nHost: remote-mic.local:8443",
			want:       "https://remote-mic.local:8443/api/v1/healthz?x=1",
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "case and trailing dot are canonicalised, not echoed",
			request:    "GET / HTTP/1.1\r\nHost: Remote-Mic.LOCAL.",
			want:       micRoot,
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "wildcard SAN of an operator certificate",
			request:    "GET / HTTP/1.1\r\nHost: mic.example.com",
			want:       "https://mic.example.com:8443/",
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "bracketed IPv6 in the certificate",
			request:    "GET / HTTP/1.1\r\nHost: [::1]:8443",
			want:       "https://[::1]:8443/",
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "empty label is rejected although VerifyHostname would match",
			request:    "GET / HTTP/1.1\r\nHost: remote-mic.local..",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "label charset is rejected although the wildcard would match",
			request:    "GET / HTTP/1.1\r\nHost: _x.example.com",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "unbracketed IPv6 is rejected",
			request:    "GET / HTTP/1.1\r\nHost: ::1",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "bracketed name is rejected",
			request:    "GET / HTTP/1.1\r\nHost: [remote-mic.local]",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "non-numeric port is rejected",
			request:    "GET / HTTP/1.1\r\nHost: remote-mic.local:x",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "IPv6 zone is rejected although VerifyHostname would match",
			request:    "GET / HTTP/1.1\r\nHost: [fe80::1%25eth0]:8443",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "unescaped IPv6 zone is rejected",
			request:    "GET / HTTP/1.1\r\nHost: [fe80::1%eth0]",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "spoofed host falls back to the local address",
			request:    "GET /x HTTP/1.1\r\nHost: evil.example",
			want:       "https://192.0.2.10:8443/x",
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "userinfo-shaped host fails the charset check",
			request:    "GET / HTTP/1.1\r\nHost: remote-mic.local@evil.example",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "empty host",
			request:    "GET / HTTP/1.0",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "no leaf means no allowlist",
			request:    "GET / HTTP/1.1\r\nHost: remote-mic.local",
			noLeaf:     true,
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "v4-mapped local address is unmapped",
			request:    spoofedGet,
			local:      "[::ffff:192.0.2.10]:8443",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "IPv6 local address is bracketed",
			request:    spoofedGet,
			local:      "[2001:db8::2]:8443",
			want:       "https://[2001:db8::2]:8443/",
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:    "zoned local address and spoofed host leave no target",
			request: spoofedGet,
			local:   "[fe80::2%eth0]:8443",
		},
		{
			name:       "verified host survives a zoned local address",
			request:    "GET / HTTP/1.1\r\nHost: remote-mic.local",
			local:      "[fe80::2%eth0]:8443",
			want:       micRoot,
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "absolute-form target is not reflected",
			request:    "GET http://evil.example/x HTTP/1.1\r\nHost: evil.example",
			want:       localRoot,
			wantStatus: http.StatusTemporaryRedirect,
		},
		{
			name:       "asterisk target",
			request:    "OPTIONS * HTTP/1.1\r\nHost: remote-mic.local",
			want:       micRoot,
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "non-ASCII target falls back to the root path",
			request:    "GET /caf\xc3\xa9 HTTP/1.1\r\nHost: remote-mic.local",
			want:       micRoot,
			wantStatus: http.StatusPermanentRedirect,
		},
		{
			name:       "double slash stays a path on our host",
			request:    "GET //evil.example/x HTTP/1.1\r\nHost: remote-mic.local",
			want:       "https://remote-mic.local:8443//evil.example/x",
			wantStatus: http.StatusPermanentRedirect,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(tc.request + "\r\n\r\n")))
			if err != nil {
				t.Fatalf("ReadRequest: %v", err)
			}
			local := tc.local
			if local == "" {
				local = defaultLocal
			}
			l := leaf
			if tc.noLeaf {
				l = nil
			}
			got, status := redirectTarget(req, l, tcpAddr(t, local))
			if got != tc.want || status != tc.wantStatus {
				t.Errorf("redirectTarget = (%q, %d), want (%q, %d)", got, status, tc.want, tc.wantStatus)
			}
		})
	}
}

func tcpAddr(t *testing.T, s string) net.Addr {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatal(err)
	}
	return net.TCPAddrFromAddrPort(ap)
}

// lineWriter sends every log line it receives on a channel.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	select {
	case w <- string(p):
	default: // the test is not listening; never block the server
	}
	return len(p), nil
}

// newRedirectServer starts a TLS test server whose listener redirects plain
// HTTP, with a short handshake deadline so the silent-client case is quick. A
// nil certificate uses the server's own, which covers example.com and
// 127.0.0.1. Each test gets its own server so log lines never cross tests.
func newRedirectServer(t *testing.T, certificate func() (*tls.Certificate, error)) (*httptest.Server, lineWriter) {
	t.Helper()
	lines := make(lineWriter, 16)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	ts.Config.ReadHeaderTimeout = time.Second
	ts.Config.ErrorLog = log.New(lines, "", 0)
	if certificate == nil {
		certificate = func() (*tls.Certificate, error) { return &ts.TLS.Certificates[0], nil }
	}
	ts.Listener = RedirectPlainHTTP(ts.Listener, certificate)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, lines
}

// plainReply is the part of a response the plain-HTTP tests check.
type plainReply struct {
	status        int
	header        http.Header
	close         bool
	contentLength int64
}

// plainExchange sends raw to the server over plain TCP and returns the parsed
// response, or nil when the server closed without answering. Any read error
// other than EOF, a reset included, or bytes after the response, fail the test.
func plainExchange(t *testing.T, ts *httptest.Server, raw string) *plainReply {
	t.Helper()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		// http.ReadResponse reports an empty stream as an unexpected EOF.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		t.Fatalf("reading the response: %v", err)
	}
	_ = resp.Body.Close()
	rest, err := io.ReadAll(br)
	if len(rest) != 0 || err != nil {
		t.Fatalf("after the redirect: %d extra bytes, err %v; want a clean close", len(rest), err)
	}
	return &plainReply{resp.StatusCode, resp.Header, resp.Close, resp.ContentLength}
}

func serverPort(ts *httptest.Server) string {
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
	return port
}

// nextLine returns the next line the server logged.
func nextLine(t *testing.T, lines lineWriter) string {
	t.Helper()
	select {
	case line := <-lines:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("net/http logged nothing for the connection")
		return ""
	}
}

func TestRedirectPlainHTTP(t *testing.T) {
	t.Parallel()

	t.Run("plain GET is redirected", func(t *testing.T) {
		t.Parallel()
		ts, _ := newRedirectServer(t, nil)
		port := serverPort(ts)
		resp := plainExchange(t, ts, "GET /a?b=1 HTTP/1.1\r\nHost: example.com:"+port+"\r\n\r\n")
		if resp == nil {
			t.Fatal("no response")
		}
		if resp.status != http.StatusPermanentRedirect {
			t.Errorf("status %d, want 308", resp.status)
		}
		// example.com differs from the local IP, so a broken certificate lookup
		// would give the 127.0.0.1 fallback instead.
		if got, want := resp.header.Get("Location"), "https://example.com:"+port+"/a?b=1"; got != want {
			t.Errorf("Location %q, want %q", got, want)
		}
		if got := resp.header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control %q, want no-store", got)
		}
		if !resp.close || resp.contentLength != 0 {
			t.Errorf("Close=%v ContentLength=%d, want a bodiless Connection: close response", resp.close, resp.contentLength)
		}
	})

	t.Run("unverified host is redirected with 307 to the local IP", func(t *testing.T) {
		t.Parallel()
		ts, _ := newRedirectServer(t, nil)
		resp := plainExchange(t, ts, "GET /a HTTP/1.1\r\nHost: evil.example\r\n\r\n")
		if resp == nil || resp.status != http.StatusTemporaryRedirect {
			t.Fatalf("response %v, want 307", resp)
		}
		if got, want := resp.header.Get("Location"), "https://127.0.0.1:"+serverPort(ts)+"/a"; got != want {
			t.Errorf("Location %q, want %q", got, want)
		}
		if got := resp.header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control %q, want no-store", got)
		}
	})

	t.Run("certificate swap applies to the next connection", func(t *testing.T) {
		t.Parallel()
		var current atomic.Pointer[tls.Certificate]
		current.Store(&tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"other.example"}}})
		ts, _ := newRedirectServer(t, func() (*tls.Certificate, error) { return current.Load(), nil })
		port := serverPort(ts)
		const raw = "GET / HTTP/1.1\r\nHost: swapped.example\r\n\r\n"
		resp := plainExchange(t, ts, raw)
		if resp == nil || resp.header.Get("Location") != "https://127.0.0.1:"+port+"/" {
			t.Fatalf("before the swap: %v, want the 127.0.0.1 fallback", resp)
		}
		current.Store(&tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"swapped.example"}}})
		resp = plainExchange(t, ts, raw)
		if resp == nil || resp.header.Get("Location") != "https://swapped.example:"+port+"/" {
			t.Fatalf("after the swap: %v, want swapped.example", resp)
		}
	})

	t.Run("request body is drained, not reset", func(t *testing.T) {
		t.Parallel()
		ts, _ := newRedirectServer(t, nil)
		const size = 32 << 10
		// One write, so the body is in flight when the server answers. Without
		// the half-close and drain the server closes with unread input, and the
		// client reads ECONNRESET instead of a clean close (plainExchange fails
		// on any read error).
		raw := "POST /api/v1/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: " + strconv.Itoa(size) + "\r\n\r\n" + strings.Repeat("x", size)
		resp := plainExchange(t, ts, raw)
		if resp == nil || resp.status != http.StatusPermanentRedirect {
			t.Fatalf("response %v, want 308", resp)
		}
	})

	t.Run("malformed plain request is not answered and still logs", func(t *testing.T) {
		t.Parallel()
		ts, lines := newRedirectServer(t, nil)
		// The bad header line carries a marker and the sentinel's own text: the
		// log must hold neither, so a client can neither fill the log with its
		// bytes nor get its own line dropped by the filter.
		raw := "GET / HTTP/1.1\r\nBad Header MARKER-xyz " + errPlainHTTP.Error() + "\r\n\r\n"
		resp := plainExchange(t, ts, raw)
		if resp != nil {
			t.Fatalf("got %v, want an empty close", resp)
		}
		line := nextLine(t, lines)
		if isExpectedHandshakeError([]byte(line)) {
			t.Errorf("a request that was not answered was dropped from the log: %q", line)
		}
		if strings.Contains(line, "MARKER-xyz") || strings.Contains(line, "Bad Header") {
			t.Errorf("client bytes reached the log: %q", line)
		}
		if !strings.Contains(line, "plain-text request") {
			t.Errorf("log line does not name the plain-text request: %q", line)
		}
	})

	t.Run("redirected connection logs only the dropped line", func(t *testing.T) {
		t.Parallel()
		ts, lines := newRedirectServer(t, nil)
		plainExchange(t, ts, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
		line := nextLine(t, lines)
		if !isExpectedHandshakeError([]byte(line)) {
			t.Fatalf("filter no longer matches net/http's wording for the redirect: %q", line)
		}
		var out bytes.Buffer
		if _, err := NewFilteredErrorLog(log.New(&out, "", 0)).Writer().Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatalf("redirect line was forwarded, want dropped: %q", out.String())
		}
	})
}

func TestRedirectPlainHTTPLeavesTLSAlone(t *testing.T) {
	t.Parallel()

	t.Run("TLS client still works", func(t *testing.T) {
		t.Parallel()
		ts, _ := newRedirectServer(t, nil)
		resp, err := ts.Client().Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Errorf("got %d %q, want 200 ok", resp.StatusCode, body)
		}
	})

	t.Run("silent client blocks neither accept nor the deadline", func(t *testing.T) {
		t.Parallel()
		ts, _ := newRedirectServer(t, nil)
		silent, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = silent.Close() }()
		resp, err := ts.Client().Get(ts.URL)
		if err != nil {
			t.Fatalf("TLS request while a silent connection is open: %v", err)
		}
		_ = resp.Body.Close()
		if err := silent.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := silent.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("silent connection read: %v, want EOF from the server's handshake deadline", err)
		}
	})
}
