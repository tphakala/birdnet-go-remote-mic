package mgmtserver

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// CertificateInfo is the public metadata of the management listener's TLS
// certificate. It never carries private key material.
type CertificateInfo struct {
	Subject           string
	Issuer            string
	SelfSigned        bool
	DNSNames          []string
	IPAddresses       []string
	NotBefore         time.Time
	NotAfter          time.Time
	FingerprintSHA256 string
}

// CertProvider supplies the management listener's certificate metadata and the
// PEM-encoded public certificate. When no provider is mounted, both certificate
// endpoints return 501. Its methods are called from handler goroutines; the
// certificate is immutable for the process lifetime, so returning the same
// snapshot each call is safe for concurrent use.
type CertProvider interface {
	Certificate() CertificateInfo
	// CertificatePEM returns the PEM-encoded public certificate only, never the
	// private key.
	CertificatePEM() []byte
}

// WithCertificate mounts cp as the source for the certificate endpoints. Without
// it GET /system/certificate and GET /system/certificate/pem return 501.
func WithCertificate(cp CertProvider) Option {
	return func(s *Server) { s.cert = cp }
}

// GetSystemCertificate handles GET /system/certificate. Without a mounted
// provider it reports 501.
func (s *Server) GetSystemCertificate(_ context.Context, _ mgmtapi.GetSystemCertificateRequestObject) (mgmtapi.GetSystemCertificateResponseObject, error) {
	if s.cert == nil {
		return mgmtapi.GetSystemCertificatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "certificate information is not available"),
		}, nil
	}
	ci := s.cert.Certificate()
	return mgmtapi.GetSystemCertificate200JSONResponse(certificateToWire(&ci)), nil
}

// GetSystemCertificatePem handles GET /system/certificate/pem. Without a mounted
// provider it reports 501.
func (s *Server) GetSystemCertificatePem(_ context.Context, _ mgmtapi.GetSystemCertificatePemRequestObject) (mgmtapi.GetSystemCertificatePemResponseObject, error) {
	if s.cert == nil {
		return mgmtapi.GetSystemCertificatePemdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "certificate information is not available"),
		}, nil
	}
	pem := s.cert.CertificatePEM()
	return mgmtapi.GetSystemCertificatePem200ApplicationxPemFileResponse{
		Body:          bytes.NewReader(pem),
		ContentLength: int64(len(pem)),
	}, nil
}

// certificateToWire maps certificate metadata to the generated wire type. The
// SAN arrays are required by the contract, so a nil slice becomes an empty array
// rather than a null, matching systemToWire's handling of network addresses.
func certificateToWire(ci *CertificateInfo) mgmtapi.CertificateInfo {
	dnsNames := ci.DNSNames
	if dnsNames == nil {
		dnsNames = []string{}
	}
	ipAddresses := ci.IPAddresses
	if ipAddresses == nil {
		ipAddresses = []string{}
	}
	return mgmtapi.CertificateInfo{
		Subject:           ci.Subject,
		Issuer:            ci.Issuer,
		SelfSigned:        ci.SelfSigned,
		DnsNames:          dnsNames,
		IpAddresses:       ipAddresses,
		NotBefore:         ci.NotBefore,
		NotAfter:          ci.NotAfter,
		FingerprintSha256: ci.FingerprintSHA256,
	}
}
