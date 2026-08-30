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
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// provider adapts the appliance's device records into mgmtserver.Provider. Its
// methods are called from HTTP handler goroutines, so each device's mutable
// state is read under the device's own lock.
type provider struct {
	version    string
	start      time.Time
	rtspListen string
	discovery  bool
	devices    []*deviceRuntime
}

var _ mgmtserver.Provider = (*provider)(nil)

func (p *provider) Version() string { return p.version }

func (p *provider) Status() mgmtserver.ApplianceStatus {
	serving := 0
	for _, d := range p.devices {
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
		DevicesTotal:     len(p.devices),
	}
}

func (p *provider) Devices() []mgmtserver.DeviceStatus {
	out := make([]mgmtserver.DeviceStatus, 0, len(p.devices))
	for _, d := range p.devices {
		out = append(out, d.status())
	}
	return out
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

// startManagement generates or loads the self-signed certificate and serves the
// management API over HTTPS in the background until ctx is cancelled. Failure to
// start is logged, not fatal: the appliance keeps capturing and serving RTSP.
func startManagement(ctx context.Context, cfgPath string, cfg *config.Config, prov *provider) {
	certDir := cfg.Management.CertDir
	if certDir == "" {
		certDir = filepath.Dir(cfgPath)
	}
	certPath := filepath.Join(certDir, "mgmt-cert.pem")
	keyPath := filepath.Join(certDir, "mgmt-key.pem")

	cert, err := mgmtcert.Ensure(certPath, keyPath, certHosts())
	if err != nil {
		log.Printf("management API disabled: cannot prepare TLS certificate: %v", err)
		return
	}

	srv := &http.Server{
		Addr:              cfg.Management.Listen,
		Handler:           mgmtserver.New(prov).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		// ListenAndServeTLS with empty paths uses TLSConfig.Certificates.
		if serr := srv.ListenAndServeTLS("", ""); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			log.Printf("management API stopped: %v (RTSP serving continues)", serr)
		}
	}()

	log.Printf("management API on https://%s%s (self-signed cert at %s)", cfg.Management.Listen, mgmtserver.BasePath, certPath)
	log.Print("WARNING: the management API is UNAUTHENTICATED until token auth is configured")
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
