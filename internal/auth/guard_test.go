package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

const testToken = "k7Qm3vX9pL2wR8nT"

func TestCheckBearer(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		header string
		want   bool
	}{
		{"disabled guard rejects everything", "", "Bearer " + testToken, false},
		{"exact match", testToken, "Bearer " + testToken, true},
		{"scheme is case-insensitive", testToken, "bearer " + testToken, true},
		{"wrong token", testToken, "Bearer nope-nope-nope", false},
		{"missing space", testToken, "Bearer" + testToken, false},
		{"trailing junk", testToken, "Bearer " + testToken + " extra", false},
		{"leading whitespace in token", testToken, "Bearer  " + testToken, false},
		{"empty header", testToken, "", false},
		{"basic scheme", testToken, "Basic " + testToken, false},
		{"token prefix only", testToken, "Bearer " + testToken[:8], false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGuard(tt.token)
			if got := g.CheckBearer(tt.header); got != tt.want {
				t.Errorf("CheckBearer(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestSetSwapsToken(t *testing.T) {
	g := NewGuard(testToken)
	if !g.Enabled() {
		t.Fatal("guard with a token must be enabled")
	}
	g.Set("another-token-1234")
	if g.CheckBearer("Bearer " + testToken) {
		t.Error("old token must be rejected after Set")
	}
	if !g.CheckBearer("Bearer another-token-1234") {
		t.Error("new token must be accepted after Set")
	}
	g.Set("")
	if g.Enabled() {
		t.Error("empty token must disable the guard")
	}
	if g.CheckBearer("Bearer another-token-1234") {
		t.Error("a disabled guard must not authenticate")
	}
}

func TestNilGuardIsDisabled(t *testing.T) {
	var g *Guard
	if g.Enabled() {
		t.Error("nil guard must report disabled")
	}
	if g.CheckBearer("Bearer " + testToken) {
		t.Error("nil guard must not authenticate")
	}
}

func TestNewNonce(t *testing.T) {
	g := NewGuard(testToken)
	a, b := g.NewNonce(), g.NewNonce()
	if len(a) != 32 {
		t.Fatalf("nonce %q has length %d, want 32 hex chars", a, len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("nonce %q is not hex: %v", a, err)
	}
	if a == b {
		t.Error("two nonces must differ")
	}
}

func TestDigestChallengeFormat(t *testing.T) {
	g := NewGuard(testToken)
	got := g.DigestChallenge("0123456789abcdef0123456789abcdef")
	want := `Digest realm="birdnet-go-remote-mic", nonce="0123456789abcdef0123456789abcdef", algorithm=MD5, qop="auth"`
	if got != want {
		t.Errorf("DigestChallenge = %q, want %q", got, want)
	}
}

func TestValidToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		ok    bool
	}{
		{"empty is open access", "", true},
		{"eleven chars too short", "abcdefghijk", false},
		{"twelve chars ok", "abcdefghijkl", true},
		{"128 chars ok", strings.Repeat("a", 128), true},
		{"129 chars too long", strings.Repeat("a", 129), false},
		{"space rejected", "abcdef ghijkl", false},
		{"at sign rejected", "abcdef@ghijkl", false},
		{"slash rejected", "abcdef/ghijkl", false},
		{"colon rejected", "abcdef:ghijkl", false},
		{"quote rejected", `abcdef"ghijkl`, false},
		{"unicode rejected", "abcdefghijklä", false},
		{"unreserved punctuation ok", "ab.cd_ef~gh-ij", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := ValidToken(tt.token)
			if (reason == "") != tt.ok {
				t.Errorf("ValidToken(%q) = %q, want ok=%v", tt.token, reason, tt.ok)
			}
		})
	}
}
