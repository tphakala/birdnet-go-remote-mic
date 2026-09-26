// Package releasemanifest defines the signed release manifest published with
// every tagged release (manifest.json plus manifest.json.sig), and how it is
// signed and verified.
//
// The release workflow writes and signs it (tools/releasemanifest). Anything
// that acts on a manifest (an update check, an updater installing a release)
// verifies it itself against the keys compiled into its own binary, never on
// another process's word. The
// signature covers the exact bytes of manifest.json, so there is no
// canonicalization to get wrong: a verifier checks the bytes it downloaded,
// then parses them.
//
// The package is platform-neutral and uses only the standard library, so the
// same code runs in CI, on the appliance, and in the updater.
package releasemanifest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Schema is the manifest schema this package reads and writes. It changes only
// for an incompatible change; new optional fields are added without a bump, and
// a reader ignores fields it does not know.
const Schema = 1

// File names of the manifest and its detached signature, as release assets.
const (
	FileName          = "manifest.json"
	SignatureFileName = "manifest.json.sig"
)

// Repository is the GitHub repository releases are published from.
const Repository = "tphakala/birdnet-go-remote-mic"

// LatestURL and LatestSignatureURL name the newest non-prerelease release's
// manifest. GitHub redirects the "latest/download" path to the asset, so a
// check is a plain download: it makes no REST API call, so the API's rate
// limit does not apply to it.
const (
	LatestURL          = "https://github.com/" + Repository + "/releases/latest/download/" + FileName
	LatestSignatureURL = "https://github.com/" + Repository + "/releases/latest/download/" + SignatureFileName
)

// signingContext prefixes every signed message. The key signs nothing else
// today; the prefix keeps it that way, so a manifest signature can never be
// replayed as a signature over some other payload if the key gains a second use.
const signingContext = "remote-mic release manifest v1\n"

// Manifest describes one release.
type Manifest struct {
	Schema int `json:"schema"`
	// Version is the release tag, v-prefixed as `remote-mic version` prints it.
	Version string `json:"version"`
	// Date is when the release was built, in UTC.
	Date time.Time `json:"date"`
	// NotesURL is the release page with the release notes.
	NotesURL string `json:"notesUrl"`
	// Targets maps a target key (see TargetKey) to its release tarball.
	Targets map[string]Target `json:"targets"`
}

// Target is the release tarball for one platform.
type Target struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Signature is the content of manifest.json.sig.
type Signature struct {
	Schema int `json:"schema"`
	// KeyID names the signing key (see KeyID), so a verifier holding several
	// trusted keys during a rotation knows which one to check against.
	KeyID string `json:"keyId"`
	// Signature is the Ed25519 signature, base64 in JSON.
	Signature []byte `json:"signature"`
}

// Sentinel errors, for errors.Is.
var (
	ErrUnsupportedSchema = errors.New("unsupported manifest schema")
	ErrUntrustedKey      = errors.New("manifest signed by an untrusted key")
	ErrBadSignature      = errors.New("manifest signature does not verify")
	ErrInvalid           = errors.New("invalid manifest")
)

// versionPattern accepts a v-prefixed semantic version, with an optional
// prerelease suffix and no leading zeros.
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// sha256Pattern is a lowercase hex SHA-256 digest.
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TargetKey names a release target in Manifest.Targets: "linux/amd64",
// "linux/arm64", and "linux/armv6" for 32-bit arm (goarm is ignored for other
// architectures).
func TargetKey(goos, goarch, goarm string) string {
	if goarch == "arm" {
		return goos + "/armv" + goarm
	}
	return goos + "/" + goarch
}

// ValidVersion reports whether v is a v-prefixed semantic version.
func ValidVersion(v string) bool {
	return versionPattern.MatchString(v)
}

// Validate checks that m is complete and well formed.
func (m *Manifest) Validate() error {
	if m.Schema != Schema {
		return fmt.Errorf("%w: %d (this build reads %d)", ErrUnsupportedSchema, m.Schema, Schema)
	}
	if !ValidVersion(m.Version) {
		return fmt.Errorf("%w: version %q is not a v-prefixed semantic version", ErrInvalid, m.Version)
	}
	if m.Date.IsZero() {
		return fmt.Errorf("%w: no date", ErrInvalid)
	}
	if err := checkHTTPS(m.NotesURL); err != nil {
		return fmt.Errorf("%w: notesUrl: %w", ErrInvalid, err)
	}
	if len(m.Targets) == 0 {
		return fmt.Errorf("%w: no targets", ErrInvalid)
	}
	for key, t := range m.Targets {
		if err := checkHTTPS(t.URL); err != nil {
			return fmt.Errorf("%w: target %s: %w", ErrInvalid, key, err)
		}
		if t.Size <= 0 {
			return fmt.Errorf("%w: target %s: size %d", ErrInvalid, key, t.Size)
		}
		if !sha256Pattern.MatchString(t.SHA256) {
			return fmt.Errorf("%w: target %s: sha256 %q is not 64 lowercase hex digits", ErrInvalid, key, t.SHA256)
		}
	}
	return nil
}

func checkHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q is not an https URL", raw)
	}
	return nil
}

// Marshal validates m and encodes it as the bytes to publish and sign.
func Marshal(m *Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Parse decodes and validates a manifest. It does not check a signature; use
// Verify for anything downloaded.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// KeyID derives a short, stable name for a public key: the first 8 bytes of
// its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Sign signs the exact manifest bytes and returns the content of
// manifest.json.sig.
func Sign(priv ed25519.PrivateKey, manifest []byte) ([]byte, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("private key has no Ed25519 public key")
	}
	sig := Signature{
		Schema:    Schema,
		KeyID:     KeyID(pub),
		Signature: ed25519.Sign(priv, signedMessage(manifest)),
	}
	b, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Verify checks that sig is a valid signature over the exact manifest bytes by
// one of the trusted keys (keyed by KeyID), then parses and validates the
// manifest. Nothing in an unverified manifest is trusted, including its schema.
func Verify(manifest, sig []byte, trusted map[string]ed25519.PublicKey) (*Manifest, error) {
	var s Signature
	if err := json.Unmarshal(sig, &s); err != nil {
		return nil, fmt.Errorf("%w: signature file: %w", ErrBadSignature, err)
	}
	if s.Schema != Schema {
		return nil, fmt.Errorf("%w: signature schema %d (this build reads %d)", ErrUnsupportedSchema, s.Schema, Schema)
	}
	pub, ok := trusted[s.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: key id %q", ErrUntrustedKey, s.KeyID)
	}
	if !ed25519.Verify(pub, signedMessage(manifest), s.Signature) {
		return nil, ErrBadSignature
	}
	return Parse(manifest)
}

func signedMessage(manifest []byte) []byte {
	msg := make([]byte, 0, len(signingContext)+len(manifest))
	msg = append(msg, signingContext...)
	return append(msg, manifest...)
}

// ParsePublicKey decodes a base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key: %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey decodes a base64 Ed25519 seed, the form keygen writes and the
// release workflow's secret holds.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("private key: not base64") // never echo key material
	}
	if len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("private key: %d bytes, want a %d-byte seed", len(b), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// TrustedKeys returns the release signing keys this build accepts, by KeyID.
func TrustedKeys() (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey, len(trustedPublicKeys))
	for _, s := range trustedPublicKeys {
		pub, err := ParsePublicKey(s)
		if err != nil {
			return nil, err
		}
		keys[KeyID(pub)] = pub
	}
	return keys, nil
}
