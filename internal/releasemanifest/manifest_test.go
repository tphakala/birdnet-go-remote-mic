package releasemanifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (priv ed25519.PrivateKey, trusted map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]ed25519.PublicKey{KeyID(pub): pub}
}

func validManifest() *Manifest {
	return &Manifest{
		Schema:   Schema,
		Version:  "v1.2.3",
		Date:     time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		NotesURL: "https://github.com/" + Repository + "/releases/tag/v1.2.3",
		Targets: map[string]Target{
			"linux/arm64": {
				URL:    "https://github.com/" + Repository + "/releases/download/v1.2.3/a.tar.gz",
				Size:   42,
				SHA256: strings.Repeat("ab", 32),
				Binary: Binary{Path: "remote-mic", Size: 7, SHA256: strings.Repeat("cd", 32)},
			},
		},
	}
}

func signed(t *testing.T, priv ed25519.PrivateKey) (body, sig []byte) {
	t.Helper()
	body, err := Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	sig, err = Sign(priv, body)
	if err != nil {
		t.Fatal(err)
	}
	return body, sig
}

// TestSignVerifyRoundTrip pins that a signed manifest verifies and parses back
// to what was signed.
func TestSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	body, sig := signed(t, priv)
	m, err := Verify(body, sig, trusted)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if want := validManifest(); !reflect.DeepEqual(m, want) {
		t.Errorf("got %+v, want %+v", m, want)
	}
}

// TestVerifyRejectsTampering pins that changing any byte of the manifest, or
// of the signature, fails verification.
func TestVerifyRejectsTampering(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	body, sig := signed(t, priv)

	changed := bytes.Replace(body, []byte(`"size": 42`), []byte(`"size": 43`), 1)
	if bytes.Equal(changed, body) {
		t.Fatal("tamper did not change the manifest")
	}
	if _, err := Verify(changed, sig, trusted); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered manifest: err = %v, want ErrBadSignature", err)
	}

	var s Signature
	s.Schema, s.KeyID = Schema, KeyID(trusted[firstKey(trusted)])
	s.Signature = make([]byte, ed25519.SignatureSize)
	badSig := mustSigFile(t, s)
	if _, err := Verify(body, badSig, trusted); !errors.Is(err, ErrBadSignature) {
		t.Errorf("zero signature: err = %v, want ErrBadSignature", err)
	}
}

// TestVerifyRejectsUntrustedKey pins that a valid signature by a key the build
// does not list is refused.
func TestVerifyRejectsUntrustedKey(t *testing.T) {
	t.Parallel()
	priv, _ := testKey(t)
	_, otherTrusted := testKey(t)
	body, sig := signed(t, priv)
	if _, err := Verify(body, sig, otherTrusted); !errors.Is(err, ErrUntrustedKey) {
		t.Errorf("err = %v, want ErrUntrustedKey", err)
	}
}

// TestSignatureIsDomainSeparated pins that the signature covers the context
// prefix, so a bare Ed25519 signature over the manifest bytes is refused.
func TestSignatureIsDomainSeparated(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	body, _ := signed(t, priv)
	bare := Signature{Schema: Schema, KeyID: firstKey(trusted), Signature: ed25519.Sign(priv, body)}
	if _, err := Verify(body, mustSigFile(t, bare), trusted); !errors.Is(err, ErrBadSignature) {
		t.Errorf("bare signature: err = %v, want ErrBadSignature", err)
	}
}

// TestParseToleratesUnknownFields pins the additive-schema rule: a field a
// later release adds does not break an older reader.
func TestParseToleratesUnknownFields(t *testing.T) {
	t.Parallel()
	body, err := Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	withExtra := bytes.Replace(body, []byte("{\n"), []byte("{\n  \"futureField\": true,\n"), 1)
	if _, err := Parse(withExtra); err != nil {
		t.Errorf("Parse with an unknown field: %v", err)
	}
}

