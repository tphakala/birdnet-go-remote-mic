// Package mgmtcert generates and persists the self-signed TLS certificate the
// management API serves. A LAN appliance stays zero-config: the certificate is
// created on first start, reused on later starts, and regenerated whenever the
// persisted pair is missing, unreadable, or expired.
package mgmtcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
)

// certValidity is how long a freshly generated certificate stays valid. It is
// long because a LAN appliance is rarely reconfigured and the certificate is
// pinned by trust rather than validated against a CA.
const certValidity = 10 * 365 * 24 * time.Hour

// Ensure returns a TLS certificate for the management server, reusing the
// persisted PEM pair at certPath/keyPath when both load and are within their
// validity window, and otherwise generating a new self-signed certificate
// covering hosts and writing it to those paths. A missing, unreadable, or
// expired pair is regenerated. The key file is written with owner-only
// permissions.
func Ensure(certPath, keyPath string, hosts []string) (tls.Certificate, error) {
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf := leafOf(&cert); leaf != nil && currentlyValid(leaf) && covers(leaf, hosts) {
			return cert, nil
		}
	}
	return generate(certPath, keyPath, hosts)
}

// leafOf returns cert's parsed leaf, using the cached Leaf when present and
// parsing the first DER entry otherwise. It returns nil if there is nothing to
// parse or parsing fails.
func leafOf(cert *tls.Certificate) *x509.Certificate {
	if cert.Leaf != nil {
		return cert.Leaf
	}
	if len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// currentlyValid reports whether leaf is within its validity window now.
// tls.LoadX509KeyPair verifies only that the PEM parses and the keys match, not
// that the certificate is still in date, so an expired pair would otherwise be
// served forever.
func currentlyValid(leaf *x509.Certificate) bool {
	now := time.Now()
	return now.After(leaf.NotBefore) && now.Before(leaf.NotAfter)
}

// covers reports whether leaf carries a SAN for every requested host. A cert
// persisted before the appliance's IP or hostname changed no longer covers the
// new address; regenerating on a miss avoids a permanent name mismatch that
// would otherwise require deleting the PEM files by hand.
func covers(leaf *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// generate creates a new self-signed ECDSA P-256 certificate for hosts, writes
// the PEM pair to disk, and returns the parsed keypair.
func generate(certPath, keyPath string, hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "birdnet-go-remote-mic"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certValidity),
		// ECDSA keys authenticate via ECDHE, which needs only DigitalSignature;
		// KeyEncipherment is an RSA key-transport usage and would be spurious here.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// Write both PEM files atomically (temp file + rename) so a crash mid-write
	// never leaves a half-written file or a cert and key that do not match. A
	// broken pair would still self-heal on the next start (Ensure regenerates
	// when the pair fails to load), but the rename keeps every on-disk pair
	// loadable in the first place.
	if err := atomicfile.Write(certPath, certPEM, 0o644); err != nil { //nolint:gosec // the certificate is public by design.
		return tls.Certificate{}, fmt.Errorf("write cert: %w", err)
	}
	if err := atomicfile.Write(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write key: %w", err)
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}
