package mgmtserver

import (
	"bytes"
	"log"
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
// every other write through dst, the process logger. log.Logger emits each
// entry as a single contiguous Write and serializes writes under its own mutex,
// so this filter sees one whole line at a time and needs no locking of its own.
//
// Forwarding through a *log.Logger rather than a captured io.Writer means the
// kept lines pick up dst's destination, flags, and lock dynamically at write
// time, so a genuine error is formatted exactly as if http.Server.ErrorLog were
// nil (net/http then logs through the standard logger). If the operator ever
// reconfigures the process logger with log.SetOutput or log.SetFlags, the
// passthrough follows it instead of writing to a stale fd with frozen flags.
type handshakeErrorFilter struct {
	dst *log.Logger
}

func (f *handshakeErrorFilter) Write(p []byte) (int, error) {
	if isClientTLSHandshakeRejection(p) {
		// Report the bytes as consumed so the caller does not treat the drop as
		// a short write.
		return len(p), nil
	}
	// The wrapping logger has no flags of its own, so p is net/http's raw
	// message plus the single trailing newline that logger added. Strip that
	// newline and hand the message to dst.Output, which applies dst's prefix,
	// flags, and timestamp and terminates the line with its own single newline.
	// The calldepth of 2 is inert here: dst.Output consults it only under
	// Lshortfile/Llongfile, which log.Default() does not set, and threading
	// net/http's own caller location back through this extra logger layer would
	// be fragile, so the passthrough carries dst's timestamp but no file:line.
	if err := f.dst.Output(2, string(bytes.TrimRight(p, "\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// NewFilteredErrorLog returns a *log.Logger suitable for http.Server.ErrorLog
// that suppresses client TLS-handshake-rejection spam while passing every other
// server error through to dst, the process logger. A nil dst defaults to
// log.Default(). The returned logger carries no flags of its own: kept lines are
// formatted by dst, so a genuine error looks identical to the output net/http
// produces with a nil ErrorLog.
func NewFilteredErrorLog(dst *log.Logger) *log.Logger {
	if dst == nil {
		dst = log.Default()
	}
	return log.New(&handshakeErrorFilter{dst: dst}, "", 0)
}
