// Package mgmtcert generates and persists the TLS certificate the management API
// serves. A LAN appliance stays zero-config: a self-signed certificate is created
// on first start, reused on later starts, and regenerated whenever the persisted
// pair is missing, unreadable, expired, or no longer covers the appliance's
// names. An operator may instead install a custom certificate, which is pinned
// (a sidecar marker) so the appliance never regenerates it automatically.
package mgmtcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
)

// certValidity is how long a freshly generated certificate stays valid. It is
// long because a LAN appliance is rarely reconfigured and the certificate is
// pinned by trust rather than validated against a CA.
const certValidity = 10 * 365 * 24 * time.Hour

// pemTypeCertificate is the PEM block type for an X.509 certificate.
const pemTypeCertificate = "CERTIFICATE"

// Ensure returns a TLS certificate for the management server. When the pair at
// certPath/keyPath is pinned (an operator installed it) and loads, it is reused
// verbatim: never regenerated, even once expired, so a custom certificate is
// never silently replaced. A stale pin is dropped and the appliance self-heals
// when either pinned file is missing or the pair does not parse, but a pinned
// file that exists and cannot be read (a permission change, an I/O error, a
// symlink whose target is gone) is left untouched and Ensure returns a
// *PinnedReadError, since regenerating over it would destroy the operator's
// certificate over what may be a transient fault. An unpinned pair is reused
// when it is in date and covers every host in hosts; when it is in date but a
// name is missing (the appliance's address changed) its existing SANs are
// carried forward and it is regenerated; a missing, unreadable, or expired
// unpinned pair is regenerated from hosts. The key file is written with
// owner-only permissions.
func Ensure(certPath, keyPath string, hosts []string) (tls.Certificate, error) {
	pinned := Pinned(certPath)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if pinned {
		if err := readFault(certPath, certErr); err != nil {
			return tls.Certificate{}, err
		}
		if err := readFault(keyPath, keyErr); err != nil {
			return tls.Certificate{}, err
		}
	}
	if cert, err := parsePair(certPEM, certErr, keyPEM, keyErr); err == nil {
		if leaf := leafOf(&cert); leaf != nil {
			if pinned {
				// An operator installed this certificate. Reuse it verbatim: never
				// apply the validity or coverage checks that would regenerate it.
				return cert, nil
			}
			if currentlyValid(leaf) {
				if covers(leaf, hosts) {
					return cert, nil
				}
				// Still in date but a current name is missing (the host's IP or
				// hostname changed). Carry the existing SANs forward so operator-added
				// extras survive the address change, then regenerate.
				hosts = carryForward(leaf, hosts)
			}
		}
	}
	if pinned {
		// The pin marker is present but the pair is incomplete (a file is missing)
		// or does not parse, so it cannot be served. Drop the stale marker and
		// self-heal; generate overwrites whichever pinned file still exists.
		_ = os.Remove(PinPath(certPath))
		atomicfile.SyncDir(filepath.Dir(PinPath(certPath)))
	}
	return generate(certPath, keyPath, hosts)
}

// PinnedReadError reports that a pinned (operator-installed) certificate or key
// file exists but could not be read. Ensure returns it instead of regenerating,
// so the operator's pair survives a fault that may be transient or fixable (a
// permission change, an I/O error, a certificate volume not mounted yet). The
// appliance does not retry: the management API stays off until the process
// restarts, and a run that still has devices serving does not restart on its
// own.
type PinnedReadError struct {
	Path string
	Err  error
}

func (e *PinnedReadError) Error() string {
	return "read installed certificate or key file " + e.Path + ": " + e.Err.Error()
}

func (e *PinnedReadError) Unwrap() error { return e.Err }

// errDanglingLink is the cause a *PinnedReadError carries for a pinned file that
// is a symlink to a missing target. It deliberately does not wrap the read's
// fs.ErrNotExist, so a caller testing errors.Is(err, fs.ErrNotExist) never
// mistakes the refusal for "missing, safe to regenerate".
var errDanglingLink = errors.New("symlink target is missing")

// readFault classifies one file read of a pinned pair: nil when the read worked
// or the file itself does not exist (a missing pinned file is safe to
// self-heal), and a *PinnedReadError for any other failure. A read that fails
// with "does not exist" is confirmed with Lstat, because through a symlink it
// can mean only that the target is gone for now (a volume mounted late at boot),
// and regenerating would replace the operator's link with a regular file. Like
// Pinned, it fails safe: only Lstat itself reporting "does not exist" counts as
// missing.
func readFault(path string, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return &PinnedReadError{Path: path, Err: err}
	}
	_, lerr := os.Lstat(path)
	switch {
	case lerr == nil:
		return &PinnedReadError{Path: path, Err: errDanglingLink}
	case errors.Is(lerr, fs.ErrNotExist):
		return nil
	default:
		return &PinnedReadError{Path: path, Err: lerr}
	}
}

// parsePair parses a certificate and key read from disk, failing when either
// read failed.
func parsePair(certPEM []byte, certErr error, keyPEM []byte, keyErr error) (tls.Certificate, error) {
	if err := errors.Join(certErr, keyErr); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
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
// tls.X509KeyPair verifies only that the PEM parses and the keys match, not
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

	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// Write the pair atomically as a unit (see writePair): if staging either file
	// fails, both destinations are left untouched rather than a mismatched cert and
	// key, so a failed regeneration never corrupts the previous pair. A broken pair
	// would still self-heal on the next start (Ensure regenerates when the pair
	// fails to load, dropping any stale pin marker first).
	if err := writePair(certPath, certPEM, keyPath, keyPEM); err != nil {
		return tls.Certificate{}, err
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}
