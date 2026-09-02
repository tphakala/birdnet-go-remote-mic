package auth

import (
	"crypto/md5" //nolint:gosec // RFC 7616 Digest over RTSP is MD5 by definition; the token itself never crosses the wire.
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// CheckDigest reports whether an Authorization header value is a valid Digest
// answer, for the given request method, to the challenge this guard issued
// under nonce. It accepts the RFC 7616 qop=auth form (cnonce and nc present)
// and the RFC 2069 legacy form (no qop). The username is free-form: the shared
// token is the password, so any client can use whatever username its URL
// carries. The uri the client hashed is used as-is rather than compared with
// the request URL: clients disagree on trailing slashes and Content-Base forms,
// and the nonce already binds the answer to this connection. Comparisons are
// constant-time. A disabled or nil guard, or an empty nonce, rejects everything.
func (g *Guard) CheckDigest(method, authorization, nonce string) bool {
	token := g.current()
	if token == "" || nonce == "" {
		return false
	}
	// Reuse go-audio-stream's RFC 7235 header parser so the server accepts
	// exactly the grammar the client emits (whitespace around "=", any param
	// order, quoted values); a second hand-rolled parser could drift from it
	// and reject RFC-legal headers.
	chs := rtsp.ParseChallenges([]string{authorization})
	if len(chs) != 1 || chs[0].Scheme != rtsp.AuthDigest {
		return false
	}
	p := chs[0].Params
	username, uri, response := p["username"], p["uri"], p["response"]
	if username == "" || uri == "" || response == "" || p["realm"] != Realm {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(p["nonce"]), []byte(nonce)) != 1 {
		return false
	}
	if alg := p["algorithm"]; alg != "" && !strings.EqualFold(alg, "MD5") {
		return false
	}

	ha1 := md5hex(username + ":" + Realm + ":" + token)
	ha2 := md5hex(method + ":" + uri)
	var expected string
	switch qop := p["qop"]; {
	case qop == "":
		expected = md5hex(ha1 + ":" + nonce + ":" + ha2)
	case strings.EqualFold(qop, "auth"):
		nc, cnonce := p["nc"], p["cnonce"]
		if nc == "" || cnonce == "" {
			return false
		}
		// Hash the qop token exactly as the client presented it (e.g. "AUTH"),
		// not the literal "auth": RFC 7616 has the client fold the presented
		// value into its own response, so folding "auth" here would reject a
		// client that sent a differently-cased but legal qop.
		expected = md5hex(strings.Join([]string{ha1, nonce, nc, cnonce, qop, ha2}, ":"))
	default:
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(response)), []byte(expected)) == 1
}

// md5hex returns the lowercase hex MD5 of s, the digest form RFC 7616 specifies.
func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // see the import comment: Digest mandates MD5.
	return hex.EncodeToString(sum[:])
}
