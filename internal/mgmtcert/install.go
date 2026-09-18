package mgmtcert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
)

// Upper bounds on an operator-supplied PEM. A management certificate and its key
// are kilobytes; these caps reject an accidental large upload cheaply, before any
// parsing, since the management API decodes the whole request body first.
const (
	maxCertPEM = 64 * 1024
	maxKeyPEM  = 16 * 1024
)

// maxExtraSANs bounds the operator-supplied extra SAN list, matching the
// maxItems constraint in the OpenAPI contract (no request-schema middleware runs,
// so it is enforced in ValidateHosts).
const maxExtraSANs = 32

// pinExt names the sidecar marker that pins a certificate as operator-installed.
const pinExt = ".pinned"

// Validation-error field names, matching the request body fields in the OpenAPI
// contract so the UI can map a 422 onto the right input.
const (
	fieldCertPem = "certPem"
	fieldKeyPem  = "keyPem"
)

// ValidationError is a rejected certificate, key, or SAN input, naming the
// offending field so the HTTP layer can render an RFC 9457 per-field problem. It
// is a local type so mgmtcert keeps importing only atomicfile among the project's
// packages.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

// PinPath returns the marker path that pins the certificate at certPath as
// operator-installed. It replaces certPath's extension with .pinned, so
// mgmt-cert.pem yields mgmt-cert.pinned in the same directory.
func PinPath(certPath string) string {
	return strings.TrimSuffix(certPath, filepath.Ext(certPath)) + pinExt
}

// Pinned reports whether the certificate at certPath is pinned, meaning an
// operator installed it and Ensure must reuse it verbatim rather than regenerate.
// Only a definitive "does not exist" counts as unpinned: an indeterminate stat
// error (a permission or I/O fault) is treated as pinned. This branch gates
// whether Ensure may regenerate over the certificate, so it fails safe and never
// discards an operator's certificate on an ambiguous read of the marker. It uses
// Lstat so the marker's own presence is what matters (a symlink marker counts as
// present even if its target is missing), not whether a link resolves.
func Pinned(certPath string) bool {
	_, err := os.Lstat(PinPath(certPath))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// Regenerate creates a fresh self-signed certificate covering hosts, writes the
// pair to certPath/keyPath, and clears any operator pin so the appliance manages
// the certificate again. It returns the parsed keypair.
func Regenerate(certPath, keyPath string, hosts []string) (tls.Certificate, error) {
	if err := os.Remove(PinPath(certPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, fmt.Errorf("clear certificate pin: %w", err)
	}
	return generate(certPath, keyPath, hosts)
}

// Install validates an operator-supplied certificate and private key, persists
// the pair to certPath/keyPath, and writes the pin marker so Ensure never
// regenerates it. Nothing is written when validation fails. It returns the parsed
// keypair (with its Leaf populated).
func Install(certPath, keyPath string, certPEM, keyPEM []byte) (tls.Certificate, error) {
	cert, err := Validate(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	// Write the public certificate, then the private key, then the pin marker. A
	// crash after the key but before the marker leaves an unpinned custom pair that
	// the next start regenerates away; the operator re-installs. The alternative
	// (marker first) could pin a half-written pair, which is worse.
	if err := atomicfile.Write(certPath, certPEM, 0o644); err != nil { //nolint:gosec // the certificate is public by design.
		return tls.Certificate{}, fmt.Errorf("write cert: %w", err)
	}
	if err := atomicfile.Write(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write key: %w", err)
	}
	if err := atomicfile.Write(PinPath(certPath), []byte("installed\n"), 0o644); err != nil { //nolint:gosec // the pin marker carries no secret.
		return tls.Certificate{}, fmt.Errorf("write pin marker: %w", err)
	}
	return cert, nil
}

// Validate parses and checks an operator-supplied certificate and key pair
// without writing anything. Every rejection is a *ValidationError naming certPem
// or keyPem. On success it returns the parsed tls.Certificate with its Leaf
// populated. It enforces size bounds, PEM structure, an unencrypted key, the key
// matching the certificate, a currently-valid window, at least one subject
// alternative name (modern clients ignore the common name), and that the
// certificate is usable for server authentication.
func Validate(certPEM, keyPEM []byte) (tls.Certificate, error) {
	if len(certPEM) == 0 || len(certPEM) > maxCertPEM {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "must be a PEM-encoded certificate under 64 KiB"}
	}
	if len(keyPEM) == 0 || len(keyPEM) > maxKeyPEM {
		return tls.Certificate{}, &ValidationError{Field: fieldKeyPem, Reason: "must be a PEM-encoded private key under 16 KiB"}
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != pemTypeCertificate {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "must be a PEM-encoded certificate"}
	}
	// Parse the leaf up front so a malformed certificate DER is reported against
	// certPem. tls.X509KeyPair below also parses the leaf internally, but it
	// surfaces a certificate-parse failure as a generic keypair error, which would
	// otherwise be mis-attributed to keyPem and highlight the wrong field.
	leaf, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "the certificate could not be parsed: " + err.Error()}
	}
	// Find the actual private-key block, skipping any leading non-key block (a
	// comment or a stray certificate), the same way crypto/tls locates the key.
	// Inspecting only the first PEM block would falsely reject a valid key that
	// carries a leading comment, and could miss an encrypted key that follows a
	// benign first block.
	keyBlock := firstPrivateKeyBlock(keyPEM)
	if keyBlock == nil {
		return tls.Certificate{}, &ValidationError{Field: fieldKeyPem, Reason: "must be a PEM-encoded private key"}
	}
	// An encrypted key cannot be loaded without a passphrase, which this endpoint
	// does not take. PKCS#8 encryption uses the ENCRYPTED PRIVATE KEY type; legacy
	// PEM encryption sets a Proc-Type: 4,ENCRYPTED header.
	if keyBlock.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(keyBlock.Headers["Proc-Type"], "ENCRYPTED") {
		return tls.Certificate{}, &ValidationError{Field: fieldKeyPem, Reason: "encrypted private keys are not supported; supply an unencrypted PKCS#8, PKCS#1, or SEC1 key"}
	}
	// X509KeyPair parses both PEMs, confirms the key matches the leaf, and accepts
	// RSA, ECDSA, and Ed25519 keys, so it is the single source of the key-matches-
	// cert check. It also collects any chain certificates after the leaf.
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, &ValidationError{Field: fieldKeyPem, Reason: "the private key does not match the certificate: " + err.Error()}
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "the certificate is not valid until " + leaf.NotBefore.Format(time.RFC3339)}
	}
	if now.After(leaf.NotAfter) {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "the certificate expired on " + leaf.NotAfter.Format(time.RFC3339)}
	}
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "the certificate has no subject alternative names; modern clients ignore the common name and would reject it"}
	}
	// An empty ExtKeyUsage means unrestricted (usable for server auth). A non-empty
	// list must include serverAuth or anyExtendedKeyUsage, else a browser rejects
	// the handshake and the operator is locked out of the UI they just secured.
	if len(leaf.ExtKeyUsage) > 0 && !usableForServerAuth(leaf.ExtKeyUsage) {
		return tls.Certificate{}, &ValidationError{Field: fieldCertPem, Reason: "the certificate is not usable for server authentication (missing the serverAuth extended key usage)"}
	}
	cert.Leaf = leaf
	return cert, nil
}

