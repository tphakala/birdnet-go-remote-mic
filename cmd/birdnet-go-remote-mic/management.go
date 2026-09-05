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
	"strings"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
	"github.com/tphakala/birdnet-go-remote-mic/web"
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
	// discovery is the effective mDNS-advertisement flag. It is atomic because a
	// runtime config reload (on the run-loop goroutine) flips it while HTTP
	// handlers read it for GET /status.
	discovery atomic.Bool
	// auth is whether a shared access token is configured. Atomic for the same
	// reason as discovery: the reconcile loop writes it, GET /status reads it.
	auth     atomic.Bool
	dataPath string // filesystem path whose storage usage /system reports
	sampler  *sysinfo.Sampler
	devices  atomic.Pointer[[]*deviceRuntime]
	// detected is the last enumerated set of host capture devices with their
	// probed capabilities, refreshed by the background enumeration goroutine.
	// AvailableDevices filters out the ones the config already lists. Atomic
	// because the enumeration goroutine stores it while HTTP handlers read it for
	// GET /devices/available.
	detected atomic.Pointer[[]audio.DetectedDevice]
	// enumTrigger asks the enumeration goroutine to re-probe now (buffered depth 1,
	// coalescing). A config change signals it so a provisioned or removed device
	// leaves or rejoins the available list promptly, without probing hardware on
	// the capture run-loop goroutine.
	enumTrigger chan struct{}
	// configured is the set of ALSA device ids the desired config owns, published
	// at the START of a reconcile (before any device is opened). The enumeration
	// skips these, so a device being provisioned is excluded from probing before
	// its capture open begins and the two never contend for the same id. Atomic
	// because reconcile stores it while the enumeration goroutine reads it.
	configured atomic.Pointer[map[string]bool]
}

// discoveryEnabled reports the current mDNS-advertisement flag.
func (p *provider) discoveryEnabled() bool { return p.discovery.Load() }

// setDiscovery updates the mDNS-advertisement flag from a config reload.
func (p *provider) setDiscovery(v bool) { p.discovery.Store(v) }

// authRequired reports whether a shared access token is configured.
func (p *provider) authRequired() bool { return p.auth.Load() }

// setAuthRequired records the auth state from a config reload.
func (p *provider) setAuthRequired(v bool) { p.auth.Store(v) }

var (
	_ mgmtserver.Provider       = (*provider)(nil)
	_ mgmtserver.SystemProvider = (*provider)(nil)
)

// System gathers host hardware facts and live metrics for GET /system.
func (p *provider) System() mgmtserver.SystemInfo {
	return sysinfo.Collect(p.dataPath, p.sampler)
}

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
		DiscoveryEnabled: p.discovery.Load(),
		AuthRequired:     p.auth.Load(),
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

// setDetected publishes the latest enumerated host capture devices.
func (p *provider) setDetected(d []audio.DetectedDevice) { p.detected.Store(&d) }

// signalEnumerate asks the enumeration goroutine to re-probe now, coalescing with
// any pending request (the channel has depth 1). It never blocks, so the caller
// (the capture run loop, after a config change) is not stalled by hardware I/O.
func (p *provider) signalEnumerate() {
	select {
	case p.enumTrigger <- struct{}{}:
	default:
	}
}

// setConfiguredIDs publishes the ids the desired config owns, called at the start
// of a reconcile before any device opens.
func (p *provider) setConfiguredIDs(ids map[string]bool) { p.configured.Store(&ids) }

// configuredIDs is the set of ALSA device ids the config owns, so the enumeration
// skips re-probing them (openAndStart already probes configured devices, and a
// device being opened must be excluded before its open begins to avoid contending
// with the probe). It uses the desired set published by reconcile; before the
// first reconcile it falls back to the running device list.
func (p *provider) configuredIDs() map[string]bool {
	if c := p.configured.Load(); c != nil {
		return *c
	}
	list := p.deviceList()
	ids := make(map[string]bool, len(list))
	for _, rt := range list {
		ids[rt.dev.Device] = true
	}
	return ids
}

// DetectedDevice returns the probed capabilities for a host device by id from the
// last enumeration, whether or not it is configured. Provisioning uses it so it
// can tell "no such device on the host" (404) from "already configured" (409),
// which AvailableDevices alone cannot because it hides configured devices.
func (p *provider) DetectedDevice(id string) (mgmtserver.AvailableDevice, bool) {
	if d := p.detected.Load(); d != nil {
		for i := range *d {
			if (*d)[i].ID == id {
				return mgmtserver.AvailableDevice{
					ID:                (*d)[i].ID,
					FriendlyName:      (*d)[i].FriendlyName,
					SupportedRates:    (*d)[i].SupportedRates,
					SupportedChannels: (*d)[i].SupportedChannels,
				}, true
			}
		}
	}
	return mgmtserver.AvailableDevice{}, false
}

// detectDevices enumerates the host's capture hardware, skipping the given ids.
// It is a package var so the enumeration wiring is testable without ALSA.
var detectDevices = audio.DetectDevices

