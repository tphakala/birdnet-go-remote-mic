package mgmtserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

const (
	testVersion = "v1.2.3"
	nameAttic   = "attic"
	rtspAddr    = ":8554"
)

type fakeProvider struct {
	status  ApplianceStatus
	devices []DeviceStatus
}

func (f *fakeProvider) Version() string         { return f.status.Version }
func (f *fakeProvider) Status() ApplianceStatus { return f.status }
func (f *fakeProvider) Devices() []DeviceStatus { return f.devices }

func servingOpus() DeviceStatus {
	return DeviceStatus{
		Config: config.Device{
			Name: "garden", Device: "hw:1,0", Path: "/garden",
			Mode: config.ModeOpus, Rate: 48000, Channels: 1, Format: "s16",
			Opus: config.Opus{Bitrate: 96000},
		},
		State:              StateServing,
		NegotiatedRate:     48000,
		NegotiatedChannels: 1,
		ClientConnected:    true,
		DroppedFrames:      12,
	}
}

func skippedPCM() DeviceStatus {
	return DeviceStatus{
		Config: config.Device{
			Name: nameAttic, Device: "hw:2,0", Path: "/attic",
			Mode: config.ModePCM, Rate: 192000, Channels: 1, Format: "s16",
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
	if d.Name != "garden" || d.Mode != mgmtapi.Opus || d.Rate != 48000 || d.Channels != 1 {
		t.Errorf("config fields wrong: %+v", d)
	}
	if d.State != mgmtapi.Serving {
		t.Errorf("state = %q, want serving", d.State)
	}
	if d.NegotiatedRate == nil || *d.NegotiatedRate != 48000 {
		t.Errorf("negotiatedRate = %v, want 48000", d.NegotiatedRate)
	}
	if d.NegotiatedChannels == nil || *d.NegotiatedChannels != 1 {
		t.Errorf("negotiatedChannels = %v, want 1", d.NegotiatedChannels)
	}
	if !d.ClientConnected || d.DroppedFrames != 12 {
		t.Errorf("runtime fields wrong: connected=%v dropped=%d", d.ClientConnected, d.DroppedFrames)
	}
	if d.Opus == nil || d.Opus.Bitrate == nil || *d.Opus.Bitrate != 96000 {
		t.Errorf("opus settings not mapped: %+v", d.Opus)
	}
	if d.Error != nil {
		t.Errorf("serving device should have no error, got %v", *d.Error)
	}
}

func TestListDevicesMapsSkippedPCM(t *testing.T) {
	s := New(&fakeProvider{devices: []DeviceStatus{skippedPCM()}})
	resp, _ := s.ListDevices(context.Background(), mgmtapi.ListDevicesRequestObject{})
	list := resp.(mgmtapi.ListDevices200JSONResponse)
	d := list[0]
	if d.State != mgmtapi.Skipped {
		t.Errorf("state = %q, want skipped", d.State)
	}
	if d.Mode != mgmtapi.Pcm {
		t.Errorf("mode = %q, want pcm", d.Mode)
	}
	if d.NegotiatedRate != nil || d.NegotiatedChannels != nil {
		t.Errorf("skipped device must have no negotiated values: %v %v", d.NegotiatedRate, d.NegotiatedChannels)
	}
	if d.ClientConnected || d.DroppedFrames != 0 {
		t.Errorf("skipped device runtime should be zero: connected=%v dropped=%d", d.ClientConnected, d.DroppedFrames)
	}
	if d.Opus != nil {
		t.Errorf("pcm device must not carry opus settings, got %+v", d.Opus)
	}
	if d.Error == nil || *d.Error != "open capture: device busy" {
		t.Errorf("skipped device error not mapped: %v", d.Error)
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
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
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
