package mgmtserver

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
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
	logger := NewFilteredErrorLog(&buf)

	// A client-rejection handshake line, exactly as net/http's logf emits it.
	logger.Printf("http: TLS handshake error from %s: %s", "192.168.4.3:63825", "remote error: tls: unknown certificate")
	if buf.Len() != 0 {
		t.Fatalf("client TLS rejection was logged, want dropped: %q", buf.String())
	}
}

func TestFilteredErrorLogPassesGenuineErrors(t *testing.T) {
	var buf bytes.Buffer
	logger := NewFilteredErrorLog(&buf)

	logger.Printf("http: Accept error: %v", "too many open files")
	got := buf.String()
	if !strings.Contains(got, "http: Accept error: too many open files") {
		t.Fatalf("genuine server error not passed through, got %q", got)
	}
	// The LstdFlags date/time prefix must be present so a real error looks
	// identical to the default net/http logger.
	if !regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `).MatchString(got) {
		t.Errorf("expected an LstdFlags timestamp prefix on passthrough, got %q", got)
	}
}

func TestFilteredErrorLogPassesLocalTLSMisconfig(t *testing.T) {
	var buf bytes.Buffer
	logger := NewFilteredErrorLog(&buf)

	// A server-side TLS setup failure (no "remote error: tls:") must still surface.
	logger.Printf("http: TLS handshake error from %s: %s", "10.0.0.9:5001", "tls: no certificates configured")
	if !strings.Contains(buf.String(), "tls: no certificates configured") {
		t.Fatalf("local TLS misconfig was dropped, want passed through: %q", buf.String())
	}
}
