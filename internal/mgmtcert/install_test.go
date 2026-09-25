package mgmtcert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TEST-NET-1 address (RFC 5737), distinct from the package's loopbackIP const so
// the two never collide and this is obviously not a real host.
const testIP = "192.0.2.10"

// customExampleSAN is the SAN used for a stand-in operator-installed certificate.
const customExampleSAN = "custom.example"

// genPairPEM builds a self-signed ECDSA pair, applying mutate to the template
// before signing so a test can tune the SANs, validity window, or key usage. It
// returns the PEM-encoded certificate and PKCS#8 key.
func genPairPEM(t *testing.T, mutate func(*x509.Certificate)) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "test-install"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{localhost},
	}
	if mutate != nil {
		mutate(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// asValidationError unwraps err to a *ValidationError, failing the test when it
// is not one, and returns it so the caller can assert the field.
func asValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error %v (%T) is not a *ValidationError", err, err)
	}
	return verr
}

func TestValidateAcceptsGoodPair(t *testing.T) {
	certPEM, keyPEM := genPairPEM(t, nil)
	cert, err := Validate(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Validate rejected a good pair: %v", err)
	}
	if cert.Leaf == nil {
		t.Error("Validate did not populate cert.Leaf")
	}
}

func TestValidateRejectsMismatchedKey(t *testing.T) {
	certPEM, _ := genPairPEM(t, nil)
	_, otherKey := genPairPEM(t, nil)
	// Sabotage target: the X509KeyPair error-to-ValidationError mapping. Deleting it
	// returns the raw crypto/tls error, so errors.As for *ValidationError fails.
	verr := asValidationError(t, mustErr(Validate(certPEM, otherKey)))
	if verr.Field != fieldKeyPem {
		t.Errorf("field = %q, want keyPem", verr.Field)
	}
}

func TestValidateRejectsExpired(t *testing.T) {
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) {
		c.NotBefore = time.Now().Add(-48 * time.Hour)
		c.NotAfter = time.Now().Add(-24 * time.Hour)
	})
	// Sabotage target: the now.After(leaf.NotAfter) check.
	verr := asValidationError(t, mustErr(Validate(certPEM, keyPEM)))
	if verr.Field != fieldCertPem {
		t.Errorf("field = %q, want certPem", verr.Field)
	}
}

func TestValidateRejectsNotYetValid(t *testing.T) {
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) {
		c.NotBefore = time.Now().Add(24 * time.Hour)
		c.NotAfter = time.Now().Add(48 * time.Hour)
	})
	// Sabotage target: the now.Before(leaf.NotBefore) check (agy finding #3).
	verr := asValidationError(t, mustErr(Validate(certPEM, keyPEM)))
	if verr.Field != fieldCertPem {
		t.Errorf("field = %q, want certPem", verr.Field)
	}
}

func TestValidateRejectsNoSANs(t *testing.T) {
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) {
		c.DNSNames = nil
		c.IPAddresses = nil
	})
	// Sabotage target: the "no subject alternative names" check (agy finding #2).
	verr := asValidationError(t, mustErr(Validate(certPEM, keyPEM)))
	if verr.Field != fieldCertPem {
		t.Errorf("field = %q, want certPem", verr.Field)
	}
}

func TestValidateRejectsNonServerAuthEKU(t *testing.T) {
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	})
	// Sabotage target: the usableForServerAuth check (agy finding #5).
	verr := asValidationError(t, mustErr(Validate(certPEM, keyPEM)))
	if verr.Field != fieldCertPem {
		t.Errorf("field = %q, want certPem", verr.Field)
	}
}

func TestValidateAcceptsEmptyEKU(t *testing.T) {
	// An empty ExtKeyUsage means unrestricted, which is usable for server auth.
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) {
		c.ExtKeyUsage = nil
	})
	if _, err := Validate(certPEM, keyPEM); err != nil {
		t.Errorf("Validate rejected an empty-EKU cert: %v", err)
	}
}

