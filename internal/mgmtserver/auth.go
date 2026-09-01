package mgmtserver

import (
	"net/http"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
)

// WithAuth mounts g as the bearer-token gate for every API route except
// /healthz. While the guard is disabled (no token configured) the API serves
// open access exactly as without this option; a hot reload that sets a token
// starts enforcing on the next request, no restart needed. Static UI assets
// stay open so the login screen can load.
func WithAuth(g *auth.Guard) Option {
	return func(s *Server) { s.guard = g }
}

// requireBearer wraps next with the RFC 6750 bearer check. A missing or wrong
// credential yields 401 with a WWW-Authenticate challenge (carrying
// error="invalid_token" when a bearer was presented but rejected) and an RFC
// 9457 problem body. The liveness probe is exempt so a restart watcher and a
// load balancer can poll it without the token.
func requireBearer(g *auth.Guard, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.Enabled() || r.URL.Path == BasePath+"/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		if g.CheckBearer(header) {
			next.ServeHTTP(w, r)
			return
		}
		challenge := `Bearer realm="` + auth.Realm + `"`
		if header != "" {
			challenge += `, error="invalid_token"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "a valid access token is required (Authorization: Bearer <token>)")
	})
}
