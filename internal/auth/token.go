package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// tokenEntropyBytes is how many random bytes GenerateToken draws. 24 bytes is
// 192 bits of entropy, past the 128-bit norm for a bearer secret, and
// base64url-encodes to 32 characters, comfortably inside ValidToken's 12..128
// band.
const tokenEntropyBytes = 24

// randRead fills b with cryptographic randomness. It is a package var so a test
// can inject an entropy failure and exercise the error path.
var randRead = func(b []byte) (int, error) { return io.ReadFull(rand.Reader, b) }

// GenerateToken returns a fresh random shared access token: 24 bytes of
// crypto/rand encoded with base64.RawURLEncoding. The RawURL alphabet
// ([A-Za-z0-9_-]) is a subset of the RFC 3986 unreserved set that ValidToken
// accepts, so the result needs no percent-encoding in an RTSP URL and no
// escaping in a Digest quoted-string. An entropy read failure is returned, never
// swallowed, so a caller never seeds a weak or empty token.
func GenerateToken() (string, error) {
	b := make([]byte, tokenEntropyBytes)
	if _, err := randRead(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