// runEnumeration keeps the available-device list fresh off the capture run-loop
// goroutine: it re-probes the host's unconfigured capture hardware once at
// startup, then on a slow tick (so a hot-plugged device appears) and whenever a
// config change signals enumTrigger (so a provisioned or removed device updates
// promptly). Probing opens hardware and can be slow, which is exactly why it must
// not run on the run loop that also drives capture, reloads and shutdown.
func (p *provider) runEnumeration(ctx context.Context) {
	const interval = 15 * time.Second
	detect := func() {
		det, err := detectDevices(p.configuredIDs())
		if err != nil {
			log.Printf("enumerate available capture devices: %v", err)
			return
		}
		p.setDetected(det)
	}
	detect()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			detect()
		case <-p.enumTrigger:
			detect()
		}
	}
}

// AvailableDevices lists detected host capture devices the config does not list,
// so the UI can offer them for provisioning. It filters the cached enumeration by
// the device ids the running config already owns.
func (p *provider) AvailableDevices() []mgmtserver.AvailableDevice {
	var det []audio.DetectedDevice
	if d := p.detected.Load(); d != nil {
		det = *d
	}
	configured := p.configuredIDs()
	out := make([]mgmtserver.AvailableDevice, 0, len(det))
	for i := range det {
		if configured[det[i].ID] {
			continue
		}
		out = append(out, mgmtserver.AvailableDevice{
			ID:                det[i].ID,
			FriendlyName:      det[i].FriendlyName,
			SupportedRates:    det[i].SupportedRates,
			SupportedChannels: det[i].SupportedChannels,
		})
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
		Config:            rt.dev,
		State:             state,
		Error:             errMsg,
		DroppedFrames:     int64(rt.dropped.Load()),
		FriendlyName:      rt.friendlyName,
		SupportedRates:    rt.supportedRates,
		SupportedChannels: rt.supportedChannels,
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
	// addr is the bound listener address (host:port), read by tests.
	addr string
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
// cfg drives the actual listener binds and certificate location, so serve
// override flags (--mgmt-listen, --cert-dir) take effect for this run. storeCfg
// is the override-free on-disk config that seeds the persistence store, so a
// later PATCH /config never bakes an ephemeral override into config.yaml
// (issue #29).
func startManagement(ctx context.Context, cfgPath string, cfg, storeCfg *config.Config, prov *provider, events http.Handler, restartFn func(), reloader mgmtserver.Reloader, guard *auth.Guard) (handle *mgmt, ok bool) {
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

	opts := []mgmtserver.Option{
		mgmtserver.WithConfigStore(mgmtserver.NewFileConfigStore(cfgPath, storeCfg)),
		mgmtserver.WithSystemInfo(prov),
		mgmtserver.WithRestart(restartFn),
		mgmtserver.WithAuth(guard),
	}
	if reloader != nil {
		opts = append(opts, mgmtserver.WithReloader(reloader))
	}
	if dfs, err := web.DistFS(); err == nil {
		opts = append(opts, mgmtserver.WithStaticAssets(dfs))
	} else {
		log.Printf("web UI assets unavailable: %v (management API still serves JSON endpoints)", err)
	}
	if events != nil {
		opts = append(opts, mgmtserver.WithEventStream(events))
	}

	srv := &http.Server{
		Handler:           mgmtserver.New(prov, opts...).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout bounds slow-client writes on the management listener. The
		// /events SSE stream is long-lived and overrides this per-connection with
		// http.ResponseController write deadlines.
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
	return &mgmt{done: done, addr: ln.Addr().String()}, true
}

// certHosts returns the SANs to embed in the self-signed certificate: loopback,
// this host's name, the <hostname>.local name it advertises over DNS-SD, and its
// LAN IP addresses, so a client reaching the appliance by the discovered .local
// URL, by bare hostname, or by IP does not hit a certificate name mismatch.
func certHosts() []string {
	host, _ := os.Hostname()
	var ips []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			ips = append(ips, ipnet.IP.String())
		}
	}
	return certHostsFor(host, ips)
}

// certHostsFor builds the certificate SANs from the host's name and its
// non-loopback interface IPs. Beyond loopback and the bare hostname it adds
// <hostname>.local, the name brutella/dnssd publishes for the appliance (it sets
// no explicit host), which is the URL an operator naturally has in hand after
// discovery. Without that SAN, https://<host>.local fails verification and the
// operator falls back to curl -k, which disables verification entirely and lets
// anything on the network impersonate the appliance and harvest the bearer
// token. Every name is added at most once, so a hostname that already ends in
// .local yields a single .local SAN rather than a duplicate or a .local.local.
func certHostsFor(hostname string, ifaceIPs []string) []string {
	hosts := make([]string, 0, 4+len(ifaceIPs))
	seen := make(map[string]bool)
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	if hostname != "" {
		add(hostname)
		if !strings.HasSuffix(hostname, ".local") {
			add(hostname + ".local")
		}
	}
	for _, ip := range ifaceIPs {
		add(ip)
	}
	return hosts
}
