package mgmtserver

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// Shared test literals for the certificate write handlers.
const (
	testCertPEM = "cert"
	testKeyPEM  = "key"
	testFP      = "AB:CD"
)

// fakeCertManager is a CertManager test double: it records what it was asked to
// install or regenerate and returns a configurable result or error.
type fakeCertManager struct {
	fakeCert
	installErr    error
	regenErr      error
	installedCert []byte
	installedKey  []byte
	gotExtras     []string
	gotRegen      bool
	result        CertificateInfo
}

func (f *fakeCertManager) Install(certPEM, keyPEM []byte) (CertificateInfo, error) {
	f.installedCert = certPEM
	f.installedKey = keyPEM
	if f.installErr != nil {
		return CertificateInfo{}, f.installErr
	}
	return f.result, nil
}

func (f *fakeCertManager) Regenerate(extraSANs []string) (CertificateInfo, error) {
	f.gotRegen = true
	f.gotExtras = extraSANs
	if f.regenErr != nil {
		return CertificateInfo{}, f.regenErr
	}
	return f.result, nil
}

// recordingNotifier captures published notifications for assertions.
type recordingNotifier struct{ published []notify.Notification }

// The Notification value parameters below are fixed by the notify.Publisher
// interface, so gocritic's hugeParam suggestion to take a pointer does not apply.
func (r *recordingNotifier) Publish(n notify.Notification)              { r.published = append(r.published, n) } //nolint:gocritic // interface-mandated value param
func (r *recordingNotifier) Onset(_ notify.Notification) bool           { return false }                         //nolint:gocritic // interface-mandated value param
func (r *recordingNotifier) Clear(_ string, _ notify.Notification) bool { return false }                         //nolint:gocritic // interface-mandated value param
func (r *recordingNotifier) Resolve(_, _ string) bool                   { return false }

func TestPutSystemCertificateNotImplementedWithoutManager(t *testing.T) {
	// A read-only provider (no manager) must reject the write with 501.
	s := New(&fakeProvider{}, WithCertificate(&fakeCert{}))
	resp, err := s.PutSystemCertificate(context.Background(), mgmtapi.PutSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateInstallRequest{CertPem: "x", KeyPem: "y"},
	})
	if err != nil {
		t.Fatalf("PutSystemCertificate: %v", err)
	}
	p, ok := resp.(mgmtapi.PutSystemCertificatedefaultApplicationProblemPlusJSONResponse)
	if !ok || p.StatusCode != 501 {
		t.Fatalf("resp = %T (status %v), want default 501", resp, ok)
	}
}

func TestRegenerateSystemCertificateNotImplementedWithoutManager(t *testing.T) {
	s := New(&fakeProvider{}, WithCertificate(&fakeCert{}))
	resp, err := s.RegenerateSystemCertificate(context.Background(), mgmtapi.RegenerateSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateRegenerateRequest{},
	})
	if err != nil {
		t.Fatalf("RegenerateSystemCertificate: %v", err)
	}
	p, ok := resp.(mgmtapi.RegenerateSystemCertificatedefaultApplicationProblemPlusJSONResponse)
	if !ok || p.StatusCode != 501 {
		t.Fatalf("resp = %T (status %v), want default 501", resp, ok)
	}
}

func TestPutSystemCertificateMapsValidationErrorTo422(t *testing.T) {
	mgr := &fakeCertManager{installErr: &mgmtcert.ValidationError{Field: "keyPem", Reason: "does not match"}}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.PutSystemCertificate(context.Background(), mgmtapi.PutSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateInstallRequest{CertPem: testCertPEM, KeyPem: testKeyPEM},
	})
	if err != nil {
		t.Fatalf("PutSystemCertificate: %v", err)
	}
	// Sabotage target: the errors.As(*mgmtcert.ValidationError) branch. Without it
	// the error falls through to a 500 default response, not this 422.
	vp, ok := resp.(mgmtapi.PutSystemCertificate422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("resp = %T, want 422", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != "keyPem" {
		t.Errorf("422 errors = %+v, want one field keyPem", vp.Errors)
	}
}

func TestPutSystemCertificateMapsGenericErrorTo500(t *testing.T) {
	// A non-ValidationError from Install must map to 500, not 422.
	// Sabotage target: the errors.As branch (deleting it would return this error
	// through the 422 path or panic on the type assertion).
	mgr := &fakeCertManager{installErr: errors.New("disk full")}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.PutSystemCertificate(context.Background(), mgmtapi.PutSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateInstallRequest{CertPem: testCertPEM, KeyPem: testKeyPEM},
	})
	if err != nil {
		t.Fatalf("PutSystemCertificate: %v", err)
	}
	p, ok := resp.(mgmtapi.PutSystemCertificatedefaultApplicationProblemPlusJSONResponse)
	if !ok || p.StatusCode != 500 {
		t.Fatalf("resp = %T (status %v), want default 500", resp, ok)
	}
}

