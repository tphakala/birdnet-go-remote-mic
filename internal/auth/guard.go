// Package auth holds the appliance's shared access token and answers the two
// checks built on it: HTTP Bearer (RFC 6750) for the management API and web UI,
// and RTSP Digest (RFC 7616 with RFC 2069 legacy) for the stream. One secret
// gates both; an empty token means open access.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
)

// Realm is the protection space named in every challenge.
const Realm = "birdnet-go-remote-mic"

const (
	minTokenLen = 12
	maxTokenLen = 128
)

// tokenState pairs the active token with its generation counter. Guard holds it
// behind a single atomic.Pointer so a reader always sees a consistent
// (token, generation) pair, even while Set swaps them.
type tokenState struct {
	token string
	// gen counts token changes. It advances only when Set installs a token
	// that differs from the current one, so the RTSP server can detect that a
	// connection authenticated under a superseded token (or under open access)
	// and re-challenge it. Setting the same token again does not advance it, so
	// an idempotent reconcile never disturbs a playing session.
	gen uint64
}

// Guard holds the active token and checks credentials against it. It is safe
// for concurrent use: Set swaps the token and its generation atomically as one
// pair so a hot reload applies without a restart, and every check reads the
// token once.
//
// A nil *Guard is a disabled guard: Enabled reports false and every check
// fails, so a caller that forgot to wire one can never authenticate by accident.
type Guard struct {
	// state holds the token and its generation together. A nil pointer is the
	// zero state (open access, generation 0). Readers load it once; Set builds
	// the next state from the loaded one under a compare-and-swap loop, so the
	// token and generation always move as an atomic pair and concurrent Sets
	// cannot lose an update.
	state atomic.Pointer[tokenState]
}

// NewGuard returns a Guard holding token. An empty token disables the guard.
func NewGuard(token string) *Guard {
	g := &Guard{}
	g.Set(token)
	return g
}

// Set replaces the active token. An empty token disables the guard (open
// access); callers skip their checks while Enabled reports false. When the new
// token differs from the current one, Generation advances so the RTSP server
// evicts sessions authenticated under the old token; setting the same value is
// a no-op for eviction. The token and generation are swapped together as one
// atomic pair, built from the current state under a compare-and-swap loop, so
// concurrent Sets cannot double-count the generation and a reader never sees a
// new token paired with an old generation. Set on a nil *Guard panics; the nil
// case is test-only.
func (g *Guard) Set(token string) {
	for {
		old := g.state.Load()
		var oldToken string
		var oldGen uint64
		if old != nil {
			oldToken, oldGen = old.token, old.gen
		}
		next := &tokenState{token: token, gen: oldGen}
		if token != oldToken {
			next.gen = oldGen + 1
		}
		if g.state.CompareAndSwap(old, next) {
			return
		}
	}
}

// Generation returns a counter that advances every time Set installs a token
// different from the current one. The RTSP server records the generation at
// which a connection authenticated and re-challenges once it advances, so
// enabling or rotating a token evicts connections authenticated under the old
// one (or under open access). A nil guard reports 0.
func (g *Guard) Generation() uint64 {
	if g == nil {
		return 0
	}
	if st := g.state.Load(); st != nil {
		return st.gen
	}
	return 0
}

// current returns the active token, or "" for a nil or disabled guard.
func (g *Guard) current() string {
	if g == nil {
		return ""
	}
	if st := g.state.Load(); st != nil {
		return st.token
	}
	return ""
}

// Enabled reports whether a token is set, meaning credentials are required.
func (g *Guard) Enabled() bool {
	return g.current() != ""
}

// Snapshot returns the enabled state and generation from a single atomic load,
// so a caller that needs both sees a consistent (enabled, gen) pair rather than
// risking a token rotation between two separate reads of Enabled and
// Generation. The RTSP writer's eviction check and session authorization both
// need the pair. A nil or never-configured guard reports (false, 0); a guard
// whose token was cleared reports (false, gen) with gen at its advanced value,
// since clearing the token disables the guard but still advances the
// generation to evict sessions authenticated under the old token.
func (g *Guard) Snapshot() (enabled bool, gen uint64) {
	if g == nil {
		return false, 0
	}
	if st := g.state.Load(); st != nil {
		return st.token != "", st.gen
	}
	return false, 0
}

// CheckBearer reports whether an Authorization header value carries the
// active token as an RFC 6750 bearer credential ("Bearer <token>", scheme
// case-insensitive). A disabled guard rejects everything; callers must consult
// Enabled to decide whether to check at all.
//
// Both sides are hashed before the constant-time compare: ConstantTimeCompare
// returns early on a length mismatch, which would otherwise leak the length of
// the configured token to a probing client.
func (g *Guard) CheckBearer(authorization string) bool {
	token := g.current()
	if token == "" {
		return false
	}
	scheme, presented, ok := strings.Cut(authorization, " ")
	// RFC 6750 section 2.1 is "Bearer" 1*SP b64token, so one or more spaces
	// separate the scheme from the credential. Drop the extra separators before
	// comparing; a space or tab INSIDE the credential is still a rejection,
	// because the token charset has neither.
	presented = strings.TrimLeft(presented, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || presented == "" || strings.ContainsAny(presented, " \t") {
		return false
	}
	want := sha256.Sum256([]byte(token))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// NewNonce returns a fresh random Digest nonce (16 bytes, hex encoded). The
// RTSP server issues one per connection and accepts answers only under it, so
// a captured Authorization header is useless on any other connection.
func (g *Guard) NewNonce() string {
	var b [16]byte
	// crypto/rand.Read never returns an error: on an unrecoverable entropy
	// failure it terminates the process (a runtime fatal, not a recoverable
	// panic), so the error is dropped by design.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DigestChallenge returns the WWW-Authenticate value for nonce. MD5 is the one
// algorithm every RTSP client answers (ffmpeg and live555 do not do SHA-256
// for RTSP); qop="auth" is advertised because go-audio-stream and ffmpeg answer
// it, while CheckDigest also accepts the RFC 2069 form from clients that ignore
// qop (live555, VLC).
func (g *Guard) DigestChallenge(nonce string) string {
	return fmt.Sprintf(`Digest realm=%q, nonce=%q, algorithm=MD5, qop="auth"`, Realm, nonce)
}

// ValidToken reports why token is not an acceptable shared token, or "" when
// it is. An empty token is valid and means open access. A non-empty token
// must be 12..128 characters from the URL-unreserved set [A-Za-z0-9._~-], so
// it can sit in an RTSP URL without percent-encoding and inside a Digest
// quoted-string without escaping.
func ValidToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) < minTokenLen {
		return fmt.Sprintf("must be at least %d characters", minTokenLen)
	}
	if len(token) > maxTokenLen {
		return fmt.Sprintf("must be at most %d characters", maxTokenLen)
	}
	for i := 0; i < len(token); i++ {
		if !unreserved(token[i]) {
			return "must contain only letters, digits, and . _ ~ -"
		}
	}
	return ""
}

// unreserved reports whether c is in the RFC 3986 unreserved set.
func unreserved(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '~', c == '-':
		return true
	}
	return false
}
