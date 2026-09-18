//go:build linux

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// genCertPEM builds a self-signed ECDSA pair for the given SANs and returns the
// PEM cert and key. It mirrors the appliance's own generation closely enough for
// the certificate-management tests (validity now, serverAuth EKU).
func genCertPEM(t *testing.T, cn string, dnsNames []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// fingerprintHex renders the SHA-256 of der as colon-separated uppercase hex, the
// same form mgmtcert uses, so a test can predict what the metadata will report.
func fingerprintHex(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// dialLeaf opens a TLS connection to addr (trusting anything) and returns the
// leaf certificate the server presented.
func dialLeaf(t *testing.T, addr string) *x509.Certificate {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test dials a self-signed appliance
	if err != nil {
		t.Fatalf("tls.Dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatal("server presented no certificate")
	}
	return certs[0]
}

func TestStartManagementServesRegeneratedCertToNewHandshake(t *testing.T) {
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newProvider()

	h, ok := startManagement(ctx, "config.yaml", cfg, cfg, prov, nil, nil, nil, nil, nil)
	if !ok {
		t.Fatal("management should have started")
	}
	defer h.Wait()
	defer cancel()

	before := dialLeaf(t, h.addr)

	// Regenerate with an extra SAN; the new certificate must reach a fresh handshake
	// (sabotage target: TLSConfig.GetCertificate = prov.tlsCertificate).
	if _, err := prov.Regenerate([]string{"mic.example.org"}); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	after := dialLeaf(t, h.addr)

	if before.SerialNumber.Cmp(after.SerialNumber) == 0 {
		t.Error("handshake after regenerate still served the old certificate")
	}
	if err := after.VerifyHostname("mic.example.org"); err != nil {
		t.Errorf("regenerated cert does not cover the extra SAN: %v", err)
	}
	if err := after.VerifyHostname("localhost"); err != nil {
		t.Errorf("regenerated cert dropped the base host: %v", err)
	}

	// GET /system/certificate must report the new fingerprint, and the pair must be
	// persisted (a restart would load it).
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	info := getCertInfo(t, ctx, client, h.addr)
	if info.FingerprintSha256 != fingerprintHex(after.Raw) {
		t.Errorf("reported fingerprint %q does not match the served cert", info.FingerprintSha256)
	}
	if !info.Managed {
		t.Error("a regenerated self-signed cert must report managed=true")
	}
}

func TestStartManagementKeepsInstalledCertAcrossRestart(t *testing.T) {
	certDir := t.TempDir()
	certPEM, keyPEM := genCertPEM(t, "operator-ca", []string{"custom.example"})
	wantLeaf, _ := x509.ParseCertificate(decodeCertDER(t, certPEM))
	wantFP := fingerprintHex(wantLeaf.Raw)

	// First run: install the custom certificate.
	func() {
		cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: certDir}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		prov := newProvider()
		h, ok := startManagement(ctx, "config.yaml", cfg, cfg, prov, nil, nil, nil, nil, nil)
		if !ok {
			t.Fatal("first run: management should have started")
		}
		if _, err := prov.Install(certPEM, keyPEM); err != nil {
			t.Fatalf("Install: %v", err)
		}
		cancel()
		h.Wait()
	}()

	// Second run over the same cert dir: Ensure must reuse the pinned custom pair,
	// not regenerate it (sabotage target: the pinned early return in mgmtcert.Ensure).
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: certDir}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, ok := startManagement(ctx, "config.yaml", cfg, cfg, newProvider(), nil, nil, nil, nil, nil)
	if !ok {
		t.Fatal("second run: management should have started")
	}
	defer h.Wait()
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	info := getCertInfo(t, ctx, client, h.addr)
	if info.FingerprintSha256 != wantFP {
		t.Errorf("after restart fingerprint %q, want the installed %q", info.FingerprintSha256, wantFP)
	}
	if info.Managed {
		t.Error("an operator-installed cert must report managed=false after a restart")
	}
}

func TestProviderCertificateSnapshotConsistent(t *testing.T) {
	// A reader must never see the metadata of one certificate with the PEM of
	// another. Run under -race; the single atomic Store is the sabotage target
	// (splitting the fields into separate stores tears here).
	prov := &provider{certPath: t.TempDir() + "/mgmt-cert.pem"}
	certA, _ := genCertPEM(t, "a", []string{"a.example"})
	certB, _ := genCertPEM(t, "b", []string{"b.example"})
	tlsA := mustTLS(t, certA)
	tlsB := mustTLS(t, certB)
	fpA := fingerprintHex(tlsA.Certificate[0])
	fpB := fingerprintHex(tlsB.Certificate[0])

	if err := prov.setCertificate(tlsA); err != nil {
		t.Fatalf("setCertificate: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				_ = prov.setCertificate(tlsA)
			} else {
				_ = prov.setCertificate(tlsB)
			}
		}
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 4000; i++ {
				// Read ONE snapshot and cross-check its three fields. A single
				// atomic.Load gives a coherent certState, so the metadata, the PEM,
				// and the parsed pair must all describe the same certificate. The
				// separate public accessors (Certificate, CertificatePEM) each Load
				// independently and may legitimately straddle a swap, which is why
				// this reads the snapshot directly. Run under -race.
				st := prov.cert.Load()
				if st == nil {
					t.Error("nil certificate snapshot")
					return
				}
				tlsFP := fingerprintHex(st.tls.Certificate[0])
				block, _ := pem.Decode(st.pem)
				if block == nil {
					t.Error("snapshot PEM is unparseable")
					return
				}
				if st.info.FingerprintSHA256 != tlsFP || fingerprintHex(block.Bytes) != tlsFP {
					t.Errorf("incoherent snapshot: info=%s tls=%s pem=%s", st.info.FingerprintSHA256, tlsFP, fingerprintHex(block.Bytes))
					return
				}
				if tlsFP != fpA && tlsFP != fpB {
					t.Errorf("fingerprint %s is neither A nor B", tlsFP)
					return
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestProviderCertificateReturnsDefensiveCopies(t *testing.T) {
	prov := &provider{certPath: t.TempDir() + "/mgmt-cert.pem"}
	certPEM, _ := genCertPEM(t, "copy", []string{"copy.example", "copy2.example"})
	if err := prov.setCertificate(mustTLS(t, certPEM)); err != nil {
		t.Fatalf("setCertificate: %v", err)
	}
	got := prov.Certificate()
	if len(got.DNSNames) == 0 {
		t.Fatal("no DNS names to mutate")
	}
	got.DNSNames[0] = "tampered"
	pemBytes := prov.CertificatePEM()
	if len(pemBytes) > 0 {
		pemBytes[0] = 'X'
	}
	// Sabotage target: the append([]string(nil), ...) and append([]byte(nil), ...)
	// defensive copies. Without them the mutations above would persist.
	again := prov.Certificate()
	if again.DNSNames[0] == "tampered" {
		t.Error("Certificate did not return a defensive copy of DNSNames")
	}
	if fresh := prov.CertificatePEM(); len(fresh) > 0 && fresh[0] == 'X' {
		t.Error("CertificatePEM did not return a defensive copy")
	}
}

func TestStartManagementNoKeyMaterialInCertEndpoints(t *testing.T) {
	certDir := t.TempDir()
	certPEM, keyPEM := genCertPEM(t, "nokey", []string{"nokey.example"})
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: certDir}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newProvider()
	h, ok := startManagement(ctx, "config.yaml", cfg, cfg, prov, nil, nil, nil, nil, nil)
	if !ok {
		t.Fatal("management should have started")
	}
	defer h.Wait()
	defer cancel()
	if _, err := prov.Install(certPEM, keyPEM); err != nil {
		t.Fatalf("Install: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	// The PEM endpoint returns only the public certificate.
	pemBody := getBody(t, ctx, client, h.addr, "/api/v1/system/certificate/pem")
	if strings.Contains(pemBody, "PRIVATE KEY") {
		t.Error("certificate PEM endpoint leaked private key material")
	}
	// The JSON metadata carries no key field.
	metaBody := getBody(t, ctx, client, h.addr, "/api/v1/system/certificate")
	if strings.Contains(metaBody, "PRIVATE KEY") || strings.Contains(strings.ToLower(metaBody), "keypem") {
		t.Error("certificate metadata leaked key material")
	}
}

func TestStartManagementEnforcesBearerOnCertWrites(t *testing.T) {
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	h, ok := startManagement(ctx, "config.yaml", cfg, cfg, newProvider(), nil, nil, nil, nil, auth.NewGuard(testAuthToken))
	if !ok {
		t.Fatal("management should have started")
	}
	defer h.Wait()
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	do := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "https://"+h.addr+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// Sabotage target: requireBearer wrapping the API subtree; both writes must 401.
	if got := do(http.MethodPut, "/api/v1/system/certificate"); got != http.StatusUnauthorized {
		t.Errorf("PUT without token = %d, want 401", got)
	}
	if got := do(http.MethodPost, "/api/v1/system/certificate/regenerate"); got != http.StatusUnauthorized {
		t.Errorf("POST regenerate without token = %d, want 401", got)
	}
}

func TestAppendUniqueHostsDedupes(t *testing.T) {
	// Distinct placeholder names, deliberately not reusing localhost/127.0.0.1 (the
	// dedup logic is host-agnostic, and repeating those literals here would trip the
	// goconst linter against the certHostsFor tests).
	base, dup, extra := "host-base", "host-dup", "host-extra"
	got := appendUniqueHosts([]string{base, dup}, []string{dup, "", extra, extra})
	want := []string{base, dup, extra}
	if len(got) != len(want) {
		t.Fatalf("appendUniqueHosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// mustTLS builds a *tls.Certificate from a PEM cert (regenerating a fresh key is
// not needed here because setCertificate only reads the leaf's public parts).
func mustTLS(t *testing.T, certPEM []byte) *tls.Certificate {
	t.Helper()
	der := decodeCertDER(t, certPEM)
	return &tls.Certificate{Certificate: [][]byte{der}}
}

func decodeCertDER(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("decode cert PEM")
	}
	return block.Bytes
}

func getCertInfo(t *testing.T, ctx context.Context, client *http.Client, addr string) mgmtapi.CertificateInfo {
	t.Helper()
	body := getBody(t, ctx, client, addr, "/api/v1/system/certificate")
	var info mgmtapi.CertificateInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("decode certificate info: %v", err)
	}
	return info
}

func getBody(t *testing.T, ctx context.Context, client *http.Client, addr, path string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