func TestRegenerateSystemCertificateMapsGenericErrorTo500(t *testing.T) {
	mgr := &fakeCertManager{regenErr: errors.New("disk full")}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.RegenerateSystemCertificate(context.Background(), mgmtapi.RegenerateSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateRegenerateRequest{},
	})
	if err != nil {
		t.Fatalf("RegenerateSystemCertificate: %v", err)
	}
	p, ok := resp.(mgmtapi.RegenerateSystemCertificatedefaultApplicationProblemPlusJSONResponse)
	if !ok || p.StatusCode != 500 {
		t.Fatalf("resp = %T (status %v), want default 500", resp, ok)
	}
}

func TestRegenerateSystemCertificatePublishes(t *testing.T) {
	// The regenerate path must publish a system notification, mirroring install.
	// Sabotage target: the s.notifier.Publish call in RegenerateSystemCertificate.
	mgr := &fakeCertManager{result: CertificateInfo{Managed: true}}
	notifier := &recordingNotifier{}
	s := New(&fakeProvider{}, WithCertificateManager(mgr), WithNotifier(notifier))
	if _, err := s.RegenerateSystemCertificate(context.Background(), mgmtapi.RegenerateSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateRegenerateRequest{},
	}); err != nil {
		t.Fatalf("RegenerateSystemCertificate: %v", err)
	}
	if len(notifier.published) != 1 {
		t.Fatalf("published %d notifications, want 1", len(notifier.published))
	}
	if notifier.published[0].Category != notify.CategorySystem {
		t.Errorf("category = %q, want system", notifier.published[0].Category)
	}
}

func TestPutSystemCertificateRejectsNilBody(t *testing.T) {
	// A nil body (defensive guard) must be a 400 and must not reach Install.
	// Sabotage target: the request.Body == nil guard.
	mgr := &fakeCertManager{}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.PutSystemCertificate(context.Background(), mgmtapi.PutSystemCertificateRequestObject{Body: nil})
	if err != nil {
		t.Fatalf("PutSystemCertificate: %v", err)
	}
	p, ok := resp.(mgmtapi.PutSystemCertificatedefaultApplicationProblemPlusJSONResponse)
	if !ok || p.StatusCode != 400 {
		t.Fatalf("resp = %T (status %v), want default 400", resp, ok)
	}
	if mgr.installedCert != nil {
		t.Error("Install was called despite a nil body")
	}
}

func TestRegenerateSystemCertificateBadSANYields422(t *testing.T) {
	mgr := &fakeCertManager{regenErr: &mgmtcert.ValidationError{Field: "extraSans[0]", Reason: "bad"}}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.RegenerateSystemCertificate(context.Background(), mgmtapi.RegenerateSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateRegenerateRequest{ExtraSans: &[]string{"*.bad"}},
	})
	if err != nil {
		t.Fatalf("RegenerateSystemCertificate: %v", err)
	}
	vp, ok := resp.(mgmtapi.RegenerateSystemCertificate422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("resp = %T, want 422", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != "extraSans[0]" {
		t.Errorf("422 errors = %+v, want extraSans[0]", vp.Errors)
	}
}

func TestPutSystemCertificateReturnsInfoAndPublishes(t *testing.T) {
	mgr := &fakeCertManager{result: CertificateInfo{Subject: certSubjectDN, FingerprintSHA256: testFP, Managed: false}}
	notifier := &recordingNotifier{}
	s := New(&fakeProvider{}, WithCertificateManager(mgr), WithNotifier(notifier))
	resp, err := s.PutSystemCertificate(context.Background(), mgmtapi.PutSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateInstallRequest{CertPem: testCertPEM, KeyPem: testKeyPEM},
	})
	if err != nil {
		t.Fatalf("PutSystemCertificate: %v", err)
	}
	got, ok := resp.(mgmtapi.PutSystemCertificate200JSONResponse)
	if !ok {
		t.Fatalf("resp = %T, want 200", resp)
	}
	if got.FingerprintSha256 != testFP || got.Managed {
		t.Errorf("200 body = %+v, want fingerprint AB:CD and managed=false", got)
	}
	// Sabotage target: the s.notifier.Publish call.
	if len(notifier.published) != 1 {
		t.Fatalf("published %d notifications, want 1", len(notifier.published))
	}
	if notifier.published[0].Category != notify.CategorySystem {
		t.Errorf("category = %q, want system", notifier.published[0].Category)
	}
}