func TestValidateAcceptsKeyWithLeadingBlock(t *testing.T) {
	// A key file whose first PEM block is a non-key block (a comment or a stray
	// certificate) followed by the real private key must be accepted, exactly as
	// crypto/tls locates the key. Sabotage target: firstPrivateKeyBlock (inspecting
	// only the first block would reject this valid input).
	certPEM, keyPEM := genPairPEM(t, nil)
	leading := pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: []byte("not a real cert")})
	prefixed := append(append([]byte(nil), leading...), keyPEM...)
	if _, err := Validate(certPEM, prefixed); err != nil {
		t.Errorf("Validate rejected a key with a leading non-key block: %v", err)
	}
}

func TestValidateRejectsUnparseable(t *testing.T) {
	good, goodKey := genPairPEM(t, nil)
	// A well-formed CERTIFICATE PEM wrapper around bytes that are not valid DER.
	// This must be attributed to certPem, not keyPem (X509KeyPair surfaces the
	// certificate-parse failure as a generic keypair error).
	badDERCert := pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: []byte("not valid DER")})
	encryptedKey := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("nope")})
	legacyEncrypted := pem.EncodeToMemory(&pem.Block{
		Type:    "EC PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,0"},
		Bytes:   []byte("nope"),
	})
	cases := []struct {
		name          string
		cert, key     []byte
		expectedField string
	}{
		{"garbage cert", []byte("not a pem"), goodKey, fieldCertPem},
		{"malformed cert DER", badDERCert, goodKey, fieldCertPem},
		{"garbage key", good, []byte("not a pem"), fieldKeyPem},
		{"pkcs8 encrypted key", good, encryptedKey, fieldKeyPem},
		{"legacy encrypted key", good, legacyEncrypted, fieldKeyPem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verr := asValidationError(t, mustErr(Validate(tc.cert, tc.key)))
			if verr.Field != tc.expectedField {
				t.Errorf("field = %q, want %q", verr.Field, tc.expectedField)
			}
		})
	}
}

func TestValidateRejectsOversize(t *testing.T) {
	certPEM, keyPEM := genPairPEM(t, nil)
	oversizeCert := make([]byte, maxCertPEM+1)
	if verr := asValidationError(t, mustErr(Validate(oversizeCert, keyPEM))); verr.Field != fieldCertPem {
		t.Errorf("oversize cert field = %q, want certPem", verr.Field)
	}
	bigKey := make([]byte, maxKeyPEM+1)
	if verr := asValidationError(t, mustErr(Validate(certPEM, bigKey))); verr.Field != fieldKeyPem {
		t.Errorf("oversize key field = %q, want keyPem", verr.Field)
	}
}

func TestInstallPersistsPairAndPins(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) { c.DNSNames = []string{customExampleSAN} })

	if _, err := Install(certPath, keyPath, certPEM, keyPEM); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !Pinned(certPath) {
		t.Error("Install did not write the pin marker") // sabotage target: the marker atomicfile.Write
	}
	if info, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stat key: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key perms = %o, want 600", perm)
	}
}

func TestInstallPersistsChainWithoutKeyMaterial(t *testing.T) {
	// A certPEM that (by operator mistake) also contains a PRIVATE KEY block must
	// never be written verbatim to the 0644 cert file. Install persists the
	// re-encoded chain, so the cert file carries only certificate blocks.
	// Sabotage target: writing ChainPEM(&cert) instead of the raw certPEM.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	leafPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) { c.DNSNames = []string{customExampleSAN} })
	// Simulate the operator pasting the key into the certificate field too.
	bundled := append(append([]byte(nil), leafPEM...), keyPEM...)
	if _, err := Install(certPath, keyPath, bundled, keyPEM); err != nil {
		t.Fatalf("Install: %v", err)
	}
	onDisk, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert file: %v", err)
	}
	if strings.Contains(string(onDisk), "PRIVATE KEY") {
		t.Error("cert file contains private key material; Install must persist only certificate blocks")
	}
}

