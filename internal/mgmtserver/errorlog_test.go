package mgmtserver

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsClientTLSHandshakeRejection(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "client rejects self-signed cert",
			line: "2026/09/05 15:04:55 http: TLS handshake error from 192.168.4.3:63825: remote error: tls: unknown certificate",
			want: true,
		},
		{
			name: "client sends bad certificate alert",
			line: "2026/09/05 15:04:55 http: TLS handshake error from 10.0.0.9:5001: remote error: tls: bad certificate",
			want: true,
		},
		{
			name: "local misconfig: no certificates configured",
			line: "2026/09/05 15:04:55 http: TLS handshake error from 10.0.0.9:5001: tls: no certificates configured",
			want: false,
		},
		{
			name: "abrupt disconnect is not a client rejection",
			line: "2026/09/05 15:04:55 http: TLS handshake error from 10.0.0.9:5001: EOF",
			want: false,
		},
		{
			name: "unrelated http.Server error",
			line: "2026/09/05 15:04:55 http: Accept error: accept tcp: too many open files",
			want: false,
		},
		{
			// The client-alert marker alone must not match: this pins the
			// handshake-error marker independently, so dropping that half of the
			// predicate turns this red.
			name: "client-alert text outside a handshake-error line",
			line: "2026/09/05 15:04:55 http: Accept error: remote error: tls: bad certificate",
			want: false,
		},
		{
			name: "arbitrary line mentioning tls but not a handshake error",
			line: "2026/09/05 15:04:55 management API on https://:8443 (tls: ok)",
			want: false,
		},
		{
			name: "empty line",
			line: "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientTLSHandshakeRejection([]byte(tc.line)); got != tc.want {
				t.Errorf("isClientTLSHandshakeRejection(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestFilteredErrorLogDropsClientRejections(t *testing.T) {
	var buf bytes.Buffer
	logger := NewFilteredErrorLog(log.New(&buf, "", log.LstdFlags))

	// A client-rejection handshake line, exactly as net/http's logf emits it.
	logger.Printf("http: TLS handshake error from %s: %s", "192.168.4.3:63825", "remote error: tls: unknown certificate")
	if buf.Len() != 0 {
		t.Fatalf("client TLS rejection was logged, want dropped: %q", buf.String())
	}
}

func TestFilteredErrorLogPassesGenuineErrors(t *testing.T) {
	var buf bytes.Buffer
	// The destination logger carries the flags; the filtered logger has none of
	// its own, so a passed-through error is formatted entirely by dst.
	logger := NewFilteredErrorLog(log.New(&buf, "", log.LstdFlags))

	logger.Printf("http: Accept error: %v", "too many open files")
	got := buf.String()
	if !strings.Contains(got, "http: Accept error: too many open files") {
		t.Fatalf("genuine server error not passed through, got %q", got)
	}
	// The destination's LstdFlags date/time prefix must be present so a real
	// error looks identical to the default net/http logger, and it must appear
	// EXACTLY ONCE: the filtered logger carries no flags of its own, so a
	// regression that let the wrapping logger re-add LstdFlags would print two
	// timestamps. A leading-anchored match alone would not catch that (it still
	// matches the first prefix), so count occurrences and pin the one to the
	// start of the line.
	tsRe := regexp.MustCompile(`\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	if locs := tsRe.FindAllStringIndex(got, -1); len(locs) != 1 || locs[0][0] != 0 {
		t.Errorf("expected exactly one LstdFlags timestamp prefix at the start of the passthrough, got %d occurrence(s): %q", len(locs), got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("passthrough should be one line with one trailing newline, got %d newlines: %q", n, got)
	}
}

func TestNewFilteredErrorLogNilDestinationDefaultsToStandardLogger(t *testing.T) {
	logger := NewFilteredErrorLog(nil)
	f, ok := logger.Writer().(*handshakeErrorFilter)
	if !ok {
		t.Fatalf("logger writer is %T, want *handshakeErrorFilter", logger.Writer())
	}
	if f.dst != log.Default() {
		t.Errorf("nil destination was not defaulted to log.Default(), got %v", f.dst)
	}
}

func TestFilteredErrorLogPassesLocalTLSMisconfig(t *testing.T) {
	var buf bytes.Buffer
	logger := NewFilteredErrorLog(log.New(&buf, "", log.LstdFlags))

	// A server-side TLS setup failure (no "remote error: tls:") must still surface.
	logger.Printf("http: TLS handshake error from %s: %s", "10.0.0.9:5001", "tls: no certificates configured")
	if !strings.Contains(buf.String(), "tls: no certificates configured") {
		t.Fatalf("local TLS misconfig was dropped, want passed through: %q", buf.String())
	}
}

// lockedBuffer is a bytes.Buffer safe for the concurrent Write (from the server's
// handshake goroutine) and String (from the test goroutine) the differential
// test needs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestFilterDropsRealNetHTTPHandshakeError validates the hand-built markers
// against the actual producer. Every other test feeds lines matching an assumed
// net/http wording; this one provokes a real client-aborted TLS handshake and
// asserts both that isClientTLSHandshakeRejection still matches net/http's real
// output and that the filter drops it end to end. If net/http or crypto/tls ever
// reword that log line, this turns red instead of the filter silently going
// inert.
func TestFilterDropsRealNetHTTPHandshakeError(t *testing.T) {
	lb := &lockedBuffer{}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// Flags 0: capture net/http's raw message with no timestamp of our own so
	// the marker assertion runs against exactly what net/http produced.
	ts.Config.ErrorLog = log.New(lb, "", 0)
	ts.StartTLS()
	defer ts.Close()

	// A default client does not trust the server's self-signed certificate, so
	// it aborts the handshake with a TLS alert. That alert is what makes the
	// server log "remote error: tls: ...".
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("closing unexpected response body: %v", cerr)
		}
		t.Fatal("expected the client to reject the self-signed certificate")
	}

	var line string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line = lb.String(); line != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line == "" {
		t.Fatal("net/http logged no error for the aborted handshake; the test could not observe the real line")
	}
	// Exactly one logged line: the aborted handshake is the only traffic this
	// server sees, so a second line would mean an unrelated error slipped in and
	// the marker/drop assertions below would be judging a multi-line batch.
	if n := strings.Count(line, "\n"); n != 1 {
		t.Fatalf("expected exactly one captured log line, got %d newlines: %q", n, line)
	}
	if !isClientTLSHandshakeRejection([]byte(line)) {
		t.Fatalf("filter markers no longer match net/http's handshake-error wording: %q", line)
	}

	// The filter's own Write path drops that real line rather than forwarding it.
	var out bytes.Buffer
	fl := NewFilteredErrorLog(log.New(&out, "", 0))
	if _, err := fl.Writer().Write([]byte(line)); err != nil {
		t.Fatalf("filter Write returned error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("real handshake-rejection line was forwarded, want dropped: %q", out.String())
	}
}
