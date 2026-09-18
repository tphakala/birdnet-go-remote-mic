package mgmtserver

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// certSubjectDN is the distinguished name the generated self-signed certificate
// carries; the tests reuse it as both subject and issuer for a self-signed cert.
const certSubjectDN = "CN=birdnet-go-remote-mic"

type fakeCert struct {
	ci  CertificateInfo
	pem []byte
}

func (f *fakeCert) Certificate() CertificateInfo { return f.ci }
func (f *fakeCert) CertificatePEM() []byte       { return f.pem }

func TestGetSystemCertificateNotImplementedWithoutProvider(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.GetSystemCertificate(context.Background(), mgmtapi.GetSystemCertificateRequestObject{})
	if err != nil {
		t.Fatalf("GetSystemCertificate: %v", err)
	}
	p, ok := resp.(mgmtapi.GetSystemCertificatedefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("GetSystemCertificate returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestGetSystemCertificateMapsFields(t *testing.T) {
	notBefore := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	// DNSNames nil exercises the nil-DNS normalization branch of certificateToWire;
	// the nil-IP branch is covered by TestGetSystemCertificateNormalizesNilIPAddresses.
	s := New(&fakeProvider{}, WithCertificate(&fakeCert{ci: CertificateInfo{
		Subject:           certSubjectDN,
		Issuer:            certSubjectDN,
		SelfSigned:        true,
		DNSNames:          nil,
		IPAddresses:       []string{"127.0.0.1"},
		NotBefore:         notBefore,
		NotAfter:          notAfter,
		FingerprintSHA256: "AB:CD:EF",
	}}))
	resp, err := s.GetSystemCertificate(context.Background(), mgmtapi.GetSystemCertificateRequestObject{})
	if err != nil {
		t.Fatalf("GetSystemCertificate: %v", err)
	}
	got, ok := resp.(mgmtapi.GetSystemCertificate200JSONResponse)
	if !ok {
		t.Fatalf("GetSystemCertificate returned %T, want 200", resp)
	}
	if got.Subject != certSubjectDN || got.Issuer != certSubjectDN || !got.SelfSigned {
		t.Errorf("scalar fields wrong: %+v", got)
	}
	if got.FingerprintSha256 != "AB:CD:EF" {
		t.Errorf("fingerprint = %q", got.FingerprintSha256)
	}
	if !got.NotBefore.Equal(notBefore) || !got.NotAfter.Equal(notAfter) {
		t.Errorf("validity window wrong: %v .. %v", got.NotBefore, got.NotAfter)
	}
	// A nil DNS slice must serialize as an empty array, not null (required field).
	if got.DnsNames == nil {
		t.Error("dnsNames = nil, want empty slice")
	}
	if len(got.DnsNames) != 0 {
		t.Errorf("dnsNames = %v, want empty", got.DnsNames)
	}
	if len(got.IpAddresses) != 1 || got.IpAddresses[0] != "127.0.0.1" {
		t.Errorf("ipAddresses = %v", got.IpAddresses)
	}
}

func TestGetSystemCertificateNormalizesNilIPAddresses(t *testing.T) {
	// The mirror of the maps-fields case: DNSNames non-nil, IPAddresses nil, so
	// the nil-IP normalization branch of certificateToWire is exercised too.
	s := New(&fakeProvider{}, WithCertificate(&fakeCert{ci: CertificateInfo{
		Subject:     certSubjectDN,
		Issuer:      "CN=example-ca",
		SelfSigned:  false,
		DNSNames:    []string{"birdmic.local"},
		IPAddresses: nil,
	}}))
	resp, err := s.GetSystemCertificate(context.Background(), mgmtapi.GetSystemCertificateRequestObject{})
	if err != nil {
		t.Fatalf("GetSystemCertificate: %v", err)
	}
	got, ok := resp.(mgmtapi.GetSystemCertificate200JSONResponse)
	if !ok {
		t.Fatalf("GetSystemCertificate returned %T, want 200", resp)
	}
	// Subject and Issuer are distinct here (unlike the self-signed maps-fields
	// test), so this pins that certificateToWire does not transpose the two.
	if got.Subject != certSubjectDN {
		t.Errorf("subject = %q, want %q", got.Subject, certSubjectDN)
	}
	if got.Issuer != "CN=example-ca" {
		t.Errorf("issuer = %q, want CN=example-ca", got.Issuer)
	}
	if got.SelfSigned {
		t.Error("selfSigned = true, want false for a CA-issued cert")
	}
	if got.IpAddresses == nil {
		t.Error("ipAddresses = nil, want empty slice")
	}
	if len(got.IpAddresses) != 0 {
		t.Errorf("ipAddresses = %v, want empty", got.IpAddresses)
	}
	if len(got.DnsNames) != 1 || got.DnsNames[0] != "birdmic.local" {
		t.Errorf("dnsNames = %v", got.DnsNames)
	}
}

func TestGetSystemCertificatePemNotImplementedWithoutProvider(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.GetSystemCertificatePem(context.Background(), mgmtapi.GetSystemCertificatePemRequestObject{})
	if err != nil {
		t.Fatalf("GetSystemCertificatePem: %v", err)
	}
	p, ok := resp.(mgmtapi.GetSystemCertificatePemdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("GetSystemCertificatePem returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestGetSystemCertificatePemReturnsPEM(t *testing.T) {
	pemBody := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	s := New(&fakeProvider{}, WithCertificate(&fakeCert{pem: pemBody}))
	resp, err := s.GetSystemCertificatePem(context.Background(), mgmtapi.GetSystemCertificatePemRequestObject{})
	if err != nil {
		t.Fatalf("GetSystemCertificatePem: %v", err)
	}
	got, ok := resp.(mgmtapi.GetSystemCertificatePem200ApplicationxPemFileResponse)
	if !ok {
		t.Fatalf("GetSystemCertificatePem returned %T, want 200 PEM", resp)
	}
	if got.ContentLength != int64(len(pemBody)) {
		t.Errorf("contentLength = %d, want %d", got.ContentLength, len(pemBody))
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, pemBody) {
		t.Errorf("body = %q, want %q", body, pemBody)
	}
}
