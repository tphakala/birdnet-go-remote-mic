//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

const (
	devHW1        = "hw:1,0"
	devHW2        = "hw:2,0"
	nameAudioMoth = "AudioMoth"
	testIfaceIP   = "192.168.1.5"
)

func TestProviderAvailableDevicesFiltersConfigured(t *testing.T) {
	p := newProvider()
	// One configured device on hw:1,0.
	p.setDevices([]*deviceRuntime{servingRecord("garden", "/garden")})
	// The host exposes hw:1,0 (configured, listed with no probed caps, as
	// DetectDevices emits it) and hw:2,0 (free, probed).
	p.setDetected([]audio.DetectedDevice{
		{ID: devHW1, FriendlyName: "Scarlett"},
		{ID: devHW2, FriendlyName: nameAudioMoth, SupportedRates: []int{384000}, SupportedChannels: []int{1}},
	})

	avail := p.AvailableDevices()
	if len(avail) != 1 || avail[0].ID != devHW2 {
		t.Fatalf("AvailableDevices = %+v, want only the unconfigured hw:2,0", avail)
	}
	if avail[0].FriendlyName != nameAudioMoth || len(avail[0].SupportedRates) != 1 {
		t.Errorf("capabilities not passed through: %+v", avail[0])
	}

	// DetectedDevice is unfiltered: it returns a configured device too (even with
	// no caps), so provisioning distinguishes already-configured (409) from
	// absent (404).
	if _, ok := p.DetectedDevice(devHW1); !ok {
		t.Error("DetectedDevice(configured) = false, want true (unfiltered)")
	}
	if _, ok := p.DetectedDevice("hw:9,0"); ok {
		t.Error("DetectedDevice(absent) = true, want false")
	}
}

func servingRecord(name, path string) *deviceRuntime {
	return &deviceRuntime{
		dev: config.Device{
			Name: name, Device: devHW1, Path: path,
			Mode: config.ModeOpus, Rate: 48000, Channels: []int{1}, Format: testFmtS16,
		},
		state:    mgmtserver.StateServing,
		rate:     48000,
		channels: 1,
	}
}

func skippedRecord(name, path, errMsg string) *deviceRuntime {
	return &deviceRuntime{
		dev: config.Device{
			Name: name, Device: "hw:2,0", Path: path,
			Mode: config.ModePCM, Rate: 192000, Channels: []int{1}, Format: testFmtS16,
		},
		state: mgmtserver.StateSkipped,
		err:   errMsg,
	}
}

func newProvider() *provider {
	p := &provider{version: "v1.0.0", start: time.Now(), rtspListen: ":8554"}
	p.setDiscovery(true)
	return p
}

func TestProviderStatusDegradedBeforeSetDevices(t *testing.T) {
	p := newProvider()
	st := p.Status()
	if st.DevicesTotal != 0 || st.DevicesServing != 0 {
		t.Errorf("before setDevices want 0/0, got serving=%d total=%d", st.DevicesServing, st.DevicesTotal)
	}
	if got := p.Devices(); len(got) != 0 {
		t.Errorf("Devices() before setDevices = %d, want 0", len(got))
	}
	if _, ok := p.Device("garden"); ok {
		t.Error("Device lookup before setDevices should miss")
	}
}

func TestProviderStatusCountsServing(t *testing.T) {
	p := newProvider()
	p.setDevices([]*deviceRuntime{
		servingRecord("garden", "/garden"),
		skippedRecord("attic", "/attic", "open capture: device busy"),
	})
	st := p.Status()
	if st.DevicesTotal != 2 || st.DevicesServing != 1 {
		t.Errorf("want serving=1 total=2, got serving=%d total=%d", st.DevicesServing, st.DevicesTotal)
	}
}

