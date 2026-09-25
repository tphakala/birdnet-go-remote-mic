//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

const (
	devHW1        = "hw:1,0"
	devHW2        = "hw:2,0"
	nameAudioMoth = "AudioMoth"
	testIfaceIP   = "192.168.1.5"
	testFmtS243LE = "s24_3le"
)

func TestProviderAvailableDevicesFiltersConfigured(t *testing.T) {
	p := newProvider()
	// One configured device on hw:1,0.
	p.setDevices([]*deviceRuntime{servingRecord("garden", "/garden")})
	// The host exposes hw:1,0 (configured, listed with no probed caps, as
	// DetectDevices emits it) and hw:2,0 (free, probed).
	p.setDetected([]audio.DetectedDevice{
		{ID: devHW1, FriendlyName: nameScarlett},
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
	st := config.Stream{Path: path, Mode: config.ModeOpus, Channels: []int{1}}
	return &deviceRuntime{
		dev: config.Device{
			Name: name, Device: devHW1, Rate: 48000, Format: testFmtS16,
			Streams: []config.Stream{st},
		},
		state:    mgmtserver.StateServing,
		rate:     48000,
		channels: 1,
		streams:  []*streamRuntime{{stream: st}},
	}
}

func skippedRecord(name, path, errMsg string) *deviceRuntime {
	return &deviceRuntime{
		dev: config.Device{
			Name: name, Device: "hw:2,0", Rate: 192000, Format: testFmtS16,
			Streams: []config.Stream{{Path: path, Mode: config.ModePCM, Channels: []int{1}}},
		},
		state: mgmtserver.StateSkipped,
		err:   errMsg,
	}
}

func newProvider() *provider {
	p := &provider{version: "v1.0.0", start: time.Now(), rtspListen: testRTSP8554}
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

func TestDeviceRuntimeStatusReportsNegotiated(t *testing.T) {
	// status() surfaces the negotiated rate, channel count and capture format only
	// for a device that actually opened (src != nil). A wider capture (here the
	// packed 24-bit s24_3le) is downconverted to the S16LE stream, and the token is
	// what the appliance reports so an operator can see that reduction.
	stream := config.Stream{Path: "/garden", Mode: config.ModeOpus, Channels: []int{1}}
	opened := &deviceRuntime{
		dev: config.Device{
			Name: "garden", Device: devHW1, Rate: 48000, Format: testFmtS16,
			Streams: []config.Stream{stream},
		},
		state:    mgmtserver.StateServing,
		src:      audio.NewFakeSource(48000, 1, nil),
		rate:     48000,
		channels: 1,
		format:   testFmtS243LE,
		streams:  []*streamRuntime{{stream: stream}},
	}
	ds := opened.status()
	if ds.NegotiatedRate != 48000 || ds.NegotiatedChannels != 1 {
		t.Errorf("negotiated rate/channels = %d/%d, want 48000/1", ds.NegotiatedRate, ds.NegotiatedChannels)
	}
	if ds.NegotiatedFormat != testFmtS243LE {
		t.Errorf("negotiatedFormat = %q, want s24_3le", ds.NegotiatedFormat)
	}

	// A device that never opened (src nil) reports no negotiated values, even if a
	// stale token lingered on the record: the src != nil gate must hide it.
	unopened := skippedRecord("attic", "/attic", "open capture: device busy")
	unopened.format = testFmtS243LE
	ds = unopened.status()
	if ds.NegotiatedRate != 0 || ds.NegotiatedChannels != 0 || ds.NegotiatedFormat != "" {
		t.Errorf("unopened device reported negotiated values: %d/%d/%q",
			ds.NegotiatedRate, ds.NegotiatedChannels, ds.NegotiatedFormat)
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

func TestNilMgmtWaitReturns(t *testing.T) {
	// A nil handle (management disabled) must make Wait return immediately so
	// shutdown never blocks on it, and report no serving API.
	var nilHandle *mgmt
	nilHandle.Wait()
	if nilHandle.serving() != nil {
		t.Error("a nil handle must report no serving API")
	}
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h, ok := startManagement(ctx, &mgmtParams{cfgPath: filepath.Join(t.TempDir(), "config.yaml"), cfg: cfg, storeCfg: cfg, prov: newProvider()})
	if ok {
		t.Error("a certificate failure must report management unavailable")
	}
	if h.serving() != nil {
		t.Error("a certificate failure must leave no serving API")
	}
	cancel()
	h.Wait() // cancelling ctx must stop the retry, so the handle does not block shutdown
}

func TestStartManagementBindFailureReportsUnavailable(t *testing.T) {
	// Occupy a port, then point the management listener at it so the bind fails.
	// A bind failure is retried like a certificate failure: once the port is
	// free, the background retry brings the API up on it.
	occupied, err := net.Listen("tcp", testListenAny)
	if err != nil {
		t.Fatal(err)
	}
	addr := occupied.Addr().String()

	cfg := &config.Config{Management: config.Management{Listen: addr, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A temp config path, so a stray config.yaml in the package directory cannot
	// change what the recovery attempt reloads.
	p := &mgmtParams{cfgPath: filepath.Join(t.TempDir(), "config.yaml"), cfg: cfg, storeCfg: cfg, prov: newProvider()}
	h, ok := startManagementWith(ctx, p, []time.Duration{10 * time.Millisecond})
	if ok {
		t.Error("a listener bind failure must report management unavailable")
	}
	if cerr := occupied.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if s := waitServing(t, h, nil); s.addr != addr {
		t.Errorf("recovered on %s, want the configured %s", s.addr, addr)
	}
	cancel()
	h.Wait()
}

func TestStartManagementServesAndShutsDown(t *testing.T) {
	// The happy path: the listener binds, ok is true, and Wait returns once ctx
	// cancellation drives a graceful shutdown.
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, ok := startManagement(ctx, &mgmtParams{cfgPath: testCfgFile, cfg: cfg, storeCfg: cfg, prov: newProvider()})
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	cancel() // trigger graceful shutdown
	h.Wait()
}

func TestStartManagementServesNotifications(t *testing.T) {
	// End-to-end wiring: a center handed to startManagement is reachable at GET
	// /api/v1/notifications, and its startup entry is already in the snapshot.
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	center := notify.NewCenter()
	center.Publish(notify.Started("v-test"))

	h, ok := startManagement(ctx, &mgmtParams{cfgPath: testCfgFile, cfg: cfg, storeCfg: cfg, prov: newProvider(), center: center})
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	defer h.Wait()
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+h.serving().addr+"/api/v1/notifications", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET notifications: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var snap mgmtapi.NotificationSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.BootId != center.BootID() {
		t.Errorf("bootId = %q, want %q", snap.BootId, center.BootID())
	}
	if len(snap.Notifications) != 1 {
		t.Fatalf("notifications = %+v, want one started entry", snap.Notifications)
	}
	n := snap.Notifications[0]
	if n.Category != mgmtapi.NotificationCategorySystem || n.Kind != mgmtapi.Event {
		t.Errorf("started entry category/kind wrong: %+v", n)
	}
	if !strings.Contains(n.Message, "v-test") {
		t.Errorf("message %q does not name the version", n.Message)
	}
}

func TestProviderAuthRequired(t *testing.T) {
	p := newProvider()
	if p.authRequired() {
		t.Fatal("a fresh provider must report open access")
	}
	p.setAuthRequired(true)
	if !p.authRequired() {
		t.Error("setAuthRequired(true) must be reported by authRequired")
	}
}

func TestAnnounceInfosCarryAuth(t *testing.T) {
	infos, port, err := announceInfos(testRTSP8554, []*deviceRuntime{servingRecord("garden", "/garden")}, true)
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

func TestInstanceLabelFitsOneDNSLabel(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 70)
	const north = " north"
	tests := []struct {
		name, in, suffix, want string
	}{
		{"short name unchanged", nameAudioMoth, "", nameAudioMoth},
		{"name at the budget unchanged", long[:labelBudget], "", long[:labelBudget]},
		{"ascii cut to the budget", long, "", long[:labelBudget]},
		// A 2-byte rune straddling the budget is dropped whole.
		{"2-byte rune never split", long[:labelBudget-1] + "ä" + "b", "", long[:labelBudget-1]},
		// A 3-byte rune whose first byte sits two bytes before the budget
		// needs two steps back to its start.
		{"3-byte rune never split", long[:labelBudget-2] + "€" + "b", "", long[:labelBudget-2]},
		{"trailing space trimmed", long[:labelBudget-1] + " rear", "", long[:labelBudget-1]},
		// The device name is cut, never the stream path that keeps a
		// fanned-out device's streams apart.
		{"suffix kept, name cut", long, north, long[:labelBudget-len(north)] + north},
		// A name cut just after a space loses it, so the suffix does not
		// follow a double space.
		{"space at the name cut trimmed", long[:labelBudget-len(north)-1] + " rear", north, long[:labelBudget-len(north)-1] + north},
		{"short name with suffix unchanged", nameAudioMoth, north, nameAudioMoth + north},
		// A path that alone exceeds the budget leaves no room for the name
		// and is cut itself.
		{"over-long suffix cut", nameAudioMoth, " " + long, long[:labelBudget]},
		// A name that fits keeps its spaces.
		{"fitting name unchanged", " " + nameAudioMoth + " ", "", " " + nameAudioMoth + " "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := instanceLabel(tc.in, tc.suffix)
			if got != tc.want {
				t.Errorf("instanceLabel(%q, %q) = %q, want %q", tc.in, tc.suffix, got, tc.want)
			}
			// A responder rename appends up to renameRoom bytes, and the
			// result must still fit one label.
			if len(got)+renameRoom > dnsLabelMax || !utf8.ValidString(got) {
				t.Errorf("instanceLabel(%q, %q) = %q: %d bytes leaves no room for a rename, or invalid UTF-8", tc.in, tc.suffix, got, len(got))
			}
		})
	}
}

// TestAnnounceInfosLongFannedOutName pins the wiring of instanceLabel into
// announceInfos: a device whose name alone exceeds a label still advertises
// each of its streams under its own name, within the label budget.
func TestAnnounceInfosLongFannedOutName(t *testing.T) {
	t.Parallel()
	rt := servingRecord(strings.Repeat("n", 100), "/north")
	south := config.Stream{Path: "/south", Mode: config.ModeOpus, Channels: []int{2}}
	rt.dev.Streams = append(rt.dev.Streams, south)
	rt.streams = append(rt.streams, &streamRuntime{stream: south})
	infos, _, err := announceInfos(testRTSP8554, []*deviceRuntime{rt, servingRecord(strings.Repeat("s", 100), "/solo")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("got %d records, want 3", len(infos))
	}
	seen := map[string]bool{}
	for _, in := range infos {
		if len(in.Name) > labelBudget {
			t.Errorf("record %s advertises %d bytes, over the %d-byte budget", in.Path, len(in.Name), labelBudget)
		}
		seen[in.Name] = true
	}
	if len(seen) != 3 {
		t.Errorf("names %v: want three distinct names", infos)
	}
	if !strings.HasSuffix(infos[0].Name, " north") || !strings.HasSuffix(infos[1].Name, " south") {
		t.Errorf("fanned-out names %q, %q: want each to keep its stream path", infos[0].Name, infos[1].Name)
	}
}

func TestStartManagementEnforcesBearer(t *testing.T) {
	cfg := &config.Config{Management: config.Management{Listen: testListenAny, CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())

	h, ok := startManagement(ctx, &mgmtParams{cfgPath: testCfgFile, cfg: cfg, storeCfg: cfg, prov: newProvider(), guard: auth.NewGuard(testAuthToken)})
	if !ok {
		t.Fatal("management should have started on an ephemeral port")
	}
	defer h.Wait()
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	get := func(path, bearer string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+h.serving().addr+path, http.NoBody)
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
			{ID: devHW1, FriendlyName: nameScarlett},
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

// TestRunEnumerationSignalsHardwareChange pins the hotplug trigger: the first
// enumeration only records the hardware, an unchanged one stays quiet, and a
// device moving to another card index (same stable id, new address) signals the
// run loop to retry devices that are down.
func TestRunEnumerationSignalsHardwareChange(t *testing.T) {
	// The fake reports each entry on entered and then waits for its result, so
	// the test knows the previous enumeration has fully finished (published and,
	// if due, signalled) whenever the next one enters.
	entered := make(chan struct{})
	results := make(chan []audio.DetectedDevice)
	stop := make(chan struct{})
	prev := detectDevices
	detectDevices = func(map[string]bool) ([]audio.DetectedDevice, error) {
		select {
		case entered <- struct{}{}:
		case <-stop:
			return nil, nil
		}
		select {
		case det := <-results:
			return det, nil
		case <-stop:
			return nil, nil
		}
	}
	defer func() { detectDevices = prev }()

	p := newProvider()
	p.enumTrigger = make(chan struct{}, 1)
	p.hwChanged = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.runEnumeration(ctx)
		close(done)
	}()
	defer func() {
		close(stop)
		cancel()
		<-done
	}()

	awaitEntry := func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("enumeration did not run")
		}
	}
	signalled := func() bool {
		select {
		case <-p.hwChanged:
			return true
		default:
			return false
		}
	}
	// next finishes the pending enumeration with det, triggers another, and
	// waits for it to enter, by which point det has been fully processed.
	next := func(det []audio.DetectedDevice) {
		t.Helper()
		results <- det
		p.enumTrigger <- struct{}{}
		awaitEntry()
	}

	boot := []audio.DetectedDevice{{ID: idMoth, HWAddr: addrHW3}, {ID: idScarlett, HWAddr: addrHW4}}
	awaitEntry() // the startup enumeration
	next(boot)
	if !signalled() {
		t.Error("the first enumeration did not signal a retry (a mic that finished enumerating after reconcile would stay down)")
	}
	next(boot)
	if signalled() {
		t.Fatal("an unchanged enumeration signalled a retry")
	}
	moved := []audio.DetectedDevice{{ID: idScarlett, HWAddr: addrHW4}, {ID: idMoth, HWAddr: addrHW5}}
	next(moved)
	if !signalled() {
		t.Error("a device moving to another card index did not signal a retry")
	}
	// A lost-device pump failure arms a retry, so the next enumeration signals
	// even though the hardware signature is unchanged (an unplug and replug at the
	// same index within one tick), and it fires exactly once.
	p.armRetry()
	next(moved)
	if !signalled() {
		t.Error("an armed retry with an unchanged signature did not signal")
	}
	next(moved)
	if signalled() {
		t.Error("the armed retry signalled more than once")
	}
}
