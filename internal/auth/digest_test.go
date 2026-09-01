package auth

import (
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

const (
	testMethod = "DESCRIBE"
	testURI    = "rtsp://mic.local:8554/garden"
	testUser   = "mic"
)

// clientAnswer builds the Authorization value go-audio-stream's RTSP client (the
// BirdNET-Go ingest path) would send in reply to challenge, so the server-side
// check is verified against the real consumer rather than a hand-rolled digest.
func clientAnswer(t *testing.T, challenge, user, password, method, uri string) string {
	t.Helper()
	chs := rtsp.ParseChallenges([]string{challenge})
	if len(chs) != 1 {
		t.Fatalf("ParseChallenges(%q) yielded %d challenges, want 1", challenge, len(chs))
	}
	value, err := rtsp.Authorize(chs[0], rtsp.Credentials{Username: user, Password: password}, rtsp.DigestInput{
		Method:     method,
		URI:        uri,
		CNonce:     "0a4f113b",
		NonceCount: 1,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return value
}

func TestCheckDigestAcceptsQopAuthAnswer(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	hdr := clientAnswer(t, g.DigestChallenge(nonce), testUser, testToken, testMethod, testURI)
	if !strings.Contains(hdr, `qop=auth`) {
		t.Fatalf("client answer should use qop=auth, got %q", hdr)
	}
	if !g.CheckDigest(testMethod, hdr, nonce) {
		t.Errorf("valid qop=auth answer rejected: %q", hdr)
	}
}

func TestCheckDigestAcceptsLegacyAnswer(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	// A client that ignores qop (live555/VLC) answers with the RFC 2069 form.
	legacy := strings.Replace(g.DigestChallenge(nonce), `, qop="auth"`, "", 1)
	hdr := clientAnswer(t, legacy, testUser, testToken, testMethod, testURI)
	if strings.Contains(hdr, "qop=") {
		t.Fatalf("legacy answer should carry no qop, got %q", hdr)
	}
	if !g.CheckDigest(testMethod, hdr, nonce) {
		t.Errorf("valid legacy answer rejected: %q", hdr)
	}
}

func TestCheckDigestAnyUsername(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	hdr := clientAnswer(t, g.DigestChallenge(nonce), "someone-else", testToken, testMethod, testURI)
	if !g.CheckDigest(testMethod, hdr, nonce) {
		t.Error("the username is free-form; only the password (token) is checked")
	}
}

func TestCheckDigestRejections(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	challenge := g.DigestChallenge(nonce)
	good := clientAnswer(t, challenge, testUser, testToken, testMethod, testURI)

	tests := []struct {
		name   string
		method string
		header string
		nonce  string
	}{
		{"wrong password", testMethod, clientAnswer(t, challenge, testUser, "wrong-password-1", testMethod, testURI), nonce},
		{"wrong nonce", testMethod, good, g.NewNonce()},
		{"realm mismatch", testMethod, clientAnswer(t, strings.Replace(challenge, Realm, "other-realm", 1), testUser, testToken, testMethod, testURI), nonce},
		{"method mismatch", "PLAY", good, nonce},
		{"basic scheme", testMethod, "Basic bWljOms3UW0zdlg5cEwyd1I4blQ=", nonce},
		{"empty header", testMethod, "", nonce},
		{"missing response", testMethod, `Digest username="mic", realm="birdnet-go-remote-mic", nonce="` + nonce + `", uri="` + testURI + `"`, nonce},
		{"unsupported qop", testMethod, strings.Replace(good, "qop=auth", "qop=auth-int", 1), nonce},
		{"empty nonce on server", testMethod, good, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if g.CheckDigest(tt.method, tt.header, tt.nonce) {
				t.Errorf("CheckDigest accepted %q", tt.header)
			}
		})
	}
}

func TestCheckDigestDisabledGuard(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	good := clientAnswer(t, g.DigestChallenge(nonce), testUser, testToken, testMethod, testURI)
	g.Set("")
	if g.CheckDigest(testMethod, good, nonce) {
		t.Error("a disabled guard must not authenticate")
	}
	var nilGuard *Guard
	if nilGuard.CheckDigest(testMethod, good, nonce) {
		t.Error("a nil guard must not authenticate")
	}
}

func TestCheckDigestAfterRotation(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	old := clientAnswer(t, g.DigestChallenge(nonce), testUser, testToken, testMethod, testURI)
	g.Set("rotated-token-9876")
	if g.CheckDigest(testMethod, old, nonce) {
		t.Error("an answer computed with the old token must fail after rotation")
	}
	fresh := clientAnswer(t, g.DigestChallenge(nonce), testUser, "rotated-token-9876", testMethod, testURI)
	if !g.CheckDigest(testMethod, fresh, nonce) {
		t.Error("an answer computed with the new token must pass")
	}
}

func TestParseDigestParams(t *testing.T) {
	got := parseDigestParams(`username="mic", REALM="a \"quoted\" realm",nonce=abc123 , uri="rtsp://h/p", algorithm=MD5, nc=00000001`)
	want := map[string]string{
		"username":  "mic",
		"realm":     `a "quoted" realm`,
		"nonce":     "abc123",
		"uri":       "rtsp://h/p",
		"algorithm": "MD5",
		"nc":        "00000001",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d params, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("param %q = %q, want %q", k, got[k], v)
		}
	}
}
