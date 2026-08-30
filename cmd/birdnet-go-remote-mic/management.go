//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// provider adapts the appliance's device records into mgmtserver.Provider. Its
// methods are called from HTTP handler goroutines, so each device's mutable
// state is read under the device's own lock. The record list itself is
// published atomically because the management API starts before the capture
// open loop finishes: until setDevices runs, the API reports zero devices
// (a starting/degraded appliance) rather than racing a growing slice.
type provider struct {
	version    string
	start      time.Time
	rtspListen string
	discovery  bool
	devices    atomic.Pointer[[]*deviceRuntime]
}

var _ mgmtserver.Provider = (*provider)(nil)

// setDevices publishes the final record list once the open loop has built it.
func (p *provider) setDevices(d []*deviceRuntime) { p.devices.Store(&d) }

// deviceList returns the currently published records, or nil before setDevices.
func (p *provider) deviceList() []*deviceRuntime {
	if d := p.devices.Load(); d != nil {
		return *d
	}
	return nil
}

func (p *provider) Version() string { return p.version }

func (p *provider) Status() mgmtserver.ApplianceStatus {
	devices := p.deviceList()
	serving := 0
	for _, d := range devices {
		if d.currentState() == mgmtserver.StateServing {
			serving++
		}
	}
	return mgmtserver.ApplianceStatus{
		Version:          p.version,
		Uptime:           time.Since(p.start),
		RTSPListen:       p.rtspListen,
		DiscoveryEnabled: p.discovery,
		DevicesServing:   serving,
		DevicesTotal:     len(devices),
	}
}

func (p *provider) Devices() []mgmtserver.DeviceStatus {
	devices := p.deviceList()
	out := make([]mgmtserver.DeviceStatus, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.status())
	}
	return out
}

// Device returns one device by name without snapshotting the whole list.
func (p *provider) Device(name string) (mgmtserver.DeviceStatus, bool) {
	for _, d := range p.deviceList() {
		if d.dev.Name == name {
			return d.status(), true
		}
	}
	return mgmtserver.DeviceStatus{}, false
}

// markFailed records that a device's pump died after startup.
func (rt *deviceRuntime) markFailed(err error) {
	rt.mu.Lock()
	rt.state = mgmtserver.StateFailed
	if err != nil {
		rt.err = err.Error()
	}
	rt.mu.Unlock()
}

func (rt *deviceRuntime) currentState() mgmtserver.DeviceState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.state
}

// status snapshots the device's current state for the API. Negotiated values
// are reported only for a device that actually opened (src is non-nil).
func (rt *deviceRuntime) status() mgmtserver.DeviceStatus {
	rt.mu.Lock()
	state, errMsg := rt.state, rt.err
	rt.mu.Unlock()

	ds := mgmtserver.DeviceStatus{
		Config:        rt.dev,
		State:         state,
		Error:         errMsg,
		DroppedFrames: int64(rt.dropped.Load()),
	}
	if rt.src != nil {
		ds.NegotiatedRate = rt.rate
		ds.NegotiatedChannels = rt.channels
	}
	// Only a serving device can hold a client slot. A device that died after
	// startup keeps its track pointer until process exit, and its slot is
	// released asynchronously during teardown, so gate on state to honor the
	// contract's "always false for skipped or failed devices".
	if state == mgmtserver.StateServing && rt.track != nil {
		ds.ClientConnected = rt.track.ClientConnected()
	}
	return ds
}

// mgmt is a running management API's shutdown handle. Wait blocks until the
// HTTP server has drained in-flight connections, so run() can hold process exit
// until the API has shut down cleanly (a prerequisite for a future config PATCH
// that must flush its response before the appliance restarts).
type mgmt struct {
	done chan struct{}
}

// Wait blocks until the management API has finished shutting down. It is safe on
// a nil handle (management disabled) and on a handle whose server never started.
func (m *mgmt) Wait() {
	if m != nil {
		<-m.done
	}
}

// closedMgmt returns a handle that is already done, for the paths where no
// server is running (a cert failure) so callers can Wait unconditionally.
func closedMgmt() *mgmt {
	done := make(chan struct{})
	close(done)
	return &mgmt{done: done}
}

// startManagement generates or loads the self-signed certificate and serves the
// management API over HTTPS in the background until ctx is cancelled. events, if
// non-nil, is mounted as the hand-written SSE handler for GET /events. It reports
// whether the API actually came up: a certificate or listener failure is logged,
// not fatal (the appliance keeps capturing and serving RTSP), but ok is false so
// the caller does not mistake a configured-but-dead API for an available
// diagnostic surface when deciding whether to stay alive with no serving device.
func startManagement(ctx context.Context, cfgPath string, cfg *config.Config, prov *provider, events http.Handler) (handle *mgmt, ok bool) {
	certDir := cfg.Management.CertDir
	if certDir == "" {
		certDir = filepath.Dir(cfgPath)
	}
	certPath := filepath.Join(certDir, "mgmt-cert.pem")
	keyPath := filepath.Join(certDir, "mgmt-key.pem")

	cert, err := mgmtcert.Ensure(certPath, keyPath, certHosts())
	if err != nil {
		log.Printf("management API disabled: cannot prepare TLS certificate: %v", err)
		return closedMgmt(), false
	}

	// Bind synchronously so a listen failure (for example the port already in
	// use) is observed here and reported through ok, rather than being swallowed
	// asynchronously inside the serve goroutine.
	ln, err := net.Listen("tcp", cfg.Management.Listen)
	if err != nil {
		log.Printf("management API disabled: cannot listen on %s: %v", cfg.Management.Listen, err)
		return closedMgmt(), false
	}

	var opts []mgmtserver.Option
	if events != nil {
		opts = append(opts, mgmtserver.WithEventStream(events))
	}

	srv := &http.Server{
		Handler:           mgmtserver.New(prov, opts...).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout bounds slow-client writes on the unauthenticated LAN
		// listener. The /events SSE stream is long-lived and overrides this
		// per-connection with http.ResponseController write deadlines.
		WriteTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		// Serve over the already-bound listener wrapped for TLS from the
		// configured certificate.
		if serr := srv.Serve(tls.NewListener(ln, srv.TLSConfig)); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			log.Printf("management API stopped: %v (RTSP serving continues)", serr)
		}
	}()

	log.Printf("management API on https://%s%s (self-signed cert at %s)", cfg.Management.Listen, mgmtserver.BasePath, certPath)
	log.Print("WARNING: the management API is UNAUTHENTICATED until token auth is configured")
	return &mgmt{done: done}, true
}

// certHosts returns the SANs to embed in the self-signed certificate: loopback,
// this host's name, and its LAN IP addresses, so a client reaching the appliance
// by hostname or by IP does not hit a certificate name mismatch.
func certHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			hosts = append(hosts, ipnet.IP.String())
		}
	}
	return hosts
}