// TestValidate covers each rule Validate enforces.
func TestValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
		want   error
	}{
		{"valid", func(*Manifest) {}, nil},
		{"prerelease", func(m *Manifest) { m.Version = "v1.2.3-rc.1" }, nil},
		{"newer schema", func(m *Manifest) { m.Schema = Schema + 1 }, ErrUnsupportedSchema},
		{"no v prefix", func(m *Manifest) { m.Version = "1.2.3" }, ErrInvalid},
		{"leading zero", func(m *Manifest) { m.Version = "v1.02.3" }, ErrInvalid},
		{"no date", func(m *Manifest) { m.Date = time.Time{} }, ErrInvalid},
		{"http notes", func(m *Manifest) { m.NotesURL = "http://example.com/" }, ErrInvalid},
		{"no targets", func(m *Manifest) { m.Targets = nil }, ErrInvalid},
		{"http target", func(m *Manifest) { setTarget(m, func(t *Target) { t.URL = "http://example.com/a" }) }, ErrInvalid},
		{"zero size", func(m *Manifest) { setTarget(m, func(t *Target) { t.Size = 0 }) }, ErrInvalid},
		{"short sha", func(m *Manifest) { setTarget(m, func(t *Target) { t.SHA256 = "abcd" }) }, ErrInvalid},
		{"upper sha", func(m *Manifest) { setTarget(m, func(t *Target) { t.SHA256 = strings.Repeat("AB", 32) }) }, ErrInvalid},
		{"binary in a directory", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "dir/remote-mic" }) }, nil},
		{"no binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "" }) }, ErrInvalid},
		{"absolute binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "/usr/bin/remote-mic" }) }, ErrInvalid},
		{"escaping binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "../remote-mic" }) }, ErrInvalid},
		{"parent binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = ".." }) }, ErrInvalid},
		{"wrong binary name", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "evil" }) }, ErrInvalid},
		{"binary two dirs deep", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "a/b/remote-mic" }) }, ErrInvalid},
		{"binary under parent dir", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "../remote-mic" }) }, ErrInvalid},
		{"backslash binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = `dir\remote-mic` }) }, ErrInvalid},
		{"drive binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "C:/remote-mic" }) }, ErrInvalid},
		{"empty prerelease identifier", func(m *Manifest) { m.Version = "v1.2.3-rc..1" }, ErrInvalid},
		{"trailing prerelease dot", func(m *Manifest) { m.Version = "v1.2.3-." }, ErrInvalid},
		{"unclean binary path", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Path = "./remote-mic" }) }, ErrInvalid},
		{"zero binary size", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.Size = 0 }) }, ErrInvalid},
		{"bad binary sha", func(m *Manifest) { setTarget(m, func(t *Target) { t.Binary.SHA256 = "abcd" }) }, ErrInvalid},
		{"unknown requirement", func(m *Manifest) { m.Requires = []string{"config-migration-v2"} }, ErrUnsupportedRequirement},
	} {
		m := validManifest()
		tc.mutate(m)
		err := m.Validate()
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestTargetKey pins the target names, which appliances look themselves up by.
func TestTargetKey(t *testing.T) {
	t.Parallel()
	const linux = "linux"
	for _, tc := range []struct{ goos, goarch, goarm, want string }{
		{linux, "amd64", "", "linux/amd64"},
		{linux, "arm64", "", "linux/arm64"},
		{linux, "arm", "6", "linux/armv6"},
	} {
		if got := TargetKey(tc.goos, tc.goarch, tc.goarm); got != tc.want {
			t.Errorf("TargetKey(%s, %s, %s) = %q, want %q", tc.goos, tc.goarch, tc.goarm, got, tc.want)
		}
	}
}

// TestTrustedKeys pins that the compiled-in key list is non-empty and parses,
// so no build ships unable to verify any manifest.
func TestTrustedKeys(t *testing.T) {
	t.Parallel()
	keys, err := TrustedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 || len(keys) != len(trustedPublicKeys) {
		t.Errorf("got %d trusted keys from %d entries, want one per entry and at least one", len(keys), len(trustedPublicKeys))
	}
}

// TestParsePrivateKeyDoesNotEchoInput pins that a malformed secret is not
// quoted back in the error, where CI would log it.
func TestParsePrivateKeyDoesNotEchoInput(t *testing.T) {
	t.Parallel()
	const secret = "not-base64-secret!!"
	_, err := ParsePrivateKey(secret)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("err = %v, want an error that does not contain the input", err)
	}
}

