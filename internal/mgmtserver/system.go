package mgmtserver

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// restartFlushDelay lets the 202 response flush to the client before the restart
// function (which typically exits the process) runs.
const restartFlushDelay = 100 * time.Millisecond

// NetworkInterface is one host network interface and its live byte counters.
type NetworkInterface struct {
	Name      string
	MAC       string // empty when the interface has no hardware address
	Up        bool
	Addresses []string
	RxBytes   int64
	TxBytes   int64
}

// SystemInfo is the host's hardware facts and live metrics. The optional
// pointer fields are nil when the underlying source is unavailable (for
// example TempCelsius off Raspberry Pi hardware, or CPUPercent before the
// sampler has two readings). Empty strings likewise mean "unknown".
type SystemInfo struct {
	Platform    string
	OS          string
	Kernel      string
	Hostname    string
	CPUModel    string
	CPUCores    int
	CPUPercent  *float64
	MemTotal    int64
	MemUsed     int64
	DiskTotal   int64
	DiskUsed    int64
	TempCelsius *float64
	Network     []NetworkInterface
}

// SystemProvider supplies host system information for GET /system. When no
// provider is mounted, GET /system returns 501. Its method is called from
// handler goroutines and must be safe for concurrent use.
type SystemProvider interface {
	System() SystemInfo
}

// WithSystemInfo mounts sp as the source for GET /system. Without it the
// endpoint returns 501.
func WithSystemInfo(sp SystemProvider) Option {
	return func(s *Server) { s.system = sp }
}

// WithRestart mounts fn as the action invoked when POST /system/restart is requested.
// Without it the endpoint returns 501.
func WithRestart(fn func()) Option {
	return func(s *Server) { s.restartFn = fn }
}

// GetSystem handles GET /system. Without a mounted provider it reports 501.
func (s *Server) GetSystem(_ context.Context, _ mgmtapi.GetSystemRequestObject) (mgmtapi.GetSystemResponseObject, error) {
	if s.system == nil {
		return mgmtapi.GetSystemdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "system information is not available"),
		}, nil
	}
	si := s.system.System()
	return mgmtapi.GetSystem200JSONResponse(systemToWire(&si)), nil
}

// PostSystemRestart handles POST /system/restart. Without a restart function it reports 501.
func (s *Server) PostSystemRestart(_ context.Context, _ mgmtapi.PostSystemRestartRequestObject) (mgmtapi.PostSystemRestartResponseObject, error) {
	if s.restartFn == nil {
		return mgmtapi.PostSystemRestartdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "restart control is not available"),
		}, nil
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("mgmtserver: restart function panicked: %v", r)
			}
		}()
		time.Sleep(restartFlushDelay)
		s.restartFn()
	}()
	return mgmtapi.PostSystemRestart202JSONResponse(mgmtapi.RestartResult{
		Status: "restarting",
	}), nil
}

// systemToWire maps host system info to the generated wire type. Empty optional
// strings become nil so the field is omitted rather than sent as "".
func systemToWire(si *SystemInfo) mgmtapi.SystemInfo {
	nets := make([]mgmtapi.NetworkInterface, 0, len(si.Network))
	for i := range si.Network {
		n := &si.Network[i]
		addrs := n.Addresses
		if addrs == nil {
			addrs = []string{}
		}
		iface := mgmtapi.NetworkInterface{
			Name:      n.Name,
			Up:        n.Up,
			Addresses: addrs,
			RxBytes:   n.RxBytes,
			TxBytes:   n.TxBytes,
		}
		if n.MAC != "" {
			iface.Mac = ptr(n.MAC)
		}
		nets = append(nets, iface)
	}
	out := mgmtapi.SystemInfo{
		Platform:       si.Platform,
		Hostname:       si.Hostname,
		CpuCores:       si.CPUCores,
		CpuPercent:     si.CPUPercent,
		MemTotalBytes:  si.MemTotal,
		MemUsedBytes:   si.MemUsed,
		DiskTotalBytes: si.DiskTotal,
		DiskUsedBytes:  si.DiskUsed,
		TempCelsius:    si.TempCelsius,
		Network:        nets,
	}
	if si.OS != "" {
		out.Os = ptr(si.OS)
	}
	if si.Kernel != "" {
		out.Kernel = ptr(si.Kernel)
	}
	if si.CPUModel != "" {
		out.CpuModel = ptr(si.CPUModel)
	}
	return out
}
