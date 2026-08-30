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
	"os"
	"time"
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
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && currentlyValid(&cert) {
		return cert, nil
	}
	return generate(certPath, keyPath, hosts)
}

// currentlyValid reports whether cert's leaf is within its validity window now.
// tls.LoadX509KeyPair verifies only that the PEM parses and the keys match, not
// that the certificate is still in date, so an expired pair would otherwise be
// served forever.
func currentlyValid(cert *tls.Certificate) bool {
	leaf := cert.Leaf
	if leaf == nil {
		if len(cert.Certificate) == 0 {
			return false
		}
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return false
		}
		leaf = parsed
	}
	now := time.Now()
	return now.After(leaf.NotBefore) && now.Before(leaf.NotAfter)
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

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec // the certificate is public by design.
		return tls.Certificate{}, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write key: %w", err)
	}
	// os.WriteFile only applies the mode when creating the file; enforce
	// owner-only permissions even when overwriting a pre-existing wider-mode key.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("chmod key: %w", err)
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}
