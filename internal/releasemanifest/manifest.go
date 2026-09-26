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
// A valid signature proves the manifest came from a release, not that it is
// the newest one: whoever controls the download can replay an older signed
// manifest, or keep serving the last one (a static asset cannot expire). So a
// consumer must act only on a version strictly newer than the one it runs,
// must not treat a prerelease version as an update unless the operator opted
// in, and must refuse a download longer than MaxManifestSize or
// MaxSignatureSize (read one byte past the limit to tell; a LimitReader alone
// truncates silently and surfaces as a bad signature). Fetch
// the manifest and its signature from the same release tag (resolve "latest"
// once), or a publish between the two downloads yields a mismatched pair.
//
// The package is platform-neutral and uses only the standard library, so the
// same code runs in CI, on the appliance, and in the updater.
package releasemanifest

import (
	"cmp"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Schema is the manifest schema this package reads and writes. New optional
// fields are added without a bump, and a reader ignores fields it does not
// know. A field an older reader must not ignore is announced in Requires
// instead. A bump is for an incompatible change only, and ships under a new
// file name published beside this one, since every installed appliance keeps
// fetching FileName and would refuse a manifest with a newer schema.
const Schema = 1

// File names of the manifest and its detached signature, as release assets.
const (
	FileName          = "manifest.json"
	SignatureFileName = "manifest.json.sig"
)

// BinaryName is the executable each release tarball holds.
const BinaryName = "remote-mic"

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

// Size limits for downloading the manifest and its signature; real ones are a
// few KiB and a few hundred bytes.
const (
	MaxManifestSize  = 64 << 10
	MaxSignatureSize = 4 << 10
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
	// Requires lists capabilities a reader must understand before acting on
	// this manifest (for example a mandatory migration step). A reader that
	// finds a value it does not know refuses the manifest instead of ignoring
	// it. Empty in schema 1 releases so far.
	Requires []string `json:"requires,omitempty"`
}

// Target is the release tarball for one platform.
type Target struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Binary is the executable inside the tarball, so an installer can check
	// the extracted file itself rather than trusting whoever unpacked it.
	Binary Binary `json:"binary"`
}

// Binary is the executable inside a release tarball.
type Binary struct {
	// Path is the entry name inside the tarball.
	Path   string `json:"path"`
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
	// ErrUnsupportedRequirement reports a Requires value this build does not know.
	ErrUnsupportedRequirement = errors.New("manifest requires a capability this build does not have")
)

// knownRequirements are the Requires values this build understands; none yet.
var knownRequirements = []string{}

// versionPattern accepts a v-prefixed SemVer 2.0 version: no leading zeros,
// and an optional prerelease of dot-separated identifiers, each numeric
// without a leading zero or alphanumeric. Build metadata (+...) is refused on
// purpose: SemVer ignores it for precedence, so two tags differing only in it
// would be the same version to an update check.
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(` + prereleaseIdent + `(?:\.` + prereleaseIdent + `)*))?$`)

// prereleaseIdent is one SemVer prerelease identifier.
const prereleaseIdent = `(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`

// sha256Pattern is a lowercase hex SHA-256 digest.
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TargetKey names a release target in Manifest.Targets: "linux/amd64",
// "linux/arm64", and "linux/armv6" for 32-bit arm. goarm is required for arm
// (a build reads it from debug.ReadBuildInfo's GOARM setting) and ignored for
// other architectures.
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

// CompareVersions orders two versions by SemVer 2.0 precedence, returning -1,
// 0 or +1 as a is older than, the same as, or newer than b. A prerelease is
// older than its release (v1.2.0-rc.1 < v1.2.0).
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range 3 {
		if c := compareNumeric(pa.core[i], pb.core[i]); c != 0 {
			return c, nil
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0, nil
	case len(pa.pre) == 0:
		return 1, nil
	case len(pb.pre) == 0:
		return -1, nil
	}
	for i := range min(len(pa.pre), len(pb.pre)) {
		if c := comparePrerelease(pa.pre[i], pb.pre[i]); c != 0 {
			return c, nil
		}
	}
	return cmp.Compare(len(pa.pre), len(pb.pre)), nil
}

// version holds a parsed version's numeric parts as digit strings, so a
// number of any length compares correctly (see compareNumeric).
type version struct {
	core [3]string
	pre  []string
}

func parseVersion(v string) (version, error) {
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return version{}, fmt.Errorf("%w: version %q is not a v-prefixed semantic version", ErrInvalid, v)
	}
	p := version{core: [3]string{m[1], m[2], m[3]}}
	if m[4] != "" {
		p.pre = strings.Split(m[4], ".")
	}
	return p, nil
}

// comparePrerelease orders two prerelease identifiers: numeric ones
// numerically, below alphanumeric ones, which compare in ASCII order.
func comparePrerelease(a, b string) int {
	numA, numB := isNumeric(a), isNumeric(b)
	switch {
	case numA && numB:
		return compareNumeric(a, b)
	case numA:
		return -1
	case numB:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// compareNumeric orders two decimal digit strings without leading zeros
// (versionPattern guarantees that) by value, at any length: the longer one is
// larger, and equal lengths compare digit by digit.
func compareNumeric(a, b string) int {
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
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
	for _, r := range m.Requires {
		if !slices.Contains(knownRequirements, r) {
			return fmt.Errorf("%w: %q", ErrUnsupportedRequirement, r)
		}
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
		b := t.Binary
		if !validBinaryPath(b.Path) {
			return fmt.Errorf("%w: target %s: binary path %q is not %s at the top level or in one directory", ErrInvalid, key, b.Path, BinaryName)
		}
		if b.Size <= 0 {
			return fmt.Errorf("%w: target %s: binary size %d", ErrInvalid, key, b.Size)
		}
		if !sha256Pattern.MatchString(b.SHA256) {
			return fmt.Errorf("%w: target %s: binary sha256 %q is not 64 lowercase hex digits", ErrInvalid, key, b.SHA256)
		}
	}
	return nil
}

// validBinaryPath reports whether p names the release executable as a clean
// tar entry: BinaryName at the top level or inside one wrapping directory,
// with no absolute, parent, backslash or drive-letter forms.
func validBinaryPath(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, `\:`) {
		return false
	}
	dir, base := path.Split(p)
	if base != BinaryName {
		return false
	}
	dir = strings.TrimSuffix(dir, "/")
	return dir == "" || dir != ".." && !strings.Contains(dir, "/")
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
// Input over MaxManifestSize or MaxSignatureSize, and a trusted key of the
// wrong length, are refused before any signature check. A manifest that
// verifies may still be an old one; see the package doc.
func Verify(manifest, sig []byte, trusted map[string]ed25519.PublicKey) (*Manifest, error) {
	if len(manifest) > MaxManifestSize {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrInvalid, len(manifest), MaxManifestSize)
	}
	if len(sig) > MaxSignatureSize {
		return nil, fmt.Errorf("%w: signature file is %d bytes, limit %d", ErrBadSignature, len(sig), MaxSignatureSize)
	}
	var s Signature
	if err := json.Unmarshal(sig, &s); err != nil {
		return nil, fmt.Errorf("%w: signature file: %w", ErrBadSignature, err)
	}
	if s.Schema != Schema {
		return nil, fmt.Errorf("%w: signature schema %d (this build reads %d)", ErrUnsupportedSchema, s.Schema, Schema)
	}
	pub, ok := trusted[s.KeyID]
	// ed25519.Verify panics on a wrong-length key; a hand-built map must not
	// turn a bad key into a crash.
	if !ok || len(pub) != ed25519.PublicKeySize {
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
