package mgmtserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

const (
	testVersion = "v1.2.3"
	nameAttic   = "attic"
	devAttic    = "hw:2,0"
	pathAttic   = "/attic"
	rtspAddr    = ":8554"
)

type fakeProvider struct {
	status    ApplianceStatus
	devices   []DeviceStatus
	available []AvailableDevice
	// detected lists host devices the available view hides because the config
	// owns them, as the real provider's unfiltered enumeration does. The fake's
	// detected view is detected plus available.
	detected []AvailableDevice
}

func (f *fakeProvider) Version() string                     { return f.status.Version }
func (f *fakeProvider) Status() ApplianceStatus             { return f.status }
func (f *fakeProvider) Devices() []DeviceStatus             { return f.devices }
func (f *fakeProvider) AvailableDevices() []AvailableDevice { return f.available }

func (f *fakeProvider) DetectedDevice(id string) (AvailableDevice, bool) {
	for _, list := range [][]AvailableDevice{f.detected, f.available} {
		for i := range list {
			if list[i].ID == id {
				return list[i], true
			}
		}
	}
	return AvailableDevice{}, false
}

func (f *fakeProvider) Device(name string) (DeviceStatus, bool) {
	for i := range f.devices {
		if f.devices[i].Config.Name == name {
			return f.devices[i], true
		}
	}
	return DeviceStatus{}, false
}

func servingOpus() DeviceStatus {
	return DeviceStatus{
		Config: config.Device{
			Name: devGarden, Device: devHW1, Rate: 48000, Format: fmtS16,
			Streams: []config.Stream{{
				Path: "/garden", Mode: config.ModeOpus, Channels: []int{1},
				Opus: config.Opus{Bitrate: 96000},
			}},
		},
		State:              StateServing,
		NegotiatedRate:     48000,
		NegotiatedChannels: 1,
		NegotiatedFormat:   "s24_3le",
		ClientConnected:    true,
		DroppedFrames:      12,
		Overruns:           9,
		Streams:            []StreamStatus{{Path: "/garden", ClientConnected: true, DroppedFrames: 12}},
		FriendlyName:       nameScarlett,
		SupportedRates:     []int{48000, 96000, 192000},
		SupportedChannels:  []int{1, 2},
	}
}

func skippedPCM() DeviceStatus {
	return DeviceStatus{
		Config: config.Device{
			Name: nameAttic, Device: devAttic, Rate: 192000, Format: "s16",
			Streams: []config.Stream{{Path: pathAttic, Mode: config.ModePCM, Channels: []int{1}}},
		},
		State: StateSkipped,
		Error: "open capture: device busy",
	}
}

func TestGetHealthReportsOkAndVersion(t *testing.T) {
	s := New(&fakeProvider{status: ApplianceStatus{Version: testVersion}})
	resp, err := s.GetHealth(context.Background(), mgmtapi.GetHealthRequestObject{})
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	h, ok := resp.(mgmtapi.GetHealth200JSONResponse)
	if !ok {
		t.Fatalf("GetHealth returned %T, want GetHealth200JSONResponse", resp)
	}
	if h.Status != mgmtapi.Ok {
		t.Errorf("status = %q, want ok", h.Status)
	}
	if h.Version != testVersion {
		t.Errorf("version = %q, want v1.2.3", h.Version)
	}
}

// TestGetHealthReportsAuthRequired asserts the open liveness probe says
// whether a token is required, following the guard as it is set and cleared.
func TestGetHealthReportsAuthRequired(t *testing.T) {
	g := auth.NewGuard("")
	s := New(&fakeProvider{}, WithAuth(g))
	health := func() bool {
		t.Helper()
		resp, err := s.GetHealth(context.Background(), mgmtapi.GetHealthRequestObject{})
		if err != nil {
			t.Fatalf("GetHealth: %v", err)
		}
		return resp.(mgmtapi.GetHealth200JSONResponse).AuthRequired
	}
	if health() {
		t.Fatal("open appliance reports authRequired=true")
	}
	g.Set("a-valid-token-123")
	if !health() {
		t.Fatal("token-gated appliance reports authRequired=false")
	}
	g.Set("")
	if health() {
		t.Fatal("cleared token still reports authRequired=true")
	}
}