func TestProviderDeviceLookup(t *testing.T) {
	p := newProvider()
	p.setDevices([]*deviceRuntime{
		servingRecord("garden", "/garden"),
		skippedRecord("attic", "/attic", "open capture: device busy"),
	})

	d, ok := p.Device("attic")
	if !ok {
		t.Fatal("Device(attic) not found")
	}
	if d.State != mgmtserver.StateSkipped || d.Error != "open capture: device busy" {
		t.Errorf("attic mapped wrong: state=%q err=%q", d.State, d.Error)
	}
	if _, ok := p.Device("ghost"); ok {
		t.Error("Device(ghost) should not be found")
	}
}

func TestClosedMgmtWaitReturns(t *testing.T) {
	// A nil handle (management disabled) and a closed handle (cert failure) must
	// both make Wait return immediately so shutdown never blocks on them.
	var nilHandle *mgmt
	nilHandle.Wait()
	closedMgmt().Wait()
}

func TestStartManagementCertFailureReportsUnavailable(t *testing.T) {
	// Point cert_dir at a regular file so certificate persistence cannot succeed.
	// A configured-but-dead API must report ok=false so run() does not treat it as
	// a live diagnostic surface.
	badDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: badDir}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil, nil, nil, nil)
	if ok {
		t.Error("a certificate failure must report management unavailable")
	}
	h.Wait() // the returned handle must not block shutdown
}

func TestStartManagementBindFailureReportsUnavailable(t *testing.T) {
	// Occupy a port, then point the management listener at it so the bind fails.
	occupied, err := net.Listen("tcp", testListenAny)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := occupied.Close(); cerr != nil {
			t.Errorf("closing occupied listener: %v", cerr)
		}
	}()

	cfg := &config.Config{Management: config.Management{Listen: occupied.Addr().String(), CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil, nil, nil, nil)
	if ok {
		t.Error("a listener bind failure must report management unavailable")
	}
	h.Wait()
}

func TestStartManagementServesAndShutsDown(t *testing.T) {
	// The happy path: the listener binds, ok is true, and Wait returns once ctx
	// cancellation drives a graceful shutdown.
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil, nil, nil, nil)
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	cancel() // trigger graceful shutdown
	h.Wait()
}

func TestProviderStatusAuthRequired(t *testing.T) {
	p := newProvider()
	if p.Status().AuthRequired {
		t.Fatal("a fresh provider must report open access")
	}
	p.setAuthRequired(true)
	if !p.Status().AuthRequired {
		t.Error("setAuthRequired(true) must be reported by Status")
	}
}

