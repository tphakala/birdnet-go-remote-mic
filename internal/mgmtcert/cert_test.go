package mgmtcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const localhost = "localhost"

func TestEnsureGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	got, err := Ensure(certPath, keyPath, []string{localhost, "127.0.0.1"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Both files must now exist.
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file not written: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file not written: %v", err)
	}

	// The returned certificate must be a usable, parseable leaf.
	if len(got.Certificate) == 0 {
		t.Fatal("returned certificate has no DER chain")
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if !leaf.NotAfter.After(time.Now()) {
		t.Errorf("certificate already expired: NotAfter=%s", leaf.NotAfter)
	}
	if err := leaf.VerifyHostname(localhost); err != nil {
		t.Errorf("hostname localhost not covered: %v", err)
	}
	if len(leaf.IPAddresses) == 0 || !leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("IP SAN 127.0.0.1 not present, got %v", leaf.IPAddresses)
	}
}

func TestEnsureKeyFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission bits do not apply on Windows")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if _, err := Ensure(filepath.Join(dir, "cert.pem"), keyPath, []string{localhost}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key permissions = %o, want 600", perm)
	}
}

func TestEnsureReusesExistingCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	first, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	fl, _ := x509.ParseCertificate(first.Certificate[0])
	sl, _ := x509.ParseCertificate(second.Certificate[0])
	if fl.SerialNumber.Cmp(sl.SerialNumber) != 0 {
		t.Errorf("certificate was regenerated: serials %s != %s", fl.SerialNumber, sl.SerialNumber)
	}
}

func TestEnsureRegeneratesWhenKeyMissing(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	first, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	second, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	fl, _ := x509.ParseCertificate(first.Certificate[0])
	sl, _ := x509.ParseCertificate(second.Certificate[0])
	if fl.SerialNumber.Cmp(sl.SerialNumber) == 0 {
		t.Error("expected regeneration after key loss, but serial was unchanged")
	}
}

func TestEnsureRegeneratesWhenCertCorrupt(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	first, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	// Corrupt the persisted certificate; the pair no longer loads.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}

	second, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	fl, _ := x509.ParseCertificate(first.Certificate[0])
	sl, _ := x509.ParseCertificate(second.Certificate[0])
	if fl.SerialNumber.Cmp(sl.SerialNumber) == 0 {
		t.Error("expected regeneration after cert corruption, but serial was unchanged")
	}
}

func TestEnsureRegeneratesWhenExpired(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// Persist a pair that loads fine but whose validity window is already past.
	expiredSerial := writeExpiredPair(t, certPath, keyPath)

	got, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if leaf.SerialNumber.Cmp(expiredSerial) == 0 {
		t.Error("expected an expired cert to be regenerated, but the serial was unchanged")
	}
	if !leaf.NotAfter.After(time.Now()) {
		t.Errorf("regenerated cert is not valid in the future: NotAfter=%s", leaf.NotAfter)
	}
}

// writeExpiredPair writes a self-signed ECDSA pair to disk whose NotAfter is in
// the past, and returns its serial number.
func writeExpiredPair(t *testing.T, certPath, keyPath string) *big.Int {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	serial := big.NewInt(42)
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "expired"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		DNSNames:     []string{localhost},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return serial
}

func TestEnsureRegeneratesWhenHostNotCovered(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	first, err := Ensure(certPath, keyPath, []string{localhost})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	// A new address the persisted cert does not cover (e.g. after a DHCP change).
	second, err := Ensure(certPath, keyPath, []string{localhost, "192.168.1.50"})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	fl, _ := x509.ParseCertificate(first.Certificate[0])
	sl, _ := x509.ParseCertificate(second.Certificate[0])
	if fl.SerialNumber.Cmp(sl.SerialNumber) == 0 {
		t.Error("expected regeneration when a requested host is not covered, but serial was unchanged")
	}
	if err := sl.VerifyHostname("192.168.1.50"); err != nil {
		t.Errorf("regenerated cert does not cover the new host: %v", err)
	}
}

func TestEnsureResultLoadsAsTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if _, err := Ensure(certPath, keyPath, []string{localhost}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// The persisted PEM pair must be a valid TLS keypair.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("persisted pair does not load as a TLS keypair: %v", err)
	}
}
