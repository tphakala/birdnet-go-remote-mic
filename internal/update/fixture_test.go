package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// Values shared across the tests.
const (
	vOld          = "v0.2.0"
	vNew          = "v0.3.0"
	vDescribe     = "v0.2.0-41-gf26d800"
	vRC           = "v1.0.0-rc.1"
	testTarget    = "linux/arm64"
	wantUntrusted = "untrusted key"
	wantNotNewer  = "not newer"
	wantNoTarget  = "no build for this platform"
	debBin        = "/usr/bin/remote-mic"
	serviceBin    = "/usr/local/bin/remote-mic"
)

// testKeys returns a fresh signing key and the trusted-key map that accepts it.
func testKeys(t *testing.T) (priv ed25519.PrivateKey, trusted map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]ed25519.PublicKey{releasemanifest.KeyID(pub): pub}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// tarGz builds a gzip-compressed tar holding one regular file per entry.
func tarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// release is a signed test release: a tarball holding bin for target, and the
// manifest and signature describing it.
type release struct {
	version  string
	target   string
	bin      []byte
	tarball  []byte
	manifest *releasemanifest.Manifest
	raw, sig []byte
}

// newRelease builds and signs a release of version whose target tarball is
// served at tarURL.
func newRelease(t *testing.T, priv ed25519.PrivateKey, version, target, tarURL string, bin []byte) *release {
	t.Helper()
	tb := tarGz(t, map[string][]byte{releasemanifest.BinaryName: bin})
	m := &releasemanifest.Manifest{
		Schema:   releasemanifest.Schema,
		Version:  version,
		Date:     time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		NotesURL: "https://github.com/" + releasemanifest.Repository + "/releases/tag/" + version,
		Targets: map[string]releasemanifest.Target{
			target: {
				URL:    tarURL,
				Size:   int64(len(tb)),
				SHA256: sha(tb),
				Binary: releasemanifest.Binary{Path: releasemanifest.BinaryName, Size: int64(len(bin)), SHA256: sha(bin)},
			},
		},
	}
	raw, err := releasemanifest.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := releasemanifest.Sign(priv, raw)
	if err != nil {
		t.Fatal(err)
	}
	return &release{version: version, target: target, bin: bin, tarball: tb, manifest: m, raw: raw, sig: sig}
}
