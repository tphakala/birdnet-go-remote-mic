package rtspserver

import "testing"

// TestShouldEvictAndAuthorizedUnder pins the shared auth predicate used by both
// the media writer (proactive eviction) and session authorization, so the two
// sites cannot drift. The load-bearing case is an enabled guard sitting at
// generation 0: a bare authGen != guardGen compare (0 != 0) would keep an
// open-access connection streaming after a token is enabled, so eviction must
// instead gate on whether the connection ever authenticated under the current
// generation.
func TestShouldEvictAndAuthorizedUnder(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		guardGen   uint64
		authed     bool
		authGen    uint64
		wantEvict  bool
		wantAuthzd bool
	}{
		{"guard disabled, open access", false, 0, false, 0, false, false},
		{"authed under current gen", true, 3, true, 3, false, true},
		{"authed under superseded gen", true, 4, true, 3, true, false},
		{"never authed, token enabled", true, 1, false, 0, true, false},
		{"never authed, enabled guard at gen 0", true, 0, false, 0, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldEvict(tt.enabled, tt.guardGen, tt.authed, tt.authGen); got != tt.wantEvict {
				t.Errorf("shouldEvict(%v, %d, %v, %d) = %v, want %v", tt.enabled, tt.guardGen, tt.authed, tt.authGen, got, tt.wantEvict)
			}
			if got := authorizedUnder(tt.authed, tt.authGen, tt.guardGen); got != tt.wantAuthzd {
				t.Errorf("authorizedUnder(%v, %d, %d) = %v, want %v", tt.authed, tt.authGen, tt.guardGen, got, tt.wantAuthzd)
			}
		})
	}
}