func TestRegenerateKeepsOldPairOnGenerateFailure(t *testing.T) {
	// If regeneration fails while staging the new pair, the previously installed
	// pair must survive intact (both files loadable, same serial) and stay pinned,
	// so the next restart still serves it rather than discarding it. Sabotage
	// targets: writePair's atomic staging (a non-atomic write corrupts the pair)
	// and Regenerate generating before clearing the pin.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) { c.DNSNames = []string{customExampleSAN} })
	installed, err := Install(certPath, keyPath, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !Pinned(certPath) {
		t.Fatal("precondition: Install should have pinned the pair")
	}
	installedLeaf, _ := x509.ParseCertificate(installed.Certificate[0])

	// Direct the key write under certPath (a regular file), so staging the new key
	// fails and the whole regeneration errors before either file is committed.
	badKeyPath := filepath.Join(certPath, "key.pem")
	if _, err := Regenerate(certPath, badKeyPath, []string{localhost}); err == nil {
		t.Fatal("Regenerate should have failed with an unwritable key path")
	}
	if !Pinned(certPath) {
		t.Error("pin was cleared despite a regeneration failure")
	}
	// The original pinned pair must still load, unchanged.
	got, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("original pair no longer loads after a failed regeneration: %v", err)
	}
	gotLeaf, _ := x509.ParseCertificate(got.Certificate[0])
	if gotLeaf.SerialNumber.Cmp(installedLeaf.SerialNumber) != 0 {
		t.Error("certificate file was overwritten by a failed regeneration")
	}
}

func TestInstallWritesNothingOnValidationFailure(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	certPEM, _ := genPairPEM(t, nil)
	_, otherKey := genPairPEM(t, nil)

	if _, err := Install(certPath, keyPath, certPEM, otherKey); err == nil {
		t.Fatal("Install accepted a mismatched pair")
	}
	// Sabotage target: the early return after Validate fails. If Install wrote
	// before validating, these files would exist.
	if _, err := os.Stat(certPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("cert file written despite validation failure")
	}
	if Pinned(certPath) {
		t.Error("pin marker written despite validation failure")
	}
}

func TestEnsureKeepsPinnedPairNotCoveringHosts(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) { c.DNSNames = []string{customExampleSAN} })
	installed, err := Install(certPath, keyPath, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A pinned pair must be reused verbatim even for hosts it does not cover.
	// Sabotage target: the `if pinned { return cert, nil }` early return in Ensure.
	got, err := Ensure(certPath, keyPath, []string{localhost, testIP})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	il, _ := x509.ParseCertificate(installed.Certificate[0])
	gl, _ := x509.ParseCertificate(got.Certificate[0])
	if il.SerialNumber.Cmp(gl.SerialNumber) != 0 {
		t.Error("pinned certificate was regenerated despite not covering the hosts")
	}
}

func TestEnsureKeepsPinnedExpiredPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	serial := writeExpiredPair(t, certPath, keyPath)
	// Pin the expired pair as if an operator had installed it.
	if err := os.WriteFile(PinPath(certPath), []byte("installed\n"), 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}

	// Sabotage target: the pinned early return bypasses the currentlyValid check.
	got, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	gl, _ := x509.ParseCertificate(got.Certificate[0])
	if gl.SerialNumber.Cmp(serial) != 0 {
		t.Error("pinned expired certificate was regenerated; a pinned cert must never be auto-replaced")
	}
}

func TestEnsureUnpinsUnloadablePinnedPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	// A pin marker beside an unloadable certificate.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(PinPath(certPath), []byte("installed\n"), 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}

	if _, err := Ensure(certPath, keyPath, []string{localhost}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Sabotage target: the os.Remove(PinPath) in the stale-pin fallthrough.
	if Pinned(certPath) {
		t.Error("stale pin marker was not dropped after regenerating an unloadable pinned pair")
	}
}