func setTarget(m *Manifest, f func(*Target)) {
	t := m.Targets["linux/arm64"]
	f(&t)
	m.Targets["linux/arm64"] = t
}

func firstKey(keys map[string]ed25519.PublicKey) string {
	for id := range keys {
		return id
	}
	return ""
}

func mustSigFile(t *testing.T, s Signature) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVerifyRejectsMalformedInput covers each way a downloaded pair can be
// unusable before or after the signature check.
func TestVerifyRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	body, sig := signed(t, priv)
	newerSig := bytes.Replace(sig, []byte(`"schema": 1`), []byte(`"schema": 2`), 1)
	if bytes.Equal(newerSig, sig) {
		t.Fatal("schema edit did not change the signature file")
	}
	junk := []byte("not json")
	junkSig, err := Sign(priv, junk)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		body, sig []byte
		want      error
	}{
		{"signature not json", body, junk, ErrBadSignature},
		{"newer signature schema", body, newerSig, ErrUnsupportedSchema},
		{"signed junk", junk, junkSig, ErrInvalid},
	} {
		if _, err := Verify(tc.body, tc.sig, trusted); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestVerifyRejectsOversizeAndBadKeys pins the download size limits, and that
// a wrong-length trusted key is refused instead of panicking in ed25519.Verify.
func TestVerifyRejectsOversizeAndBadKeys(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	body, sig := signed(t, priv)
	big := append(bytes.Clone(body), bytes.Repeat([]byte(" "), MaxManifestSize)...)
	if _, err := Verify(big, sig, trusted); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversize manifest: err = %v, want ErrInvalid", err)
	}
	bigSig := append(bytes.Clone(sig), bytes.Repeat([]byte(" "), MaxSignatureSize)...)
	if _, err := Verify(body, bigSig, trusted); !errors.Is(err, ErrBadSignature) {
		t.Errorf("oversize signature: err = %v, want ErrBadSignature", err)
	}
	short := map[string]ed25519.PublicKey{firstKey(trusted): trusted[firstKey(trusted)][:16]}
	if _, err := Verify(body, sig, short); !errors.Is(err, ErrUntrustedKey) {
		t.Errorf("short trusted key: err = %v, want ErrUntrustedKey", err)
	}
}

// TestParseKeysRejectMalformed pins that a key of the wrong encoding or size
// is refused rather than truncated or padded.
func TestParseKeysRejectMalformed(t *testing.T) {
	t.Parallel()
	short := "AAAA" // valid base64, 3 bytes
	for _, s := range []string{"!!!", short} {
		if _, err := ParsePublicKey(s); err == nil {
			t.Errorf("ParsePublicKey(%q): got nil error", s)
		}
		if _, err := ParsePrivateKey(s); err == nil {
			t.Errorf("ParsePrivateKey(%q): got nil error", s)
		}
	}
}

// TestValidateRejectsUnparsableURL pins that a URL url.Parse rejects is
// invalid, not just a non-https one.
func TestValidateRejectsUnparsableURL(t *testing.T) {
	t.Parallel()
	m := validManifest()
	m.NotesURL = "https://exa mple.com/%zz"
	if err := m.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// TestParsePrivateKeyRoundTrip pins that the seed form keygen writes decodes
// back to the same key pair.
func TestParsePrivateKeyRoundTrip(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	got, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := got.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("parsed key has no Ed25519 public key")
	}
	if _, trustedPub := trusted[KeyID(pub)]; !trustedPub {
		t.Errorf("parsed key id %s does not match the original", KeyID(pub))
	}
}
