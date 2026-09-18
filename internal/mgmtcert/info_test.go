package mgmtcert

import (
	"crypto/tls"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fingerprintPattern matches SHA-256 rendered as 32 colon-separated uppercase
// hex pairs (31 colons, 95 characters total).
var fingerprintPattern = regexp.MustCompile("^([0-9A-F]{2}:){31}[0-9A-F]{2}$")

// loopbackIP is the loopback SAN the tests request and assert on.
const loopbackIP = "127.0.0.1"

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
	if !fingerprintPattern.MatchString(info.FingerprintSHA256) {
		t.Errorf("fingerprint = %q, want colon-separated uppercase hex", info.FingerprintSHA256)
	}

	pem, err := LeafPEM(&cert)
	if err != nil {
		t.Fatalf("LeafPEM: %v", err)
	}
	if !strings.HasPrefix(string(pem), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("LeafPEM did not start with a PEM certificate header: %q", pem[:min(40, len(pem))])
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
	// A nil certificate returns an error rather than panicking, matching LeafPEM.
	if _, err := Describe(nil); err == nil {
		t.Error("Describe(nil) returned nil error")
	}
}

func TestLeafPEMErrors(t *testing.T) {
	if _, err := LeafPEM(&tls.Certificate{}); err == nil {
		t.Error("LeafPEM on an empty certificate returned nil error")
	}
	if _, err := LeafPEM(nil); err == nil {
		t.Error("LeafPEM(nil) returned nil error")
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
