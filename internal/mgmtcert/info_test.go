package mgmtcert

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// loopbackIP is the loopback SAN the tests request and assert on.
const loopbackIP = "127.0.0.1"

// wantFingerprint independently derives the expected fingerprint from the leaf
// DER, mirroring info.go's format, so the assertion pins both the format and
// that the digest is taken over the public leaf certificate.
func wantFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func TestDescribe(t *testing.T) {
	dir := t.TempDir()
	cert, err := Ensure(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"),
		[]string{localhost, loopbackIP})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := Describe(&cert)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !strings.Contains(info.Subject, "birdnet-go-remote-mic") {
		t.Errorf("subject = %q, want it to name birdnet-go-remote-mic", info.Subject)
	}
	if info.Subject != info.Issuer {
		t.Errorf("subject %q != issuer %q for a self-signed cert", info.Subject, info.Issuer)
	}
	if !info.SelfSigned {
		t.Error("selfSigned = false, want true for the generated cert")
	}
	if !contains(info.DNSNames, localhost) {
		t.Errorf("dnsNames = %v, want it to include %q", info.DNSNames, localhost)
	}
	if !contains(info.IPAddresses, loopbackIP) {
		t.Errorf("ipAddresses = %v, want it to include %s", info.IPAddresses, loopbackIP)
	}
	if !info.NotAfter.After(info.NotBefore) {
		t.Errorf("notAfter %v not after notBefore %v", info.NotAfter, info.NotBefore)
	}
	if want := wantFingerprint(cert.Certificate[0]); info.FingerprintSHA256 != want {
		t.Errorf("fingerprint = %q, want %q (SHA-256 of the leaf DER)", info.FingerprintSHA256, want)
	}

	chainPEM, err := ChainPEM(&cert)
	if err != nil {
		t.Fatalf("ChainPEM: %v", err)
	}
	if !strings.HasPrefix(string(chainPEM), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("ChainPEM did not start with a PEM certificate header: %q", chainPEM[:min(40, len(chainPEM))])
	}
}

func TestChainPEMEncodesFullChain(t *testing.T) {
	// A chain of a leaf plus one intermediate: ChainPEM must emit BOTH blocks so an
	// operator who installed a chain downloads the whole chain back, not just the
	// leaf. Sabotage target: the loop over cert.Certificate in ChainPEM.
	leafPEM, _ := genPairPEM(t, nil)
	intPEM, _ := genPairPEM(t, nil)
	leafBlock, _ := pem.Decode(leafPEM)
	intBlock, _ := pem.Decode(intPEM)
	cert := &tls.Certificate{Certificate: [][]byte{leafBlock.Bytes, intBlock.Bytes}}
	got, err := ChainPEM(cert)
	if err != nil {
		t.Fatalf("ChainPEM: %v", err)
	}
	if n := strings.Count(string(got), "-----BEGIN CERTIFICATE-----"); n != 2 {
		t.Errorf("ChainPEM emitted %d certificate blocks, want 2 (leaf + intermediate)", n)
	}
}

func TestDescribeIPOnlyHasEmptyDNSNames(t *testing.T) {
	dir := t.TempDir()
	cert, err := Ensure(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"),
		[]string{loopbackIP})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := Describe(&cert)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	// An IP-only certificate has no DNS SANs; Describe must report an empty
	// (non-nil) slice so it serializes as [] rather than null.
	if info.DNSNames == nil {
		t.Fatal("dnsNames = nil, want an empty slice")
	}
	if len(info.DNSNames) != 0 {
		t.Errorf("dnsNames = %v, want empty", info.DNSNames)
	}
}

func TestDescribeErrorsWithoutLeaf(t *testing.T) {
	if _, err := Describe(&tls.Certificate{}); err == nil {
		t.Error("Describe on an empty certificate returned nil error")
	}
	// A nil certificate returns an error rather than panicking, matching ChainPEM.
	if _, err := Describe(nil); err == nil {
		t.Error("Describe(nil) returned nil error")
	}
}

func TestChainPEMErrors(t *testing.T) {
	if _, err := ChainPEM(&tls.Certificate{}); err == nil {
		t.Error("ChainPEM on an empty certificate returned nil error")
	}
	if _, err := ChainPEM(nil); err == nil {
		t.Error("ChainPEM(nil) returned nil error")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