// firstPrivateKeyBlock returns the first PEM block whose type ends with
// "PRIVATE KEY", skipping any leading non-key block (a comment or a stray
// certificate), matching how crypto/tls locates the key in a bundle. It returns
// nil when the PEM carries no private-key block.
func firstPrivateKeyBlock(pemBytes []byte) *pem.Block {
	rest := pemBytes
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return nil
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			return block
		}
		rest = remaining
	}
}

// usableForServerAuth reports whether a non-empty extended-key-usage list permits
// TLS server authentication.
func usableForServerAuth(usages []x509.ExtKeyUsage) bool {
	for _, u := range usages {
		if u == x509.ExtKeyUsageServerAuth || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// hostnameLabel matches one DNS label: a letter or digit, then up to 61 more
// letters, digits, or hyphens, ending in a letter or digit. It rejects a leading
// or trailing hyphen and, by construction, a wildcard.
var hostnameLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// ValidateHosts checks operator-supplied extra SAN entries: each must be a valid
// IP address or DNS hostname, and there must be no more than maxExtraSANs of
// them (the contract's maxItems, enforced here because no request-schema
// middleware runs). It returns a *ValidationError naming the offending entry
// (extraSans[i]) on the first bad one, so the UI can mark the field.
func ValidateHosts(extra []string) error {
	if len(extra) > maxExtraSANs {
		return &ValidationError{Field: "extraSans", Reason: fmt.Sprintf("at most %d extra subject alternative names are allowed", maxExtraSANs)}
	}
	for i, h := range extra {
		if net.ParseIP(h) != nil {
			continue
		}
		if !validHostname(h) {
			return &ValidationError{Field: fmt.Sprintf("extraSans[%d]", i), Reason: h + " is not a valid DNS name or IP address"}
		}
	}
	return nil
}

// validHostname reports whether h is a syntactically valid DNS hostname: at least
// one label, no label longer than 63 or malformed, and no more than 253 total.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// carryForward returns the union of hosts and the SAN names already on leaf, so
// regenerating an appliance-managed certificate after an address change keeps any
// operator-added extra names alongside the current auto-detected ones.
func carryForward(leaf *x509.Certificate, hosts []string) []string {
	seen := make(map[string]bool, len(hosts))
	out := make([]string, 0, len(hosts)+len(leaf.DNSNames)+len(leaf.IPAddresses))
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for _, h := range hosts {
		add(h)
	}
	for _, d := range leaf.DNSNames {
		add(d)
	}
	for _, ip := range leaf.IPAddresses {
		add(ip.String())
	}
	return out
}