func TestRegenerateSystemCertificateForwardsExtraSans(t *testing.T) {
	mgr := &fakeCertManager{result: CertificateInfo{Managed: true}}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	extras := []string{"a.example", "10.0.0.9"}
	if _, err := s.RegenerateSystemCertificate(context.Background(), mgmtapi.RegenerateSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateRegenerateRequest{ExtraSans: &extras},
	}); err != nil {
		t.Fatalf("RegenerateSystemCertificate: %v", err)
	}
	// Sabotage target: the `extras = *request.Body.ExtraSans` forwarding line.
	if len(mgr.gotExtras) != 2 || mgr.gotExtras[0] != "a.example" || mgr.gotExtras[1] != "10.0.0.9" {
		t.Errorf("forwarded extras = %v, want [a.example 10.0.0.9]", mgr.gotExtras)
	}
}

func TestRegenerateSystemCertificateAcceptsNilBody(t *testing.T) {
	// The strict server rejects an empty body before the handler, but a JSON null
	// body reaches the handler with a nil Body pointer; treat it as no extras.
	mgr := &fakeCertManager{result: CertificateInfo{Managed: true}}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.RegenerateSystemCertificate(context.Background(), mgmtapi.RegenerateSystemCertificateRequestObject{Body: nil})
	if err != nil {
		t.Fatalf("RegenerateSystemCertificate: %v", err)
	}
	if _, ok := resp.(mgmtapi.RegenerateSystemCertificate200JSONResponse); !ok {
		t.Fatalf("resp = %T, want 200", resp)
	}
	if !mgr.gotRegen || mgr.gotExtras != nil {
		t.Errorf("regen=%v extras=%v, want regen with nil extras", mgr.gotRegen, mgr.gotExtras)
	}
}

func TestPutSystemCertificateNeverEchoesKey(t *testing.T) {
	// A contract guard: the 200 response carries CertificateInfo, which has no key
	// field, so a rendered success body must never contain the private key. There
	// is no single line to delete; sabotage by adding the key to the response type.
	const secretKey = "-----BEGIN PRIVATE KEY-----\nSUPERSECRETKEYMATERIAL\n-----END PRIVATE KEY-----\n"
	mgr := &fakeCertManager{result: CertificateInfo{Subject: certSubjectDN, FingerprintSHA256: testFP}}
	s := New(&fakeProvider{}, WithCertificateManager(mgr))
	resp, err := s.PutSystemCertificate(context.Background(), mgmtapi.PutSystemCertificateRequestObject{
		Body: &mgmtapi.CertificateInstallRequest{CertPem: testCertPEM, KeyPem: secretKey},
	})
	if err != nil {
		t.Fatalf("PutSystemCertificate: %v", err)
	}
	vr, ok := resp.(mgmtapi.PutSystemCertificate200JSONResponse)
	if !ok {
		t.Fatalf("resp = %T, want 200", resp)
	}
	rec := httptest.NewRecorder()
	if verr := vr.VisitPutSystemCertificateResponse(rec); verr != nil {
		t.Fatalf("render response: %v", verr)
	}
	if strings.Contains(rec.Body.String(), "SUPERSECRETKEYMATERIAL") {
		t.Error("rendered install response contains private key material")
	}
	// Belt and suspenders: the manager received the key (so it was used) but it is
	// not in the response.
	if string(mgr.installedKey) != secretKey {
		t.Error("manager did not receive the submitted key")
	}
}

func TestGetSystemCertificateIncludesManaged(t *testing.T) {
	s := New(&fakeProvider{}, WithCertificate(&fakeCert{ci: CertificateInfo{Managed: true}}))
	resp, err := s.GetSystemCertificate(context.Background(), mgmtapi.GetSystemCertificateRequestObject{})
	if err != nil {
		t.Fatalf("GetSystemCertificate: %v", err)
	}
	got, ok := resp.(mgmtapi.GetSystemCertificate200JSONResponse)
	if !ok {
		t.Fatalf("resp = %T, want 200", resp)
	}
	// Sabotage target: Managed: ci.Managed in certificateToWire.
	if !got.Managed {
		t.Error("managed = false, want true")
	}
}
