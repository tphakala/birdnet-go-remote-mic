package mgmtcert

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Info is the public metadata of a TLS certificate, safe to expose to an
// operator. It never carries private key material.
type Info struct {
	Subject           string
	Issuer            string
	SelfSigned        bool
	DNSNames          []string
	IPAddresses       []string
	NotBefore         time.Time
	NotAfter          time.Time
	FingerprintSHA256 string
}

// Describe extracts the public metadata of cert's leaf. It returns an error when
// cert has no parseable leaf. SelfSigned is judged by subject-equals-issuer
// rather than a signature check: the generated leaf sets only
// KeyUsageDigitalSignature (no CertSign), so a signature-based self-check would
// spuriously fail on a genuinely self-signed leaf, while subject/issuer equality
// still distinguishes a future operator-supplied CA-signed certificate.
func Describe(cert *tls.Certificate) (Info, error) {
	if cert == nil {
		return Info{}, errors.New("mgmtcert: nil certificate")
	}
	leaf := leafOf(cert)
	if leaf == nil {
		return Info{}, errors.New("mgmtcert: certificate has no parseable leaf")
	}
	ips := make([]string, 0, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	// ips is preallocated (make) so it is already non-nil; normalize the DNS SANs
	// the same way so an IP-only certificate reports [] rather than null on the wire.
	dnsNames := append([]string{}, leaf.DNSNames...)
	return Info{
		Subject:           leaf.Subject.String(),
		Issuer:            leaf.Issuer.String(),
		SelfSigned:        leaf.Subject.String() == leaf.Issuer.String(),
		DNSNames:          dnsNames,
		IPAddresses:       ips,
		NotBefore:         leaf.NotBefore,
		NotAfter:          leaf.NotAfter,
		FingerprintSHA256: fingerprint(leaf.Raw),
	}, nil
}

// ChainPEM returns the PEM-encoded public certificate chain: the leaf first,
// followed by any intermediate certificates the pair carries, in order. It
// returns an error when cert has no DER body. It encodes public certificates
// only and never touches the private key, so an operator who installed a
// leaf-plus-intermediates chain downloads the same chain back rather than a bare
// leaf. For the appliance's own self-signed certificate the chain is a single
// leaf, so the output is identical to encoding the leaf alone.
func ChainPEM(cert *tls.Certificate) ([]byte, error) {
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, errors.New("mgmtcert: certificate has no DER body")
	}
	var buf bytes.Buffer
	for _, der := range cert.Certificate {
		if err := pem.Encode(&buf, &pem.Block{Type: pemTypeCertificate, Bytes: der}); err != nil {
			return nil, fmt.Errorf("mgmtcert: encode certificate chain: %w", err)
		}
	}
	return buf.Bytes(), nil
}

// fingerprint returns the SHA-256 digest of der as colon-separated uppercase
// hex (for example "AB:CD:EF"), the form an operator compares against a client's
// certificate viewer.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	b.Grow(len(sum)*3 - 1)
	for i, by := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", by)
	}
	return b.String()
}
