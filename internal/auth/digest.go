package auth

import (
	"crypto/md5" //nolint:gosec // RFC 7616 Digest over RTSP is MD5 by definition; the token itself never crosses the wire.
	"crypto/subtle"
	"encoding/hex"
	"strings"
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
	scheme, rest, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Digest") {
		return false
	}
	p := parseDigestParams(rest)
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
		expected = md5hex(strings.Join([]string{ha1, nonce, nc, cnonce, "auth", ha2}, ":"))
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

// parseDigestParams parses the auth-params of a Digest credential ("k=v, k="v
// with \"escapes\"", ...) into a map keyed by lowercased name. Malformed input
// yields whatever parsed cleanly; the caller's field checks reject the rest.
func parseDigestParams(s string) map[string]string {
	params := make(map[string]string, 10)
	for i := 0; i < len(s); {
		// Skip separators between parameters.
		for i < len(s) && (s[i] == ',' || s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[i : i+eq]))
		i += eq + 1
		var value string
		if i < len(s) && s[i] == '"' {
			i++
			var b strings.Builder
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++ // closing quote (or end of input)
			value = b.String()
		} else {
			start := i
			for i < len(s) && s[i] != ',' && s[i] != ' ' && s[i] != '\t' {
				i++
			}
			value = s[start:i]
		}
		if key != "" {
			params[key] = value
		}
	}
	return params
}
