package auth

import (
	"encoding/hex"
	"strings"
	"sync"
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
		{"separator spaces only, no credential", testToken, "Bearer   ", false},
		{"leading whitespace before scheme", testToken, " Bearer " + testToken, false},
		{"multi-space separator plus trailing junk", testToken, "Bearer  " + testToken + " x", false},
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

func TestSnapshot(t *testing.T) {
	// Snapshot returns the enabled state and generation from a single load, so a
	// caller needing both (the RTSP writer's eviction check, authorized()) sees a
	// consistent pair. Its (enabled, gen) must track Set the same way Enabled and
	// Generation do separately.
	steps := []struct {
		set         string
		wantEnabled bool
		wantGen     uint64
	}{
		{"", false, 0},                  // open access from the start
		{testToken, true, 1},            // enabling advances once
		{testToken, true, 1},            // same token: idempotent
		{"rotated-token-0001", true, 2}, // rotating advances
		{"", false, 3},                  // clearing advances, disables
	}
	g := NewGuard("")
	for i, s := range steps {
		g.Set(s.set)
		enabled, gen := g.Snapshot()
		if enabled != s.wantEnabled || gen != s.wantGen {
			t.Errorf("step %d Set(%q): Snapshot = (%v, %d), want (%v, %d)", i, s.set, enabled, gen, s.wantEnabled, s.wantGen)
		}
	}
}

func TestNilGuardSnapshot(t *testing.T) {
	var g *Guard
	if enabled, gen := g.Snapshot(); enabled || gen != 0 {
		t.Errorf("nil guard Snapshot = (%v, %d), want (false, 0)", enabled, gen)
	}
}

func TestZeroValueGuardIsOpenAccess(t *testing.T) {
	// A guard constructed as a zero value and never routed through Set (its state
	// pointer still nil) is a never-configured, open-access guard: disabled at
	// generation 0, exactly as the Snapshot and Generation docs promise for the
	// never-configured case (distinct from the nil-pointer guard above).
	var g Guard
	if enabled, gen := g.Snapshot(); enabled || gen != 0 {
		t.Errorf("zero-value guard Snapshot = (%v, %d), want (false, 0)", enabled, gen)
	}
	if got := g.Generation(); got != 0 {
		t.Errorf("zero-value guard Generation = %d, want 0", got)
	}
	if g.Enabled() {
		t.Error("zero-value guard must report disabled")
	}
}

func TestConcurrentSetAdvancesGenerationOnce(t *testing.T) {
	// Enabling the token is racy: PatchConfig sets it while the startup reconcile
	// may set the same token concurrently (the management API is mounted before
	// the reconcile runs). Set moves the token and its generation as one atomic
	// pair under a compare-and-swap loop, so N concurrent Sets of the SAME new
	// token must advance the generation exactly once. A store-then-add
	// implementation could double-count or lose an update; this guards that fix.
	const goroutines = 64
	g := NewGuard("")
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			g.Set(testToken)
		}()
	}
	close(start)
	wg.Wait()
	if got := g.Generation(); got != 1 {
		t.Errorf("after %d concurrent Set(sameToken): Generation = %d, want 1", goroutines, got)
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
