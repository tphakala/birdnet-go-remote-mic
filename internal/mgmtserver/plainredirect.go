package mgmtserver

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// errPlainHTTP ends a TLS handshake whose plain HTTP request was answered. net/http
// logs it as the handshake error, and the error-log filter drops that line by
// this text, so there is one source of truth for the wording.
var errPlainHTTP = errors.New("plain HTTP request on the HTTPS port")

const (
	// maxPlainRequestBytes caps what is read to parse a plain request. The limit
	// is on the reader, not the bufio buffer: textproto accumulates a long line
	// across buffer refills, so the buffer size alone bounds nothing.
	maxPlainRequestBytes = 64 << 10
	// maxPlainDrainBytes caps the input discarded after the answer, so a client
	// that is still uploading does not turn the close into a reset that eats it.
	maxPlainDrainBytes = 256 << 10
)

// RedirectPlainHTTP wraps ln, the raw listener under tls.NewListener, so that a
// client that speaks plain HTTP to the TLS port gets a redirect to https on the
// same port instead of a handshake error. certificate supplies the current
// certificate; its leaf decides which Host header may be echoed back.
//
// The decision is made on the first Read of a connection, inside net/http's
// per-connection TLS handshake, never in Accept, so a client that connects and
// sends nothing cannot hold up accepting. The plain path relies on the deadline
// net/http sets on the connection before the handshake, which exists when the
// http.Server has a nonzero ReadHeaderTimeout, ReadTimeout or WriteTimeout;
// without one a silent client is not bounded, with or without this wrapper.
//
// Only a first byte in 'A'..'Z' (an HTTP method) takes the plain path. Every
// other connection, TLS or garbage, is passed through byte for byte.
//
// crypto/tls replaces the handshake error with the context's error if the
// handshake context is cancelled mid-read. net/http cancels it only when the
// connection ends, and the management server sets no BaseContext, so the
// sentinel reaches the log intact; the end-to-end log test would catch a change.
func RedirectPlainHTTP(ln net.Listener, certificate func() (*tls.Certificate, error)) net.Listener {
	return &redirectListener{Listener: ln, certificate: certificate}
}

type redirectListener struct {
	net.Listener
	certificate func() (*tls.Certificate, error)
}

func (l *redirectListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sniffConn{Conn: c, certificate: l.certificate}, nil
}

// sniffConn inspects the first Read. It embeds net.Conn without overriding the
// deadline methods, so net/http's handshake deadline reaches the raw socket.
type sniffConn struct {
	net.Conn
	certificate func() (*tls.Certificate, error)
	// decided is only touched from Read, which crypto/tls serialises under its
	// input lock, so it needs no atomic.
	decided bool
}

func (c *sniffConn) Read(p []byte) (int, error) {
	if c.decided {
		return c.Conn.Read(p)
	}
	n, err := c.Conn.Read(p)
	if n == 0 {
		return n, err
	}
	c.decided = true
	if p[0] < 'A' || p[0] > 'Z' {
		return n, err
	}
	// p belongs to crypto/tls, so copy what the parser will read from it.
	return 0, c.answerPlain(bytes.Clone(p[:n]))
}

// answerPlain parses the request that began with first and writes the redirect
// (or a 400 when there is no host to name). It returns errPlainHTTP only when a
// response was written. A request that does not parse, or that times out or
// ends mid-way, gets no response and a wrapped error that still reaches the log,
// as non-HTTP traffic on this port does today. Write errors are ignored: the
// client is gone, and the connection deadline bounds every step here.
//
// A request that declared a body, or that left bytes buffered (pipelining), is
// answered, half-closed and drained before the close: closing a socket with
// unread input sends an RST that can discard the response, and even a Linux
// client then reads ECONNRESET. This mirrors net/http's closeWriteAndWait. A
// bodiless request skips it and the connection closes at once.
func (c *sniffConn) answerPlain(first []byte) error {
	r := io.MultiReader(bytes.NewReader(first), &io.LimitedReader{R: c.Conn, N: maxPlainRequestBytes})
	br := bufio.NewReader(r)
	req, err := http.ReadRequest(br)
	if err != nil {
		return fmt.Errorf("plain-text request: %w", err)
	}
	var leaf *x509.Certificate
	if cert, cerr := c.certificate(); cerr == nil && cert != nil {
		leaf = cert.Leaf
	}
	location, status := redirectTarget(req, leaf, c.LocalAddr())
	_, _ = c.Write(plainResponse(location, status))
	if req.ContentLength != 0 || len(req.TransferEncoding) > 0 || br.Buffered() > 0 {
		if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(c.Conn, maxPlainDrainBytes))
	}
	return errPlainHTTP
}