func TestEnsureRefusesUnreadablePinnedPair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		unreadable func(certPath, keyPath string) string
	}{
		{"cert", func(c, _ string) string { return c }},
		{"key", func(_, k string) string { return k }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			certPath := filepath.Join(dir, "mgmt-cert.pem")
			keyPath := filepath.Join(dir, "mgmt-key.pem")
			certPEM, keyPEM := genPairPEM(t, nil)
			if _, err := Install(certPath, keyPath, certPEM, keyPEM); err != nil {
				t.Fatalf("Install: %v", err)
			}
			// Replace one file with a directory: reading it fails with EISDIR, an
			// I/O fault that is neither "missing" nor "corrupt", and unlike a
			// chmod 000 it also fails when the test runs as root.
			bad := tc.unreadable(certPath, keyPath)
			good := certPath
			if bad == certPath {
				good = keyPath
			}
			goodBefore, err := os.ReadFile(good)
			if err != nil {
				t.Fatalf("read %s: %v", good, err)
			}
			if err := os.Remove(bad); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if err := os.Mkdir(bad, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			_, err = Ensure(certPath, keyPath, []string{localhost})
			// Sabotage target: the readFault checks in Ensure. Without them the
			// read error falls through to generate, which overwrites the pair.
			pe, ok := errors.AsType[*PinnedReadError](err)
			if !ok {
				t.Fatalf("Ensure error = %v, want a *PinnedReadError", err)
			}
			if pe.Path != bad {
				t.Errorf("PinnedReadError.Path = %q, want %q", pe.Path, bad)
			}
			// The operator reads the cause and the file in the log line.
			if !errors.Is(err, syscall.EISDIR) {
				t.Errorf("error %v does not unwrap to EISDIR", err)
			}
			if !strings.Contains(err.Error(), bad) {
				t.Errorf("error %q does not name %s", err, bad)
			}
			if !Pinned(certPath) {
				t.Error("pin marker dropped over an unreadable pinned pair")
			}
			if fi, err := os.Stat(bad); err != nil || !fi.IsDir() {
				t.Errorf("unreadable pinned file was replaced (stat: %v)", err)
			}
			if after, err := os.ReadFile(good); err != nil || !bytes.Equal(after, goodBefore) {
				t.Errorf("readable pinned file %s changed (read error: %v)", good, err)
			}
		})
	}
}

func TestEnsureRefusesDanglingPinnedSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vol := filepath.Join(dir, "vol") // stands in for a volume mounted late at boot
	if err := os.Mkdir(vol, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	// The targets must exist before Install, which resolves the links and writes
	// through them (a link that already dangles would be replaced).
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(vol, name), nil, 0o600); err != nil {
			t.Fatalf("seed target: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join(vol, "cert.pem"), certPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(vol, "key.pem"), keyPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	certPEM, keyPEM := genPairPEM(t, nil)
	if _, err := Install(certPath, keyPath, certPEM, keyPEM); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Unmount the volume: both links now dangle.
	if err := os.Rename(vol, vol+".away"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	_, err := Ensure(certPath, keyPath, []string{localhost})
	// Sabotage target: the Lstat step in readFault. Without it the dangling
	// link reads as "missing", the pin is dropped, and generate replaces the
	// link with a regular file.
	pe, ok := errors.AsType[*PinnedReadError](err)
	if !ok {
		t.Fatalf("Ensure error = %v, want a *PinnedReadError", err)
	}
	if !errors.Is(pe.Err, errDanglingLink) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("cause = %v, want errDanglingLink and not fs.ErrNotExist", pe.Err)
	}
	if !Pinned(certPath) {
		t.Error("pin marker dropped over a dangling pinned link")
	}
	for _, p := range []string{certPath, keyPath} {
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s is no longer a symlink (lstat: %v)", p, err)
		}
	}
}

