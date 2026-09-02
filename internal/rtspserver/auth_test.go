package rtspserver

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	rtsp "github.com/tphakala/go-audio-stream/rtsp"
)

const (
	testAuthToken = "k7Qm3vX9pL2wR8nT"
	testUser      = "mic"
	methodDesc    = "DESCRIBE"
)

func authConfig(token string) Config {
	return Config{Timeout: 60 * time.Second, Auth: auth.NewGuard(token)}
}

// challengeOf extracts the Digest challenge from a 401 response.
func challengeOf(t *testing.T, resp *rtsp.Response) rtsp.Challenge {
	t.Helper()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	raw := resp.Header.Get("WWW-Authenticate")
	chs := rtsp.ParseChallenges([]string{raw})
	if len(chs) != 1 || chs[0].Scheme != rtsp.AuthDigest {
		t.Fatalf("WWW-Authenticate %q did not parse to one Digest challenge: %+v", raw, chs)
	}
	return chs[0]
}

// answer builds the Authorization header go-audio-stream's client would send
// for challenge, so the server is tested against the real BirdNET-Go path.
func answer(t *testing.T, ch rtsp.Challenge, password, method, uri string) rtsp.Header {
	t.Helper()
	value, err := rtsp.Authorize(ch, rtsp.Credentials{Username: testUser, Password: password}, rtsp.DigestInput{
		Method: method, URI: uri, CNonce: "0a4f113b", NonceCount: 1,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	h := rtsp.Header{}
	h.Set("Authorization", value)
	return h
}

func TestAuthOptionsIsOpen(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c := dial(t, addr)
	if resp := c.do(t, "OPTIONS", baseURL(addr), nil); resp.StatusCode != 200 {
		t.Errorf("OPTIONS without credentials: status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthDescribeChallengesThenAccepts(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c := dial(t, addr)
	ch := challengeOf(t, c.do(t, methodDesc, baseURL(addr), nil))
	if ch.Realm != auth.Realm {
		t.Errorf("realm = %q, want %q", ch.Realm, auth.Realm)
	}
	if ch.Params["qop"] != "auth" || ch.Params["nonce"] == "" {
		t.Errorf("challenge params = %v, want qop=auth and a nonce", ch.Params)
	}

	resp := c.do(t, methodDesc, baseURL(addr), answer(t, ch, testAuthToken, methodDesc, baseURL(addr)))
	if resp.StatusCode != 200 {
		t.Fatalf("authenticated DESCRIBE: status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "m=audio") {
		t.Errorf("DESCRIBE body is not the SDP: %q", resp.Body)
	}

	// The connection is now authenticated: SETUP and PLAY carry no credentials.
	setup := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if setup.StatusCode != 200 {
		t.Fatalf("SETUP on an authenticated connection: status = %d, want 200", setup.StatusCode)
	}
	h := rtsp.Header{}
	h.Set("Session", setup.Header.Get("Session"))
	if play := c.do(t, "PLAY", baseURL(addr), h); play.StatusCode != 200 {
		t.Errorf("PLAY on an authenticated connection: status = %d, want 200", play.StatusCode)
	}
}

func TestAuthLegacyAnswerAccepted(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c := dial(t, addr)
	ch := challengeOf(t, c.do(t, methodDesc, baseURL(addr), nil))
	// A client that ignores qop (live555/VLC) answers with the RFC 2069 form.
	delete(ch.Params, "qop")
	hdr := answer(t, ch, testAuthToken, methodDesc, baseURL(addr))
	if strings.Contains(hdr.Get("Authorization"), "qop=") {
		t.Fatalf("test setup: legacy answer still carries qop: %q", hdr.Get("Authorization"))
	}
	if resp := c.do(t, methodDesc, baseURL(addr), hdr); resp.StatusCode != 200 {
		t.Errorf("legacy Digest answer: status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthWrongPasswordKeepsChallenging(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c := dial(t, addr)
	ch := challengeOf(t, c.do(t, methodDesc, baseURL(addr), nil))
	bad := c.do(t, methodDesc, baseURL(addr), answer(t, ch, "wrong-password-1", methodDesc, baseURL(addr)))
	again := challengeOf(t, bad)
	if again.Params["nonce"] != ch.Params["nonce"] {
		t.Errorf("nonce changed across retries on one connection: %q vs %q", again.Params["nonce"], ch.Params["nonce"])
	}
	// The connection stays open: a correct answer still gets through.
	if resp := c.do(t, methodDesc, baseURL(addr), answer(t, again, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Errorf("correct answer after a rejected one: status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthNoncePerConnection(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c1 := dial(t, addr)
	c2 := dial(t, addr)
	n1 := challengeOf(t, c1.do(t, methodDesc, baseURL(addr), nil)).Params["nonce"]
	n2 := challengeOf(t, c2.do(t, methodDesc, baseURL(addr), nil)).Params["nonce"]
	if n1 == n2 {
		t.Error("two connections received the same nonce")
	}
	// Authenticating one connection does not authenticate the other.
	ch := challengeOf(t, c1.do(t, methodDesc, baseURL(addr), nil))
	if resp := c1.do(t, methodDesc, baseURL(addr), answer(t, ch, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Fatalf("c1 auth: status = %d", resp.StatusCode)
	}
	if resp := c2.do(t, "SETUP", trackURL(addr), tcpTransport("0-1")); resp.StatusCode != 401 {
		t.Errorf("c2 SETUP without credentials: status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthAllMethodsButOptionsChallenge(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	// RECORD is recognized by the classifier but not implemented: it must still be
	// challenged before the 501, so an unauthenticated probe learns nothing.
	for _, m := range []string{"SETUP", "PLAY", "GET_PARAMETER", "TEARDOWN", "RECORD"} {
		c := dial(t, addr)
		url := baseURL(addr)
		hdr := rtsp.Header{}
		if m == "SETUP" {
			url = trackURL(addr)
			hdr = tcpTransport("0-1")
		}
		if resp := c.do(t, m, url, hdr); resp.StatusCode != 401 {
			t.Errorf("%s without credentials: status = %d, want 401", m, resp.StatusCode)
		}
	}
}

func TestAuthBasicRejected(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c := dial(t, addr)
	h := rtsp.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(testUser+":"+testAuthToken)))
	if resp := c.do(t, methodDesc, baseURL(addr), h); resp.StatusCode != 401 {
		t.Errorf("Basic credentials: status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthDisabledGuardIsOpen(t *testing.T) {
	addr, _ := startServer(t, authConfig(""), defaultTrack())
	c := dial(t, addr)
	if resp := c.do(t, methodDesc, baseURL(addr), nil); resp.StatusCode != 200 {
		t.Errorf("DESCRIBE with an empty token configured: status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthRotationChallengesNegotiatingConnection is closure (b) for G3. A
// connection that authenticated with the old token but is still negotiating
// (it never reached SETUP, so its state is stateInit) is re-challenged on its
// next request after a rotation, keeps its socket, and re-authenticates
// transparently with the new token. A fresh connection must present the new
// token too.
func TestAuthRotationChallengesNegotiatingConnection(t *testing.T) {
	g := auth.NewGuard(testAuthToken)
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, Auth: g}, defaultTrack())
	c1 := dial(t, addr)
	ch := challengeOf(t, c1.do(t, methodDesc, baseURL(addr), nil))
	if resp := c1.do(t, methodDesc, baseURL(addr), answer(t, ch, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Fatalf("c1 auth: status = %d", resp.StatusCode)
	}
	g.Set("rotated-token-0001")
	// The connection authenticated under the old generation is re-challenged.
	reCh := challengeOf(t, c1.do(t, methodDesc, baseURL(addr), nil))
	// The old token no longer works: answering the re-challenge with it is
	// rejected and the connection (still stateInit) stays open to try again.
	if resp := c1.do(t, methodDesc, baseURL(addr), answer(t, reCh, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 401 {
		t.Errorf("old token after rotation on the same connection: status = %d, want 401", resp.StatusCode)
	}
	// It kept its socket (state was stateInit): answering with the new token on
	// the same connection succeeds.
	reCh2 := challengeOf(t, c1.do(t, methodDesc, baseURL(addr), nil))
	if resp := c1.do(t, methodDesc, baseURL(addr), answer(t, reCh2, "rotated-token-0001", methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Errorf("re-auth with the new token on the same connection: status = %d, want 200", resp.StatusCode)
	}
	// A new connection must present the new token.
	c2 := dial(t, addr)
	ch2 := challengeOf(t, c2.do(t, methodDesc, baseURL(addr), nil))
	if resp := c2.do(t, methodDesc, baseURL(addr), answer(t, ch2, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 401 {
		t.Errorf("old token on a new connection: status = %d, want 401", resp.StatusCode)
	}
	ch3 := challengeOf(t, c2.do(t, methodDesc, baseURL(addr), nil))
	if resp := c2.do(t, methodDesc, baseURL(addr), answer(t, ch3, "rotated-token-0001", methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Errorf("new token on a new connection: status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthEnableEvictsPlayingOpenAccessSession is closure (a) for G3. A session
// set up and playing while access was open is torn down on its next request
// once a token is enabled: the request is answered 401, the connection is
// closed, and the freed track slot is available to a second client. Without the
// eviction the stream would keep serving audio to a now-unauthenticated client
// and the single slot would stay held.
func TestAuthEnableEvictsPlayingOpenAccessSession(t *testing.T) {
	g := auth.NewGuard("")
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, Auth: g}, defaultTrack())
	c1 := dial(t, addr)
	setup := c1.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if setup.StatusCode != 200 {
		t.Fatalf("open-access SETUP: status = %d, want 200", setup.StatusCode)
	}
	play := rtsp.Header{}
	play.Set("Session", setup.Header.Get("Session"))
	if resp := c1.do(t, "PLAY", baseURL(addr), play); resp.StatusCode != 200 {
		t.Fatalf("open-access PLAY: status = %d, want 200", resp.StatusCode)
	}

	// Enable a token: the playing connection is now unauthenticated.
	g.Set(testAuthToken)
	if resp := c1.do(t, "GET_PARAMETER", baseURL(addr), nil); resp.StatusCode != 401 {
		t.Fatalf("GET_PARAMETER after a token was enabled: status = %d, want 401", resp.StatusCode)
	}
	// The connection is torn down after the challenge (state was past init).
	_ = c1.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c1.conn.Read(make([]byte, 1)); err == nil {
		t.Error("connection stayed open after eviction; want it closed")
	}

	// The slot the evicted connection held is released, so a second client that
	// presents the token can take it.
	c2 := dial(t, addr)
	ch := challengeOf(t, c2.do(t, methodDesc, baseURL(addr), nil))
	if resp := c2.do(t, methodDesc, baseURL(addr), answer(t, ch, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Fatalf("c2 auth: status = %d, want 200", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if resp := c2.do(t, "SETUP", trackURL(addr), tcpTransport("0-1")); resp.StatusCode == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("SETUP after eviction never got the slot")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAuthenticatedClientTCPDropReleasesSlot proves the track slot an
// authenticated client claimed at SETUP is released when its TCP connection
// simply drops (no TEARDOWN), so the next client can take it.
func TestAuthenticatedClientTCPDropReleasesSlot(t *testing.T) {
	addr, _ := startServer(t, authConfig(testAuthToken), defaultTrack())
	c := dial(t, addr)
	ch := challengeOf(t, c.do(t, methodDesc, baseURL(addr), nil))
	if resp := c.do(t, methodDesc, baseURL(addr), answer(t, ch, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Fatalf("auth: status = %d", resp.StatusCode)
	}
	if resp := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1")); resp.StatusCode != 200 {
		t.Fatalf("SETUP: status = %d", resp.StatusCode)
	}
	_ = c.conn.Close()
	// A second client must be able to take the slot once the first connection
	// is gone (the deferred teardown in serveConn releases it).
	c2 := dial(t, addr)
	ch2 := challengeOf(t, c2.do(t, methodDesc, baseURL(addr), nil))
	if resp := c2.do(t, methodDesc, baseURL(addr), answer(t, ch2, testAuthToken, methodDesc, baseURL(addr))); resp.StatusCode != 200 {
		t.Fatalf("c2 auth: status = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp := c2.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
		if resp.StatusCode == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SETUP after the first client dropped: status = %d, want 200", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