func plainResponse(location string, status int) []byte {
	var b []byte
	switch status {
	case http.StatusPermanentRedirect, http.StatusTemporaryRedirect:
		b = append(b, "HTTP/1.1 "...)
		b = strconv.AppendInt(b, int64(status), 10)
		b = append(b, ' ')
		b = append(b, http.StatusText(status)...)
		b = append(b, "\r\nLocation: "...)
		b = append(b, location...)
		b = append(b, "\r\nCache-Control: no-store\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"...)
	default:
		const body = "Client sent an HTTP request to an HTTPS server.\n"
		b = append(b, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: "...)
		b = strconv.AppendInt(b, int64(len(body)), 10)
		b = append(b, "\r\n\r\n"...)
		b = append(b, body...)
	}
	return b
}

// redirectTarget returns the https Location for req and the status to send it
// with. The host is the request's Host only when leaf verifies it, written in
// canonical form, and then the redirect is permanent (308). Otherwise it is the
// local address the connection arrived on, and the redirect is temporary (307):
// that is the only case that changes the host, and a stored permanent redirect
// would follow the address to whatever device holds it after a DHCP change. A
// status of 0 means no host can be named (an unverified Host and no usable
// local IP), and the caller answers 400. The port is always the local port,
// taken separately from the IP so a verified Host works over a link-local
// connection too.
func redirectTarget(req *http.Request, leaf *x509.Certificate, local net.Addr) (location string, status int) {
	ta, ok := local.(*net.TCPAddr)
	if !ok {
		return "", 0
	}
	host, ok := canonicalHost(req.Host)
	if ok && leaf != nil && leaf.VerifyHostname(host) == nil {
		status = http.StatusPermanentRedirect
	} else {
		// The zone check comes before Unmap, which erases it.
		addr := ta.AddrPort().Addr()
		if !addr.IsValid() || addr.IsUnspecified() || addr.Zone() != "" {
			return "", 0
		}
		host, status = addr.Unmap().String(), http.StatusTemporaryRedirect
	}
	path := "/"
	if uri := req.RequestURI; isOriginForm(uri) {
		path = uri
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(ta.Port)) + path, status
}

// isOriginForm reports whether uri starts with '/' and holds only printable
// ASCII, so it is safe to append to an absolute Location of our own making.
func isOriginForm(uri string) bool {
	if uri == "" || uri[0] != '/' {
		return false
	}
	for i := range len(uri) {
		if uri[i] < 0x21 || uri[i] > 0x7e {
			return false
		}
	}
	return true
}

// canonicalHost reduces a Host header value to a bare host in canonical form,
// or reports false. It accepts "[ipv6]", "[ipv6]:port", "ipv4", "name" and
// "ipv4:port" or "name:port" (one colon only), where port is 1 to 5 digits and
// is otherwise ignored. IPv6 must be bracketed and unzoned. A name is lowercase
// ASCII with at most one trailing dot removed, 1 to 253 bytes, in labels of 1 to
// 63 bytes from [a-z0-9-] that start and end alphanumeric. The result is never
// the raw header: IPs are re-rendered from the parsed address.
func canonicalHost(hostport string) (string, bool) {
	host := hostport
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", false
		}
		rest := host[end+1:]
		if port, hasPort := strings.CutPrefix(rest, ":"); rest != "" && (!hasPort || !validPort(port)) {
			return "", false
		}
		addr, err := netip.ParseAddr(host[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		return addr.String(), true
	}
	if h, port, found := strings.Cut(host, ":"); found {
		if !validPort(port) {
			return "", false
		}
		host = h
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !addr.Is4() {
			return "", false
		}
		return addr.String(), true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || len(host) > 253 {
		return "", false
	}
	for label := range strings.SplitSeq(host, ".") {
		if !validLabel(label) {
			return "", false
		}
	}
	return host, true
}

func validPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func validLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := range len(l) {
		ch := l[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}