func TestEnsureRegeneratesDanglingUnpinnedLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	if _, err := Ensure(certPath, keyPath, []string{localhost}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	// Point the appliance's own (unpinned) cert at a missing target: the refusal
	// is only for an operator's pinned pair, so this must still self-heal.
	if err := os.Remove(certPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone.pem"), certPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Sabotage target: the `if pinned` scoping around readFault in Ensure.
	if _, err := Ensure(certPath, keyPath, []string{localhost}); err != nil {
		t.Fatalf("Ensure over an unreadable unpinned pair: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("regenerated pair does not load: %v", err)
	}
}

func TestEnsureSelfHealsMissingPinnedPair(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	if err := os.WriteFile(PinPath(certPath), []byte("installed\n"), 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}

	// A missing file is not a read fault: there is nothing to preserve.
	if _, err := Ensure(certPath, keyPath, []string{localhost}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if Pinned(certPath) {
		t.Error("stale pin marker kept for a missing pinned pair")
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("certificate not regenerated: %v", err)
	}
}

func TestEnsureCarriesForwardSANsOnDrift(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	if _, err := Ensure(certPath, keyPath, []string{localhost, "mic.example.org"}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	// The address changed: a new host that the persisted cert does not cover.
	got, err := Ensure(certPath, keyPath, []string{localhost, testIP})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	gl, _ := x509.ParseCertificate(got.Certificate[0])
	// Sabotage target: hosts = carryForward(leaf, hosts). Without it the operator's
	// original name is dropped when regenerating for the new address.
	if err := gl.VerifyHostname("mic.example.org"); err != nil {
		t.Errorf("carried-forward SAN missing: %v", err)
	}
	if err := gl.VerifyHostname(testIP); err != nil {
		t.Errorf("new host SAN missing: %v", err)
	}
}

func TestRegenerateUnpinsAndCoversHosts(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "mgmt-cert.pem")
	keyPath := filepath.Join(dir, "mgmt-key.pem")
	certPEM, keyPEM := genPairPEM(t, func(c *x509.Certificate) { c.DNSNames = []string{customExampleSAN} })
	if _, err := Install(certPath, keyPath, certPEM, keyPEM); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, err := Regenerate(certPath, keyPath, []string{localhost, testIP})
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	// Sabotage target: the os.Remove(PinPath) in Regenerate.
	if Pinned(certPath) {
		t.Error("Regenerate did not clear the pin marker")
	}
	gl, _ := x509.ParseCertificate(got.Certificate[0])
	if err := gl.VerifyHostname(testIP); err != nil {
		t.Errorf("regenerated cert does not cover the requested host: %v", err)
	}
}

func TestValidateHosts(t *testing.T) {
	cases := []struct {
		name    string
		hosts   []string
		wantErr bool
	}{
		{"ipv4", []string{"10.1.2.3"}, false},
		{"ipv6", []string{"2001:db8::1"}, false},
		{"hostname", []string{"mic.example.org"}, false},
		{"dotlocal", []string{"birdmic.local"}, false},
		{"empty entry", []string{""}, true},
		{"wildcard", []string{"*.example.org"}, true},
		{"leading hyphen", []string{"-bad.example"}, true},
		{"overlong", []string{makeLongName()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHosts(tc.hosts)
			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.wantErr && err != nil {
				if verr := asValidationError(t, err); verr.Field != "extraSans[0]" {
					t.Errorf("field = %q, want extraSans[0]", verr.Field)
				}
			}
		})
	}
}

func TestValidateHostsRejectsTooMany(t *testing.T) {
	// The contract caps extraSans at maxExtraSANs; ValidateHosts enforces it since
	// no request-schema middleware runs. Sabotage target: the len(extra) check.
	many := make([]string, maxExtraSANs+1)
	for i := range many {
		many[i] = "host.example"
	}
	err := ValidateHosts(many)
	if err == nil {
		t.Fatal("ValidateHosts accepted more than the allowed number of SANs")
	}
	if verr := asValidationError(t, err); verr.Field != "extraSans" {
		t.Errorf("field = %q, want extraSans", verr.Field)
	}
}

// mustErr returns err from a (value, error) pair, discarding the value, so a
// one-liner can assert on the error alone.
func mustErr[T any](_ T, err error) error { return err }

func makeLongName() string {
	// 254 characters, over the 253-byte hostname limit.
	b := make([]byte, 254)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// TestUnpinReportsRemoveError pins that a pin marker that cannot be removed
// fails unpin, so Regenerate does not report a certificate as managed while
// the marker still pins it. A non-empty directory in the marker's place makes
// the remove fail as a real permission or I/O error would.
func TestUnpinReportsRemoveError(t *testing.T) {
	t.Parallel()
	certPath := filepath.Join(t.TempDir(), "mgmt-cert.pem")
	marker := PinPath(certPath)
	if err := os.MkdirAll(filepath.Join(marker, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unpin(certPath); err == nil {
		t.Fatal("unpin succeeded although the marker could not be removed")
	}
	if err := os.RemoveAll(marker); err != nil {
		t.Fatal(err)
	}
	if err := unpin(certPath); err != nil {
		t.Errorf("unpin with no marker: got %v, want nil", err)
	}
}
