package auth

import (
	"errors"
	"testing"
)

// TestGenerateTokenReturnsEntropyError asserts a random-source failure is
// surfaced, never swallowed into a weak or empty token.
func TestGenerateTokenReturnsEntropyError(t *testing.T) {
	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() { randRead = orig }()
	if _, err := GenerateToken(); err == nil {
		t.Fatal("expected an error when the random source fails")
	}
}

// TestGenerateTokenIsValid asserts a generated token passes ValidToken, so a
// token seeded by `init` is always accepted by the guard.
func TestGenerateTokenIsValid(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if msg := ValidToken(tok); msg != "" {
		t.Fatalf("generated token %q is not valid: %s", tok, msg)
	}
}

// TestGenerateTokenHasEntropy asserts the token is long enough to be a real
// secret, not a short guessable string that merely clears the 12-char floor.
func TestGenerateTokenHasEntropy(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(tok) < 24 {
		t.Fatalf("token %q is too short (%d chars); want >= 24", tok, len(tok))
	}
}

// TestGenerateTokenIsUnique asserts successive calls do not repeat, catching a
// generator that forgets to read fresh randomness each time.
func TestGenerateTokenIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = true
	}
}
