package auth

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTokenRuleMatchesOpenAPIPattern guards against the token rule drifting
// between its two machine-readable encodings: the Go validator (ValidToken) and
// the OpenAPI pattern on AuthSettings.token. It parses the pattern out of the
// committed spec and asserts it accepts exactly the tokens ValidToken accepts, so
// a change to one without the other fails the build rather than shipping a
// contract that lies about what the appliance enforces.
func TestTokenRuleMatchesOpenAPIPattern(t *testing.T) {
	pattern := openAPITokenPattern(t)
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("OpenAPI token pattern %q does not compile: %v", pattern, err)
	}

	cases := []string{
		"",                       // empty: open access, accepted
		"short",                  // too short
		strings.Repeat("a", 11),  // one below the 12 minimum
		strings.Repeat("a", 12),  // exactly the minimum
		strings.Repeat("a", 128), // exactly the maximum
		strings.Repeat("a", 129), // one above the maximum
		"valid.token_1~2-3ok",    // the full unreserved set (. _ ~ -)
		"ABCDEFGHIJKL",           // uppercase letters (both sides accept A-Z)
		"abcdefghijkl!",          // an excluded punctuation char
		"abcdefghijkl ",          // a space
		"abcdefghijkl/",          // a slash
		"twelve+chars",           // a plus (not unreserved)
		"café-token12",           // a non-ASCII letter
	}
	for _, tok := range cases {
		specOK := re.MatchString(tok)
		goOK := ValidToken(tok) == ""
		if specOK != goOK {
			t.Errorf("token %q: OpenAPI pattern accepts=%v, Go ValidToken accepts=%v; the two token rules have drifted", tok, specOK, goOK)
		}
	}
}

// openAPITokenPattern extracts components.schemas.AuthSettings.properties.token.pattern
// from the committed OpenAPI spec, failing loudly if the shape moved rather than
// silently skipping the drift check.
func openAPITokenPattern(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	nav := func(m map[string]any, key string) map[string]any {
		v, ok := m[key].(map[string]any)
		if !ok {
			t.Fatalf("openapi.yaml: %q is missing or not a mapping; the spec shape moved", key)
		}
		return v
	}
	node := doc
	for _, key := range []string{"components", "schemas", "AuthSettings", "properties", "token"} {
		node = nav(node, key)
	}
	pattern, ok := node["pattern"].(string)
	if !ok || pattern == "" {
		t.Fatal("openapi.yaml: AuthSettings.token has no pattern; the token drift guard cannot run")
	}
	return pattern
}
