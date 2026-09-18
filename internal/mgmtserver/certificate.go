package mgmtserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
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
	// Managed is true when the appliance generated this certificate and will
	// regenerate it on an address change or expiry; false when an operator
	// installed a custom certificate, which the appliance never auto-replaces.
	Managed bool
}

// CertProvider supplies the management listener's certificate metadata and the
// PEM-encoded public certificate. When no provider is mounted, both read
// certificate endpoints return 501. Its methods are called from handler
// goroutines concurrently with a certificate swap by an operator regenerate or
// install, so each call must return a consistent snapshot of one certificate,
// never a mix of two.
type CertProvider interface {
	Certificate() CertificateInfo
	// CertificatePEM returns the PEM-encoded public certificate only, never the
	// private key.
	CertificatePEM() []byte
}

// CertManager is a CertProvider that can also rotate the management certificate
// at runtime: regenerate a self-signed one (optionally with extra SANs), or
// install an operator-supplied certificate and key. Both persist the new pair and
// swap it into the live listener for new connections, returning the new metadata.
// When no manager is mounted, the write endpoints return 501.
type CertManager interface {
	CertProvider
	// Regenerate replaces the certificate with a fresh self-signed one covering the
	// auto-detected names plus extraSANs. It returns a *mgmtcert.ValidationError for
	// an invalid extra SAN.
	Regenerate(extraSANs []string) (CertificateInfo, error)
	// Install replaces the certificate with the operator-supplied pair after
	// validating it. It returns a *mgmtcert.ValidationError when the pair is
	// rejected, and never returns or logs the private key.
	Install(certPEM, keyPEM []byte) (CertificateInfo, error)
}

// WithCertificate mounts cp as the source for the read-only certificate
// endpoints. Without it GET /system/certificate and GET /system/certificate/pem
// return 501.
func WithCertificate(cp CertProvider) Option {
	return func(s *Server) { s.cert = cp }
}

// WithCertificateManager mounts cm as both the read source and the write manager
// for the certificate endpoints, enabling PUT /system/certificate and
// POST /system/certificate/regenerate in addition to the read endpoints.
func WithCertificateManager(cm CertManager) Option {
	return func(s *Server) {
		s.cert = cm
		s.certMgr = cm
	}
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

// PutSystemCertificate handles PUT /system/certificate: install an operator-
// supplied certificate and key. Without a mounted manager it reports 501. A
// validation failure maps to 422 naming the offending field. The private key is
// never echoed in a response, log line, or notification.
func (s *Server) PutSystemCertificate(_ context.Context, request mgmtapi.PutSystemCertificateRequestObject) (mgmtapi.PutSystemCertificateResponseObject, error) {
	if s.certMgr == nil {
		return mgmtapi.PutSystemCertificatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "installing a certificate is not available"),
		}, nil
	}
	// The body is required by the contract, so the strict server rejects an empty
	// one before this handler; guard nil defensively anyway (matching the
	// regenerate handler) so a future optional-body contract change cannot panic.
	if request.Body == nil {
		return mgmtapi.PutSystemCertificatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusBadRequest,
			Body:       problem(http.StatusBadRequest, "bad request", "a certificate and private key are required"),
		}, nil
	}
	info, err := s.certMgr.Install([]byte(request.Body.CertPem), []byte(request.Body.KeyPem))
	if err != nil {
		var verr *mgmtcert.ValidationError
		if errors.As(err, &verr) {
			return mgmtapi.PutSystemCertificate422ApplicationProblemPlusJSONResponse(certValidationProblem(verr)), nil
		}
		return mgmtapi.PutSystemCertificatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       problem(http.StatusInternalServerError, "certificate install failed", err.Error()),
		}, nil
	}
	if s.notifier != nil {
		s.notifier.Publish(notify.Notification{
			Severity: notify.SeverityInfo,
			Category: notify.CategorySystem,
			Kind:     notify.KindEvent,
			Title:    "Management certificate installed",
			Message:  "A custom certificate was installed and is live for new connections; clients must trust its fingerprint",
		})
	}
	return mgmtapi.PutSystemCertificate200JSONResponse(certificateToWire(&info)), nil
}

// RegenerateSystemCertificate handles POST /system/certificate/regenerate:
// generate a fresh self-signed certificate, optionally adding extra SANs. Without
// a mounted manager it reports 501; an invalid extra SAN maps to 422.
func (s *Server) RegenerateSystemCertificate(_ context.Context, request mgmtapi.RegenerateSystemCertificateRequestObject) (mgmtapi.RegenerateSystemCertificateResponseObject, error) {
	if s.certMgr == nil {
		return mgmtapi.RegenerateSystemCertificatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "regenerating the certificate is not available"),
		}, nil
	}
	var extras []string
	if request.Body != nil && request.Body.ExtraSans != nil {
		extras = *request.Body.ExtraSans
	}
	info, err := s.certMgr.Regenerate(extras)
	if err != nil {
		var verr *mgmtcert.ValidationError
		if errors.As(err, &verr) {
			return mgmtapi.RegenerateSystemCertificate422ApplicationProblemPlusJSONResponse(certValidationProblem(verr)), nil
		}
		return mgmtapi.RegenerateSystemCertificatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       problem(http.StatusInternalServerError, "certificate regeneration failed", err.Error()),
		}, nil
	}
	if s.notifier != nil {
		s.notifier.Publish(notify.Notification{
			Severity: notify.SeverityInfo,
			Category: notify.CategorySystem,
			Kind:     notify.KindEvent,
			Title:    "Management certificate regenerated",
			Message:  "A new self-signed certificate is live for new connections; clients that trusted the old one must trust the new fingerprint",
		})
	}
	return mgmtapi.RegenerateSystemCertificate200JSONResponse(certificateToWire(&info)), nil
}

// certValidationProblem renders a *mgmtcert.ValidationError as an RFC 9457
// ValidationProblem carrying the single offending field. It mirrors
// validationProblem (config.go) for the certificate endpoints.
func certValidationProblem(verr *mgmtcert.ValidationError) mgmtapi.ValidationProblem {
	return mgmtapi.ValidationProblem{
		Status: ptr(http.StatusUnprocessableEntity),
		Title:  ptr("invalid certificate"),
		Detail: ptr(verr.Error()),
		Errors: &[]struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		}{
			{Field: verr.Field, Reason: verr.Reason},
		},
	}
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
		Managed:           ci.Managed,
	}
}