func TestAnnounceInfosCarryAuth(t *testing.T) {
	infos, port, err := announceInfos(":8554", []*deviceRuntime{servingRecord("garden", "/garden")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if port != 8554 {
		t.Errorf("port = %d, want 8554", port)
	}
	if len(infos) != 1 || !infos[0].AuthRequired {
		t.Errorf("infos = %+v, want one entry with AuthRequired", infos)
	}
	if _, _, err := announceInfos("not-an-address", nil, false); err == nil {
		t.Error("an unparsable listen address must be an error")
	}
}

func TestStartManagementEnforcesBearer(t *testing.T) {
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())

	h, ok := startManagement(ctx, "config.yaml", cfg, newProvider(), nil, nil, nil, auth.NewGuard(testAuthToken))
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	defer h.Wait()
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	get := func(path, bearer string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+h.addr+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("/api/v1/healthz", ""); got != http.StatusOK {
		t.Errorf("healthz without a token = %d, want 200", got)
	}
	if got := get("/api/v1/status", ""); got != http.StatusUnauthorized {
		t.Errorf("status without a token = %d, want 401", got)
	}
	if got := get("/api/v1/status", testAuthToken); got != http.StatusOK {
		t.Errorf("status with the token = %d, want 200", got)
	}
}

// TestRunEnumerationWiresDetectionToProvider drives the enumeration goroutine
// end to end: DetectDevices is called with the configured-id skip set, its
// result is published, and the two provider views (unfiltered DetectedDevice,
// filtered AvailableDevices) reflect it.
func TestRunEnumerationWiresDetectionToProvider(t *testing.T) {
	var gotSkip map[string]bool
	called := make(chan struct{}, 1)
	prev := detectDevices
	detectDevices = func(skip map[string]bool) ([]audio.DetectedDevice, error) {
		gotSkip = skip
		select {
		case called <- struct{}{}:
		default:
		}
		return []audio.DetectedDevice{
			{ID: devHW1, FriendlyName: "Scarlett"},
			{ID: devHW2, FriendlyName: nameAudioMoth, SupportedRates: []int{384000}, SupportedChannels: []int{1}},
		}, nil
	}
	defer func() { detectDevices = prev }()

	p := newProvider()
	p.enumTrigger = make(chan struct{}, 1)
	p.setConfiguredIDs(map[string]bool{devHW1: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.runEnumeration(ctx)
		close(done)
	}()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("DetectDevices was not called")
	}
	cancel()
	<-done

	if !gotSkip[devHW1] {
		t.Errorf("DetectDevices skip set = %v, want the configured hw:1,0", gotSkip)
	}
	if _, ok := p.DetectedDevice(devHW1); !ok {
		t.Error("DetectedDevice(configured) = false, want true (unfiltered view)")
	}
	if _, ok := p.DetectedDevice(devHW2); !ok {
		t.Error("DetectedDevice(free) = false, want true")
	}
	avail := p.AvailableDevices()
	if len(avail) != 1 || avail[0].ID != devHW2 {
		t.Errorf("AvailableDevices = %+v, want only the unconfigured hw:2,0", avail)
	}
}

func TestCertHostsForCoversDiscoveredDotLocal(t *testing.T) {
	// The appliance advertises <hostname>.local over DNS-SD, so that name must be
	// in the certificate SANs: otherwise the discovered https://<host>.local URL
	// fails verification and the operator falls back to curl -k, which disables
	// verification entirely and exposes the bearer token to an impersonator.
	// The duplicate interface IP must collapse to a single SAN, exercising the
	// add() dedup on the IP path as well as the name path.
	got := certHostsFor("birdmic", []string{testIfaceIP, testIfaceIP})
	for _, want := range []string{"localhost", "127.0.0.1", "::1", "birdmic", "birdmic.local", testIfaceIP} {
		if !slices.Contains(got, want) {
			t.Errorf("certHostsFor missing %q; got %v", want, got)
		}
	}
	if dupes := duplicates(got); len(dupes) != 0 {
		t.Errorf("certHostsFor has duplicate SANs %v in %v", dupes, got)
	}
}

func TestCertHostsForHostnameAlreadyDotLocal(t *testing.T) {
	// A host whose name already ends in .local must yield a single .local SAN, not
	// a duplicate and not birdmic.local.local.
	got := certHostsFor("birdmic.local", nil)
	if c := countOf(got, "birdmic.local"); c != 1 {
		t.Errorf("want exactly one birdmic.local SAN, got %d in %v", c, got)
	}
	if slices.Contains(got, "birdmic.local.local") {
		t.Errorf(".local appended twice: %v", got)
	}
}

func TestCertHostsForNoHostname(t *testing.T) {
	// With no resolvable hostname the loopback SANs still stand and nothing empty
	// leaks into the list.
	got := certHostsFor("", nil)
	if slices.Contains(got, "") {
		t.Errorf("empty SAN leaked into %v", got)
	}
	for _, want := range []string{"localhost", "127.0.0.1", "::1"} {
		if !slices.Contains(got, want) {
			t.Errorf("certHostsFor missing loopback SAN %q; got %v", want, got)
		}
	}
}

func countOf(list []string, v string) int {
	n := 0
	for _, s := range list {
		if s == v {
			n++
		}
	}
	return n
}

func duplicates(list []string) []string {
	seen := map[string]bool{}
	var dupes []string
	for _, s := range list {
		if seen[s] {
			dupes = append(dupes, s)
		}
		seen[s] = true
	}
	return dupes
}
