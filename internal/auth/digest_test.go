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

	// Hand-built headers for the branches rtsp.Authorize will not emit: a non-MD5
	// algorithm, and a qop=auth answer missing nc or cnonce. Building them by hand
	// keeps the CheckDigest field checks (not the client) under test.
	const nc, cnonce = "00000001", "0a4f113b"
	legacyResp := digestResponse(testUser, testToken, testMethod, testURI, nonce, "", "", "")
	// Compute each qop response over the header EXACTLY as sent (the missing field
	// empty), so the only thing left to reject the answer is the explicit nc/cnonce
	// presence check, not a response mismatch.
	respNoCnonce := digestResponse(testUser, testToken, testMethod, testURI, nonce, nc, "", "auth")
	respNoNc := digestResponse(testUser, testToken, testMethod, testURI, nonce, "", cnonce, "auth")
	base := `Digest username="mic", realm="birdnet-go-remote-mic", nonce="` + nonce + `", uri="` + testURI + `"`

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
		{"missing response", testMethod, base, nonce},
		{"unsupported qop", testMethod, strings.Replace(good, "qop=auth", "qop=auth-int", 1), nonce},
		{"empty nonce on server", testMethod, good, ""},
		// A non-MD5 algorithm is rejected even with an otherwise valid MD5 response:
		// the appliance advertises MD5 only and must not silently accept a mislabeled
		// answer.
		{"non-md5 algorithm", testMethod, base + `, algorithm=SHA-256, response="` + legacyResp + `"`, nonce},
		// qop=auth requires both nc and cnonce as digest inputs; a missing one
		// leaves the hash inputs incomplete, so it is rejected rather than folded
		// in as an empty string. This is an input-completeness check, not replay
		// protection: the server never stores or validates nc as a replay counter
		// (one nonce is issued per connection, and the server does not track nc
		// across requests). The response matches the header as sent, so only the
		// presence check rejects.
		{"qop auth missing cnonce", testMethod, base + `, qop=auth, nc=` + nc + `, response="` + respNoCnonce + `"`, nonce},
		{"qop auth missing nc", testMethod, base + `, qop=auth, cnonce="` + cnonce + `", response="` + respNoNc + `"`, nonce},
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

// digestResponse recomputes the RFC 7616/2069 response the client would put in
// its Authorization header, so the acceptance cases below can be assembled by
// hand (varying whitespace, param order, and qop casing) while still carrying a
// correct response. Passing "" for qop selects the RFC 2069 legacy form. It
// calls the package's own md5hex (this is an internal test) rather than
// re-deriving the MD5 primitive, so the helper cannot silently diverge from the
// hash CheckDigest uses. The join order still mirrors CheckDigest, but that
// end-to-end shape is independently checked by the rtsp.Authorize-based
// acceptance tests above (a separate implementation from go-audio-stream).
func digestResponse(user, token, method, uri, nonce, nc, cnonce, qop string) string {
	ha1 := md5hex(user + ":" + Realm + ":" + token)
	ha2 := md5hex(method + ":" + uri)
	if qop == "" {
		return md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return md5hex(strings.Join([]string{ha1, nonce, nc, cnonce, qop, ha2}, ":"))
}

// TestCheckDigestAcceptanceDomain proves the parser accepts RFC-legal header
// shapes a naive hand-rolled parser tends to get wrong: whitespace around "="
// and a qop token in a different case than the literal "auth". Arbitrary
// parameter order is exercised too, but unlike the others it is not a shape a
// reasonable parser would reject (the grammar is unordered), so it stands as a
// plain regression guard rather than a case the old code failed. Each header is
// built by hand (not via rtsp.Authorize) so the server-side parser is what is
// under test.
func TestCheckDigestAcceptanceDomain(t *testing.T) {
	g := NewGuard(testToken)
	nonce := g.NewNonce()
	const nc, cnonce = "00000001", "0a4f113b"
	legacyResp := digestResponse(testUser, testToken, testMethod, testURI, nonce, "", "", "")
	upperQopResp := digestResponse(testUser, testToken, testMethod, testURI, nonce, nc, cnonce, "AUTH")

	tests := []struct {
		name   string
		header string
	}{
		{
			"space after algorithm equals",
			`Digest username="mic", realm="birdnet-go-remote-mic", nonce="` + nonce + `", uri="` + testURI + `", algorithm= MD5, response="` + legacyResp + `"`,
		},
		{
			"spaces around nonce equals",
			`Digest username="mic", realm="birdnet-go-remote-mic", nonce = "` + nonce + `", uri="` + testURI + `", response="` + legacyResp + `"`,
		},
		{
			"params in reversed order",
			`Digest response="` + legacyResp + `", uri="` + testURI + `", nonce="` + nonce + `", realm="birdnet-go-remote-mic", username="mic"`,
		},
		{
			"qop token uppercased",
			`Digest username="mic", realm="birdnet-go-remote-mic", nonce="` + nonce + `", uri="` + testURI + `", qop=AUTH, nc=` + nc + `, cnonce="` + cnonce + `", response="` + upperQopResp + `"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !g.CheckDigest(testMethod, tt.header, nonce) {
				t.Errorf("CheckDigest rejected RFC-legal header: %q", tt.header)
			}
		})
	}
}