func TestGetStatusMapsFields(t *testing.T) {
	s := New(&fakeProvider{status: ApplianceStatus{
		Version:          testVersion,
		Uptime:           90 * time.Second,
		RTSPListen:       rtspAddr,
		DiscoveryEnabled: true,
		DevicesServing:   2,
		DevicesTotal:     3,
	}})
	resp, err := s.GetStatus(context.Background(), mgmtapi.GetStatusRequestObject{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	st, ok := resp.(mgmtapi.GetStatus200JSONResponse)
	if !ok {
		t.Fatalf("GetStatus returned %T, want GetStatus200JSONResponse", resp)
	}
	if st.Version != testVersion || st.RtspListen != rtspAddr || !st.DiscoveryEnabled {
		t.Errorf("scalar fields wrong: %+v", st)
	}
	if st.UptimeSeconds != 90 {
		t.Errorf("uptimeSeconds = %d, want 90", st.UptimeSeconds)
	}
	if st.DevicesServing != 2 || st.DevicesTotal != 3 {
		t.Errorf("device counts wrong: serving=%d total=%d", st.DevicesServing, st.DevicesTotal)
	}
	// With no serve overrides the optional array is omitted (nil), not an empty
	// array, so a run without overrides sends no overrides key.
	if st.Overrides != nil {
		t.Errorf("overrides = %v, want nil when none are active", st.Overrides)
	}
}

func TestGetStatusMapsOverrides(t *testing.T) {
	s := New(&fakeProvider{status: ApplianceStatus{
		Version:    testVersion,
		RTSPListen: rtspAddr,
		Overrides: []ConfigOverride{
			{Field: "listen", Effective: ":9000", Persisted: rtspAddr},
			{Field: "discovery.enabled", Effective: "false", Persisted: "true"},
		},
	}})
	resp, err := s.GetStatus(context.Background(), mgmtapi.GetStatusRequestObject{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	st, ok := resp.(mgmtapi.GetStatus200JSONResponse)
	if !ok {
		t.Fatalf("GetStatus returned %T, want GetStatus200JSONResponse", resp)
	}
	if st.Overrides == nil {
		t.Fatal("overrides = nil, want two entries")
	}
	ovs := *st.Overrides
	if len(ovs) != 2 {
		t.Fatalf("overrides len = %d, want 2", len(ovs))
	}
	if ovs[0].Field != "listen" || ovs[0].Effective != ":9000" || ovs[0].Persisted != rtspAddr {
		t.Errorf("overrides[0] = %+v", ovs[0])
	}
	if ovs[1].Field != "discovery.enabled" || ovs[1].Effective != "false" || ovs[1].Persisted != "true" {
		t.Errorf("overrides[1] = %+v", ovs[1])
	}
}

func TestListDevicesMapsServingOpus(t *testing.T) {
	s := New(&fakeProvider{devices: []DeviceStatus{servingOpus()}})
	resp, err := s.ListDevices(context.Background(), mgmtapi.ListDevicesRequestObject{})
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	list, ok := resp.(mgmtapi.ListDevices200JSONResponse)
	if !ok {
		t.Fatalf("ListDevices returned %T", resp)
	}
	if len(list) != 1 {
		t.Fatalf("got %d devices, want 1", len(list))
	}
	d := list[0]
	if d.Name != devGarden || d.Mode != mgmtapi.Opus || d.Rate != 48000 || !slices.Equal(d.Channels, []int{1}) {
		t.Errorf("config fields wrong: %+v", d)
	}
	if d.State != mgmtapi.DeviceStateServing {
		t.Errorf("state = %q, want serving", d.State)
	}
	if d.NegotiatedRate == nil || *d.NegotiatedRate != 48000 {
		t.Errorf("negotiatedRate = %v, want 48000", d.NegotiatedRate)
	}
	if d.NegotiatedChannels == nil || *d.NegotiatedChannels != 1 {
		t.Errorf("negotiatedChannels = %v, want 1", d.NegotiatedChannels)
	}
	if d.NegotiatedFormat == nil || *d.NegotiatedFormat != "s24_3le" {
		t.Errorf("negotiatedFormat = %v, want s24_3le", d.NegotiatedFormat)
	}
	if !d.ClientConnected || d.DroppedFrames != 12 || d.Overruns != 9 {
		t.Errorf("runtime fields wrong: connected=%v dropped=%d overruns=%d", d.ClientConnected, d.DroppedFrames, d.Overruns)
	}
	if d.Opus == nil || d.Opus.Bitrate == nil || *d.Opus.Bitrate != 96000 {
		t.Errorf("opus settings not mapped: %+v", d.Opus)
	}
	if d.Error != nil {
		t.Errorf("serving device should have no error, got %v", *d.Error)
	}
	if d.FriendlyName == nil || *d.FriendlyName != nameScarlett {
		t.Errorf("friendlyName = %v, want Scarlett 2i2 USB", d.FriendlyName)
	}
	if d.SupportedRates == nil || !slices.Equal(*d.SupportedRates, []int{48000, 96000, 192000}) {
		t.Errorf("supportedRates = %v, want [48000 96000 192000]", d.SupportedRates)
	}
	if d.SupportedChannels == nil || !slices.Equal(*d.SupportedChannels, []int{1, 2}) {
		t.Errorf("supportedChannels = %v, want [1 2]", d.SupportedChannels)
	}
}

// TestMapDeviceDownCause pins that the down class reaches the wire only for a
// device that is not serving.
func TestMapDeviceDownCause(t *testing.T) {
	t.Parallel()
	skipped := skippedPCM()
	skipped.DownCause = "not-connected"
	if got := mapDevice(&skipped).DownCause; got == nil || *got != mgmtapi.DeviceDownCauseNotConnected {
		t.Errorf("skipped downCause = %v, want not-connected", got)
	}
	serving := skipped
	serving.State = StateServing
	if got := mapDevice(&serving).DownCause; got != nil {
		t.Errorf("serving downCause = %v, want absent", *got)
	}
	skipped.DownCause = ""
	if got := mapDevice(&skipped).DownCause; got != nil {
		t.Errorf("unclassified skip downCause = %v, want absent", *got)
	}
}

func TestListDevicesMapsSkippedPCM(t *testing.T) {
	s := New(&fakeProvider{devices: []DeviceStatus{skippedPCM()}})
	resp, _ := s.ListDevices(context.Background(), mgmtapi.ListDevicesRequestObject{})
	list := resp.(mgmtapi.ListDevices200JSONResponse)
	d := list[0]
	if d.State != mgmtapi.DeviceStateSkipped {
		t.Errorf("state = %q, want skipped", d.State)
	}
	if d.Mode != mgmtapi.Pcm {
		t.Errorf("mode = %q, want pcm", d.Mode)
	}
	if d.NegotiatedRate != nil || d.NegotiatedChannels != nil || d.NegotiatedFormat != nil {
		t.Errorf("skipped device must have no negotiated values: %v %v %v", d.NegotiatedRate, d.NegotiatedChannels, d.NegotiatedFormat)
	}
	if d.ClientConnected || d.DroppedFrames != 0 || d.Overruns != 0 {
		t.Errorf("skipped device runtime should be zero: connected=%v dropped=%d overruns=%d", d.ClientConnected, d.DroppedFrames, d.Overruns)
	}
	if d.Opus != nil {
		t.Errorf("pcm device must not carry opus settings, got %+v", d.Opus)
	}
	if d.Error == nil || *d.Error != "open capture: device busy" {
		t.Errorf("skipped device error not mapped: %v", d.Error)
	}
	if d.FriendlyName != nil {
		t.Errorf("device with no friendly name must omit it, got %v", *d.FriendlyName)
	}
	if d.SupportedRates != nil {
		t.Errorf("device with no probed rates must omit supportedRates, got %v", *d.SupportedRates)
	}
	if d.SupportedChannels != nil {
		t.Errorf("device with no probed channels must omit supportedChannels, got %v", *d.SupportedChannels)
	}
}

func TestGetDeviceFound(t *testing.T) {
	s := New(&fakeProvider{devices: []DeviceStatus{servingOpus(), skippedPCM()}})
	resp, err := s.GetDevice(context.Background(), mgmtapi.GetDeviceRequestObject{Name: nameAttic})
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	d, ok := resp.(mgmtapi.GetDevice200JSONResponse)
	if !ok {
		t.Fatalf("GetDevice returned %T, want 200", resp)
	}
	if d.Name != nameAttic {
		t.Errorf("got device %q, want attic", d.Name)
	}
}

func TestGetDeviceNotFound(t *testing.T) {
	s := New(&fakeProvider{devices: []DeviceStatus{servingOpus()}})
	resp, err := s.GetDevice(context.Background(), mgmtapi.GetDeviceRequestObject{Name: "nope"})
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	p, ok := resp.(mgmtapi.GetDevice404ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("GetDevice returned %T, want 404", resp)
	}
	if p.Status == nil || *p.Status != 404 {
		t.Errorf("problem status = %v, want 404", p.Status)
	}
}

func TestHandlerServesReadEndpointsUnderBasePath(t *testing.T) {
	s := New(&fakeProvider{
		status:  ApplianceStatus{Version: "v9", RTSPListen: rtspAddr, DiscoveryEnabled: true, DevicesServing: 1, DevicesTotal: 2},
		devices: []DeviceStatus{servingOpus(), skippedPCM()},
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	t.Run("healthz", func(t *testing.T) {
		resp := get("/api/v1/healthz")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var h mgmtapi.Health
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if h.Status != mgmtapi.Ok || h.Version != "v9" {
			t.Errorf("health body wrong: %+v", h)
		}
	})

	t.Run("status", func(t *testing.T) {
		resp := get("/api/v1/status")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("devices list", func(t *testing.T) {
		resp := get("/api/v1/devices")
		defer func() { _ = resp.Body.Close() }()
		var list []mgmtapi.Device
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("got %d devices, want 2", len(list))
		}
	})

	t.Run("device found", func(t *testing.T) {
		resp := get("/api/v1/devices/garden")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("device missing 404", func(t *testing.T) {
		resp := get("/api/v1/devices/ghost")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 404 {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != problemJSONType {
			t.Errorf("content-type = %q, want application/problem+json", ct)
		}
	})

	t.Run("config not implemented", func(t *testing.T) {
		resp := get("/api/v1/config")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 501 {
			t.Errorf("status = %d, want 501", resp.StatusCode)
		}
	})

	t.Run("malformed patch body yields problem+json 400", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/api/v1/config", strings.NewReader("{ not json"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 400 {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != problemJSONType {
			t.Errorf("content-type = %q, want application/problem+json", ct)
		}
	})
}

func TestOversizedBodyYieldsProblem413(t *testing.T) {
	t.Parallel()
	h := New(&fakeProvider{}).Handler()
	for _, tc := range []struct {
		name string
		size int
		want int
	}{
		// A body of exactly the cap still decodes (and fails as a 501 from the
		// unconfigured store), so the cap does not reject legitimate bodies; one
		// byte more is refused. size is the whole body length.
		{"at cap", maxRequestBody, http.StatusNotImplemented},
		{"one byte over cap", maxRequestBody + 1, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const head, tail = `{"discovery":{"enabled":true},"pad":"`, `"}`
			body := head + strings.Repeat("a", tc.size-len(head)-len(tail)) + tail
			if len(body) != tc.size {
				t.Fatalf("body length = %d, want %d", len(body), tc.size)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/api/v1/config", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// Sabotage target: limitBody in Handler. Without it the oversized
			// body decodes in full and the store's 501 comes back instead.
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != problemJSONType {
				t.Errorf("content-type = %q, want application/problem+json", ct)
			}
			var p mgmtapi.Problem
			if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if p.Status == nil {
				t.Errorf("problem status missing, want %d", tc.want)
			} else if *p.Status != tc.want {
				t.Errorf("problem status = %d, want %d", *p.Status, tc.want)
			}
			// The 413 detail names the limit, so a client can tell the operator
			// what to cut rather than only that the body was refused.
			if tc.want == http.StatusRequestEntityTooLarge {
				limit := fmt.Sprintf("%d KiB", maxRequestBody>>10)
				if p.Detail == nil || !strings.Contains(*p.Detail, limit) {
					t.Errorf("problem detail = %v, want it to name the %s limit", p.Detail, limit)
				}
			}
		})
	}
}

func TestEventsRoutedToStreamHandlerWhenMounted(t *testing.T) {
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
	})
	s := New(&fakeProvider{status: ApplianceStatus{Version: "v1"}}, WithEventStream(stream))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	// The outer mux must still delegate the generated endpoints.
	health, err := http.Get(srv.URL + "/api/v1/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer func() { _ = health.Body.Close() }()
	if health.StatusCode != 200 {
		t.Errorf("healthz status = %d, want 200", health.StatusCode)
	}
}

func TestEventsNotImplementedWithoutStream(t *testing.T) {
	s := New(&fakeProvider{status: ApplianceStatus{Version: "v1"}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 501 {
		t.Errorf("events status = %d, want 501 (no stream mounted)", resp.StatusCode)
	}
}

func TestGetConfigNotImplemented(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	p, ok := resp.(mgmtapi.GetConfigdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("GetConfig returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestPatchConfigNotImplemented(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	p, ok := resp.(mgmtapi.PatchConfigdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestStreamEventsNotImplemented(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.StreamEvents(context.Background(), mgmtapi.StreamEventsRequestObject{})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	p, ok := resp.(mgmtapi.StreamEventsdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("StreamEvents returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}
