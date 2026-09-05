package mgmtserver

import (
	"bytes"
	"io"
	"log"
	"os"
)

// These markers identify a client-aborted TLS handshake in an http.Server
// error-log line. net/http emits "http: TLS handshake error from <addr>:
// <err>", and when the client aborts the handshake it sends a TLS alert that
// crypto/tls formats as "remote error: tls: <reason>". That covers a rejected
// certificate (unknown certificate, bad certificate) and equally a failed
// negotiation (protocol version, handshake failure, insufficient security):
// every one of them is the client declining to trust or talk to the appliance,
// not a server fault. Requiring BOTH markers scopes the filter to that
// client-rejection class: a server-side TLS misconfiguration (for example
// "tls: no certificates configured") lacks the "remote error:" prefix, and
// every other http.Server error lacks the handshake marker, so both still log.
var (
	tlsHandshakeErrorMarker = []byte("http: TLS handshake error")
	clientTLSAlertMarker    = []byte("remote error: tls:")
)

// isClientTLSHandshakeRejection reports whether line is an http.Server log entry
// for a handshake the client aborted, whether by rejecting the appliance's
// self-signed certificate or by failing to negotiate (a version or cipher
// mismatch). Such lines are expected traffic from an untrusting or incompatible
// client, not a server fault, and logging one per reconnect buries real events.
func isClientTLSHandshakeRejection(line []byte) bool {
	return bytes.Contains(line, tlsHandshakeErrorMarker) &&
		bytes.Contains(line, clientTLSAlertMarker)
}

// handshakeErrorFilter drops client TLS-handshake-rejection lines and forwards
// every other write to w unchanged. log.Logger emits each entry as a single
// contiguous Write and serializes writes under its own mutex, so this filter
// sees one whole line at a time and needs no locking of its own.
type handshakeErrorFilter struct {
	w io.Writer
}

func (f *handshakeErrorFilter) Write(p []byte) (int, error) {
	if isClientTLSHandshakeRejection(p) {
		// Report the bytes as consumed so the caller does not treat the drop as
		// a short write.
		return len(p), nil
	}
	return f.w.Write(p)
}

// NewFilteredErrorLog returns a *log.Logger suitable for http.Server.ErrorLog
// that suppresses client TLS-handshake-rejection spam while passing every other
// server error through to w. It uses log.LstdFlags so a genuine error looks
// identical to the default net/http logger's output.
func NewFilteredErrorLog(w io.Writer) *log.Logger {
	if w == nil {
		w = os.Stderr
	}
	return log.New(&handshakeErrorFilter{w: w}, "", log.LstdFlags)
}
