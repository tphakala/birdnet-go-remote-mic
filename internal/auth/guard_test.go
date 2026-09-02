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
		{"two separator spaces (RFC 6750 1*SP)", testToken, "Bearer  " + testToken, true},
		{"many separator spaces", testToken, "Bearer    " + testToken, true},
		{"tab separator is not SP", testToken, "Bearer\t" + testToken, false},
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

func TestGeneration(t *testing.T) {
	// Generation advances only when Set installs a token that differs from the
	// current one. Setting the same token again must NOT advance it: PatchConfig
	// and the reconcile both call Set with the same token on every config change,
	// and a spurious advance would evict every live stream.
	g := NewGuard("")
	steps := []struct {
		set  string
		want uint64
	}{
		{"", 0},                   // open access from the start: no advance
		{testToken, 1},            // enabling a token advances once
		{testToken, 1},            // same token again: idempotent, no advance
		{"rotated-token-0001", 2}, // rotating to a different token advances
		{"", 3},                   // clearing the token advances (open access)
	}
	for i, s := range steps {
		g.Set(s.set)
		if got := g.Generation(); got != s.want {
			t.Errorf("step %d Set(%q): Generation = %d, want %d", i, s.set, got, s.want)
		}
	}
}

func TestNilGuardGenerationIsZero(t *testing.T) {
	var g *Guard
	if got := g.Generation(); got != 0 {
		t.Errorf("nil guard Generation = %d, want 0", got)
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
