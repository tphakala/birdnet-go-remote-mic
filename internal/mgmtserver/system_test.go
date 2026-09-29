package mgmtserver

import (
	"context"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

type fakeSystem struct{ si SystemInfo }

func (f *fakeSystem) System() SystemInfo { return f.si }

func TestGetSystemNotImplementedWithoutProvider(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.GetSystem(context.Background(), mgmtapi.GetSystemRequestObject{})
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	p, ok := resp.(mgmtapi.GetSystemdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("GetSystem returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestGetSystemMapsFields(t *testing.T) {
	pct := 42.5
	temp := 47.2
	sys := &fakeSystem{si: SystemInfo{
		Platform:    "linux/arm64",
		OS:          "Debian GNU/Linux 13 (trixie)",
		Kernel:      "6.6.20",
		Hostname:    "birdmic",
		CPUModel:    "Raspberry Pi Zero 2 W",
		CPUCores:    4,
		CPUPercent:  &pct,
		MemTotal:    4_000_000,
		MemUsed:     1_500_000,
		DiskTotal:   32_000_000_000,
		DiskUsed:    8_000_000_000,
		TempCelsius: &temp,
		Network: []NetworkInterface{
			{Name: ifaceEth0, MAC: "dc:a6:32:00:11:22", Up: true, Addresses: []string{"192.168.1.20/24"}, RxBytes: 100, TxBytes: 200},
			{Name: "wlan0", Up: false},
		},
	}}
	s := New(&fakeProvider{}, WithSystemInfo(sys))
	resp, err := s.GetSystem(context.Background(), mgmtapi.GetSystemRequestObject{})
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	got, ok := resp.(mgmtapi.GetSystem200JSONResponse)
	if !ok {
		t.Fatalf("GetSystem returned %T, want 200", resp)
	}
	if got.Platform != "linux/arm64" || got.CpuCores != 4 || got.Hostname != "birdmic" {
		t.Errorf("scalar fields wrong: %+v", got)
	}
	if got.Os == nil || *got.Os != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("os = %v", got.Os)
	}
	if got.CpuModel == nil || *got.CpuModel != "Raspberry Pi Zero 2 W" {
		t.Errorf("cpuModel = %v", got.CpuModel)
	}
	if got.CpuPercent == nil || *got.CpuPercent != 42.5 {
		t.Errorf("cpuPercent = %v", got.CpuPercent)
	}
	if got.TempCelsius == nil || *got.TempCelsius != 47.2 {
		t.Errorf("tempCelsius = %v", got.TempCelsius)
	}
	if got.MemTotalBytes != 4_000_000 || got.DiskUsedBytes != 8_000_000_000 {
		t.Errorf("byte fields wrong: mem %d disk %d", got.MemTotalBytes, got.DiskUsedBytes)
	}
	if len(got.Network) != 2 {
		t.Fatalf("network len = %d, want 2", len(got.Network))
	}
	eth := got.Network[0]
	if eth.Name != "eth0" || eth.Mac == nil || *eth.Mac != "dc:a6:32:00:11:22" || !eth.Up {
		t.Errorf("eth0 mapped wrong: %+v", eth)
	}
	if len(eth.Addresses) != 1 || eth.Addresses[0] != "192.168.1.20/24" {
		t.Errorf("eth0 addresses = %v", eth.Addresses)
	}
	// An interface with no MAC must omit it, and its addresses must be non-nil
	// (an empty JSON array, not null) to satisfy the required schema field.
	wlan := got.Network[1]
	if wlan.Mac != nil {
		t.Errorf("wlan0 mac = %v, want nil", wlan.Mac)
	}
	if wlan.Addresses == nil {
		t.Error("wlan0 addresses = nil, want empty slice")
	}
}

const (
	ifaceEth0    = "eth0"
	kindWifiTest = "wifi"
)

func TestSystemToWireNetworkKindAndWifi(t *testing.T) {
	t.Parallel()
	sig, freq := -52, 5220
	si := &SystemInfo{Network: []NetworkInterface{
		{Name: "wlan0", Kind: kindWifiTest, Wifi: &WifiLink{SSID: "garden-ap", SignalDBM: &sig, FrequencyMHz: &freq}},
		{Name: "wlan1", Kind: kindWifiTest, Wifi: &WifiLink{SignalDBM: &sig}},
		{Name: "wlan2", Kind: kindWifiTest, Wifi: &WifiLink{}},
		{Name: ifaceEth0, Kind: "ethernet"},
		{Name: "tun0"},
	}}
	got := systemToWire(si).Network

	w := got[0]
	if w.Kind == nil || *w.Kind != mgmtapi.Wifi {
		t.Errorf("wlan0 kind = %v, want wifi", w.Kind)
	}
	if w.Wifi == nil || w.Wifi.Ssid == nil || *w.Wifi.Ssid != "garden-ap" ||
		w.Wifi.SignalDbm == nil || *w.Wifi.SignalDbm != -52 ||
		w.Wifi.FrequencyMhz == nil || *w.Wifi.FrequencyMhz != 5220 {
		t.Errorf("wlan0 wifi = %+v, want all three facts", w.Wifi)
	}
	if w1 := got[1].Wifi; w1 == nil || w1.Ssid != nil || w1.FrequencyMhz != nil || w1.SignalDbm == nil {
		t.Errorf("wlan1 wifi = %+v, want only the signal", w1)
	}
	if got[2].Wifi != nil {
		t.Errorf("wlan2 wifi = %+v, want nil for a link with no known field", got[2].Wifi)
	}
	if got[3].Kind == nil || *got[3].Kind != mgmtapi.Ethernet || got[3].Wifi != nil {
		t.Errorf("eth0 = kind %v wifi %v, want ethernet and no wifi", got[3].Kind, got[3].Wifi)
	}
	if got[4].Kind != nil || got[4].Wifi != nil {
		t.Errorf("tun0 = kind %v wifi %v, want both omitted", got[4].Kind, got[4].Wifi)
	}
}

func TestPostSystemRestartNotImplemented(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.PostSystemRestart(context.Background(), mgmtapi.PostSystemRestartRequestObject{})
	if err != nil {
		t.Fatalf("PostSystemRestart: %v", err)
	}
	p, ok := resp.(mgmtapi.PostSystemRestartdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PostSystemRestart returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestPostSystemRestartAccepted(t *testing.T) {
	restartCalled := make(chan struct{})
	s := New(&fakeProvider{}, WithRestart(func() {
		close(restartCalled)
	}))
	resp, err := s.PostSystemRestart(context.Background(), mgmtapi.PostSystemRestartRequestObject{})
	if err != nil {
		t.Fatalf("PostSystemRestart: %v", err)
	}
	got, ok := resp.(mgmtapi.PostSystemRestart202JSONResponse)
	if !ok {
		t.Fatalf("PostSystemRestart returned %T, want 202 JSON", resp)
	}
	if got.Status != "restarting" {
		t.Errorf("status = %q, want restarting", got.Status)
	}
	select {
	case <-restartCalled:
	case <-time.After(500 * time.Millisecond):
		t.Error("restart function was not called within deadline")
	}
}
