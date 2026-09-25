//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
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
	dataPath string // filesystem path whose storage usage /system and the host-health disk check report
	// cpu reports host CPU utilization for GET /system, read only when a
	// request asks (see sysinfo.CPUGauge), so an appliance with no browser open
	// reads nothing. The host-health monitor diffs /proc/stat over its own poll
	// window instead. nil when the API is off, which sysinfo.Collect tolerates
	// by omitting CPUPercent.
	cpu     *sysinfo.CPUGauge
	devices atomic.Pointer[[]*deviceRuntime]
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
	// configured is the set of ids the desired config owns, including the stable
	// ids its entries resolved to (so a card-index entry also hides the stable id
	// of the hardware it names), published at the START of a reconcile (before any
	// device is opened). The enumeration skips these, so a device being
	// provisioned is excluded from probing before its capture open begins and the
	// two never contend for the same id. Atomic because reconcile stores it while
	// the enumeration goroutine reads it.
	configured atomic.Pointer[map[string]bool]
	// hwChanged tells the run loop to retry devices that are down (buffered depth
	// 1, coalescing), either because the host's capture hardware changed since the
	// previous enumeration (a device was plugged, unplugged, or renumbered) or
	// because a capture pump died when its device was lost and armed a retry (see
	// retryArmed), which covers a device unplugged and replugged at the same card
	// index within one enumeration tick. The enumeration goroutine sends without
	// blocking and never waits on the run loop, so it cannot deadlock against a
	// reconcile. A nil channel (tests that wire none) drops the signal.
	hwChanged chan struct{}
	// retryArmed is set by onPumpDone when a capture pump dies because its device
	// was lost (unplugged or powered off). The next enumeration signals hwChanged
	// even when the hardware signature is unchanged, then clears the flag with
	// Swap, so one lost-device failure arms exactly one retry. A pump that dies
	// while its device is still present does not arm this enumeration retry, since
	// re-arming a persistent non-hardware fault would restart it every tick; it
	// is retried on a backoff instead (see scheduleRetry), unless it is bound by
	// a card-index id, which only a config save restarts. Atomic because
	// onPumpDone (run loop) writes it while the enumeration goroutine reads and
	// clears it.
	retryArmed atomic.Bool
	// overrides names the config fields a serve CLI flag overrode for this run,
	// as a startup snapshot of the running-vs-persisted divergence. It is set once
	// before the API starts serving and never mutated, so a plain field read from
	// handler goroutines is safe. Empty when no serve override is active.
	overrides []mgmtserver.ConfigOverride
	// cert holds the management listener's current certificate: the metadata for
	// the certificate endpoints, the public PEM, and the parsed pair the TLS
	// GetCertificate callback serves. It is one atomic pointer to an immutable
	// snapshot, so a handler read and a TLS handshake never observe a mix of two
	// certificates. An operator regenerate or install swaps it under certMu, which
	// serializes the persist-then-swap so two writers cannot interleave.
	cert   atomic.Pointer[certState]
	certMu sync.Mutex
	// certPath and keyPath are where the certificate pair is persisted, set
	// under certMu by useConfig before each attempt to bring the API up.
	// Regenerate and Install write here (and the pin marker derived from
	// certPath) before swapping the live certificate.
	certPath string
	keyPath  string
}

// certState is one immutable certificate snapshot the provider publishes through
// its atomic pointer. Its fields are never mutated after Store: a rotation builds
// a fresh certState and swaps the pointer, so a reader keeps a consistent view.
type certState struct {
	info mgmtserver.CertificateInfo
	pem  []byte
	tls  *tls.Certificate
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
	_ mgmtserver.CertProvider   = (*provider)(nil)
	_ mgmtserver.CertManager    = (*provider)(nil)
)

// System gathers host hardware facts and live metrics for GET /system.
func (p *provider) System() mgmtserver.SystemInfo {
	return sysinfo.Collect(p.dataPath, p.cpu)
}

// setCertificate describes cert, records its public metadata and chain PEM, and
// publishes the new snapshot through the atomic pointer. It sets Managed from the
// pin marker (an operator-installed certificate is pinned). It publishes ONLY on
// success: if the metadata cannot be described or the chain cannot be encoded it
// returns the error and leaves the previous snapshot in place, so a runtime
// rotation error never swaps the live certificate to one with blank metadata.
// The startup caller installs a raw-certificate fallback separately (see
// prepareCertificate) so a describe failure at boot still leaves TLS serving.
// Its callers in the appliance hold certMu around it so the persisted pair and
// the published snapshot stay in step.
func (p *provider) setCertificate(cert *tls.Certificate) error {
	info, err := mgmtcert.Describe(cert)
	if err != nil {
		return err
	}
	pemBytes, err := mgmtcert.ChainPEM(cert)
	if err != nil {
		return err
	}
	ci := toCertInfo(&info)
	ci.Managed = !mgmtcert.Pinned(p.certPath)
	p.cert.Store(&certState{info: ci, pem: pemBytes, tls: cert})
	return nil
}

// Certificate returns the management listener's certificate metadata for
// GET /system/certificate. The SAN slices are copied so a caller cannot mutate
// the shared stored value. It returns the zero value before setCertificate runs.
func (p *provider) Certificate() mgmtserver.CertificateInfo {
	st := p.cert.Load()
	if st == nil {
		return mgmtserver.CertificateInfo{}
	}
	ci := st.info
	ci.DNSNames = append([]string(nil), st.info.DNSNames...)
	ci.IPAddresses = append([]string(nil), st.info.IPAddresses...)
	return ci
}

// CertificatePEM returns the PEM-encoded public certificate for
// GET /system/certificate/pem. It never returns the private key, and returns a
// copy so a caller cannot mutate the shared stored bytes.
func (p *provider) CertificatePEM() []byte {
	st := p.cert.Load()
	if st == nil {
		return nil
	}
	return append([]byte(nil), st.pem...)
}

// tlsCertificate is the management listener's tls.Config.GetCertificate callback:
// it returns the current certificate for every new handshake, so a regenerate or
// install applies to new connections without a restart. Connections already open
// keep the certificate they handshook with. It never returns (nil, nil), which
// crypto/tls would treat as "no certificate available" and fail the handshake
// with an opaque error.
func (p *provider) tlsCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	st := p.cert.Load()
	if st == nil {
		return nil, errors.New("management certificate not loaded")
	}
	return st.tls, nil
}

// Regenerate replaces the certificate with a fresh self-signed one covering the
// auto-detected names plus extraSANs, persists it, clears any operator pin, and
// swaps it into the live listener for new connections. It validates extraSANs
// first, returning a *mgmtcert.ValidationError for a bad entry without touching
// the certificate.
func (p *provider) Regenerate(extraSANs []string) (mgmtserver.CertificateInfo, error) {
	if err := mgmtcert.ValidateHosts(extraSANs); err != nil {
		return mgmtserver.CertificateInfo{}, err
	}
	p.certMu.Lock()
	defer p.certMu.Unlock()
	hosts := appendUniqueHosts(certHosts(), extraSANs)
	cert, err := mgmtcert.Regenerate(p.certPath, p.keyPath, hosts)
	if err != nil {
		return mgmtserver.CertificateInfo{}, err
	}
	if err := p.setCertificate(&cert); err != nil {
		return mgmtserver.CertificateInfo{}, err
	}
	log.Printf("management certificate regenerated (%d SANs, fingerprint %s)", len(hosts), p.cert.Load().info.FingerprintSHA256)
	return p.Certificate(), nil
}

// Install replaces the certificate with the operator-supplied pair after
// validation, persists it, pins it so the appliance never regenerates it, and
// swaps it into the live listener for new connections. It returns a
// *mgmtcert.ValidationError when the pair is rejected. The private key is never
// logged or returned.
func (p *provider) Install(certPEM, keyPEM []byte) (mgmtserver.CertificateInfo, error) {
	p.certMu.Lock()
	defer p.certMu.Unlock()
	cert, err := mgmtcert.Install(p.certPath, p.keyPath, certPEM, keyPEM)
	if err != nil {
		return mgmtserver.CertificateInfo{}, err
	}
	if err := p.setCertificate(&cert); err != nil {
		return mgmtserver.CertificateInfo{}, err
	}
	st := p.cert.Load()
	log.Printf("management certificate installed (subject %q, fingerprint %s)", st.info.Subject, st.info.FingerprintSHA256)
	return p.Certificate(), nil
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
		DevicesServing:   serving,
		DevicesTotal:     len(devices),
		Overrides:        p.overrides,
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

// signalHardwareChanged asks the run loop to retry devices that are down,
// coalescing with a pending signal. It never blocks. The trigger is either a
// host hardware change or a pump that died because its device was lost and
// armed a retry.
func (p *provider) signalHardwareChanged() {
	select {
	case p.hwChanged <- struct{}{}:
	default:
	}
}

// armRetry records that a capture pump died because its device was lost, so the
// next enumeration signals hwChanged and the run loop retries devices that are
// down even if the host's hardware signature is unchanged (a device unplugged
// and replugged at the same card index within one tick). runEnumeration clears
// the flag with Swap, so one such failure arms exactly one retry. onPumpDone
// does not call it for a pump that died while its device was still present, so a
// persistent non-hardware fault does not retry-flap.
func (p *provider) armRetry() { p.retryArmed.Store(true) }

// hardwareSignature summarises which devices the host exposes and where, so two
// enumerations can be compared for a plug, unplug, or renumbering. It includes
// the current-boot address, so a device that moved to another card index
// changes the signature even though its stable id did not.
func hardwareSignature(det []audio.DetectedDevice) string {
	parts := make([]string, 0, len(det))
	for i := range det {
		parts = append(parts, det[i].ID+"="+det[i].HWAddr)
	}
	slices.Sort(parts)
	return strings.Join(parts, "\n")
}

// configuredIDs is the set of ids the config owns, including the stable ids its
// entries resolved to, so the enumeration skips re-probing them (openAndStart
// already probes configured devices, and a device being opened must be excluded
// before its open begins to avoid contending with the probe). It uses the desired
// set published by reconcile; before the first reconcile it falls back to the
// configured ids of the running device list (no resolved stable ids yet).
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
					HWAddr:            (*d)[i].HWAddr,
					IDStable:          (*d)[i].IDStable,
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
//
// It also detects host hardware changes and signals hwChanged so the run loop
// retries devices that are down. It signals when the hardware signature changed
// since the previous enumeration (which includes the first enumeration whenever
// the host exposes any device, so a mic that finished enumerating between the
// initial reconcile and this probe is retried) or when a pump died because its
// device was lost and armed a retry (retryArmed) even though the signature is
// unchanged (a device unplugged and replugged at the same card index within one
// tick).
func (p *provider) runEnumeration(ctx context.Context) {
	const interval = 15 * time.Second
	// last is the previous enumeration's hardware signature; it starts empty, so
	// the first enumeration signals whenever the host exposes any device.
	var last string
	// first distinguishes the startup enumeration: last starts empty, so the first
	// pass reads as "changed" whenever the host exposes any device, though nothing
	// actually changed. It only kicks the startup retry, so it is logged as an
	// initial enumeration rather than as a hardware change that never happened.
	first := true
	detect := func() {
		det, err := detectDevices(p.configuredIDs())
		if err != nil {
			log.Printf("enumerate available capture devices: %v", err)
			return
		}
		p.setDetected(det)
		sig := hardwareSignature(det)
		changed := sig != last
		last = sig
		wasFirst := first
		first = false
		// Consume the armed flag every time (evaluate it, do not let a changed
		// signature short-circuit it away), so a lost-device pump failure arms
		// exactly one retry.
		armed := p.retryArmed.Swap(false)
		if changed || armed {
			var reason string
			switch {
			case wasFirst:
				reason = "initial capture hardware enumeration"
			case !changed:
				reason = "a capture device was lost"
			default:
				reason = "capture hardware changed"
			}
			log.Printf("%s (%d device(s) present); retrying devices that are down", reason, len(det))
			p.signalHardwareChanged()
		}
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
			HWAddr:            det[i].HWAddr,
			IDStable:          det[i].IDStable,
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

// markFailed records that a device's pump died after startup. It does not touch
// hwAddr: that field is static per record (set once before the record is
// published, then read lock-free), so the stale-address suppression for a failed
// device is done at read time in status() instead, which keeps the field
// immutable-after-publish and free of a data race with the API readers.
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

	// A device that died after startup is no longer capturing at its last-known
	// address, and the kernel can reassign that card index to another device before
	// this entry is retried, so a failed device does not report its stale hw:N,D
	// (which may now name different hardware). hwAddr stays set on the record (it is
	// static per record and read lock-free here alongside friendlyName); this hides
	// it only in the failed view, so a later successful retry surfaces it again.
	hwAddr := rt.hwAddr
	if state == mgmtserver.StateFailed {
		hwAddr = ""
	}

	ds := mgmtserver.DeviceStatus{
		Config:            rt.dev,
		State:             state,
		Error:             errMsg,
		DroppedFrames:     int64(rt.droppedTotal()),
		FriendlyName:      rt.friendlyName,
		HWAddr:            hwAddr,
		IDStable:          !config.IsCardIndexID(rt.dev.Device),
		SupportedRates:    rt.supportedRates,
		SupportedChannels: rt.supportedChannels,
	}
	if rt.src != nil {
		ds.NegotiatedRate = rt.rate
		ds.NegotiatedChannels = rt.channels
		ds.NegotiatedFormat = rt.format
	}
	// Only a serving device can hold a client slot. A device that died after
	// startup keeps its track pointers until process exit, and slots are released
	// asynchronously during teardown, so gate on state to honor the contract's
	// "always false for skipped or failed devices". The device-level
	// ClientConnected is true when ANY stream has a client; each stream's own state
	// is reported per stream, and DroppedFrames per stream sums the same way the
	// device figure does.
	if state == mgmtserver.StateServing {
		ds.Streams = make([]mgmtserver.StreamStatus, 0, len(rt.streams))
		for _, sr := range rt.streams {
			connected := sr.track != nil && sr.track.ClientConnected()
			if connected {
				ds.ClientConnected = true
			}
			ds.Streams = append(ds.Streams, mgmtserver.StreamStatus{
				Path:            sr.stream.Path,
				ClientConnected: connected,
				DroppedFrames:   int64(sr.dropped.Load()),
			})
		}
	}
	return ds
}

// mgmt is the management API's lifecycle handle. It exists for the whole run
// once management is enabled, whether or not an API is serving: a supervisor
// goroutine (see superviseManagement) owns the API, brings it back when it
// fails to start or stops on its own, and records the serving API (nil while
// none serves) for run()'s exit decisions. Wait blocks until the supervisor has
// stopped and the last API has drained in-flight connections, so run() can hold
// process exit until the API has shut down cleanly.
type mgmt struct {
	cur  atomic.Pointer[mgmtServer]
	done chan struct{}
}

// mgmtServer is one serving management API.
type mgmtServer struct {
	mgmtEndpoint
	// store is the API's persistence store, whose config (including every PATCH
	// since the API came up) seeds the next API if this one stops.
	store *mgmtserver.FileConfigStore
	// ln is the bound listener. Closing it stops the API as a runtime listener
	// fault would, which tests use to drive the recovery.
	ln      net.Listener
	stopped chan struct{}
	// err is why the API stopped, set before stopped closes: nil after a
	// shutdown on ctx, the serve error when the listener failed on its own.
	err error
}

// wait blocks until the API has shut down and reports why it stopped: nil when
// ctx was cancelled, the serve error when the API died at runtime.
func (s *mgmtServer) wait() error {
	<-s.stopped
	return s.err
}

// mgmtEndpoint is where a serving management API listens, as published in the
// run lock (see runLockPublisher) for the token commands.
type mgmtEndpoint struct {
	// addr is the bound listener address (host:port).
	addr string
	// certPath is the PEM certificate the listener serves, so a token command
	// can pin it.
	certPath string
}

// Wait blocks until the management API has finished shutting down. It returns at
// once on a nil handle (management disabled); otherwise it returns once ctx is
// cancelled and the supervisor, and any API it runs, has stopped, or once the
// supervisor gave up because the config file disabled management.
func (m *mgmt) Wait() {
	if m != nil {
		<-m.done
	}
}

// serving returns the API serving now, or nil when none serves (it failed to
// start, stopped at runtime and is being retried, or the handle is nil). run()
// reads it for its exit decisions: only a serving API keeps an appliance with
// no serving device up as a diagnostic surface.
func (m *mgmt) serving() *mgmtServer {
	if m == nil {
		return nil
	}
	return m.cur.Load()
}

// mgmtParams carries what serveManagement needs to bring the API up, so a
// background attempt can repeat it. After startManagement returns, only the
// supervisor goroutine touches it.
type mgmtParams struct {
	cfgPath string
	// cfg drives the listener bind and certificate location, so the serve
	// override flags (--mgmt-listen, --cert-dir) take effect for the run. A
	// background attempt replaces it with the reloaded config plus the
	// overrides (see useConfig).
	cfg *config.Config
	// storeCfg is the override-free on-disk config that seeds the persistence
	// store, so a later PATCH /config never bakes an ephemeral override into
	// config.yaml (issue #29).
	storeCfg *config.Config
	// overrides are the serve flags, re-applied to the config a background
	// attempt reloads, as the serve reloader does on every PATCH.
	overrides serveOverrides
	prov      *provider
	events    http.Handler
	center    *notify.Center
	restartFn func()
	reloader  mgmtserver.Reloader
	guard     *auth.Guard
	// runLock publishes where the API listens, or that none serves; nil
	// publishes nothing.
	runLock  *runLockPublisher
	certPath string
	keyPath  string
}

// useConfig makes running (a config with the serve overrides applied) the one
// the next serveManagement binds and reads the certificate from, and publishes
// the certificate paths to the provider. setCertificate reads certPath to
// decide the Managed flag (a pin marker sits beside it), and Regenerate/Install
// write there, so the paths are published before the certificate is prepared.
// No API serves while this runs, but a handler of the previous one can outlive
// its forced Close (see serveManagement), so the paths are written under
// certMu, which Regenerate and Install hold while they read them.
func (p *mgmtParams) useConfig(running *config.Config) {
	p.cfg = running
	certDir := running.Management.CertDir
	if certDir == "" {
		certDir = filepath.Dir(p.cfgPath)
	}
	p.certPath = filepath.Join(certDir, "mgmt-cert.pem")
	p.keyPath = filepath.Join(certDir, "mgmt-key.pem")
	p.prov.certMu.Lock()
	p.prov.certPath = p.certPath
	p.prov.keyPath = p.keyPath
	p.prov.certMu.Unlock()
}

// startManagement generates or loads the self-signed certificate and serves the
// management API over HTTPS in the background until ctx is cancelled. p.events,
// if non-nil, is mounted as the hand-written SSE handler for GET /events. It
// reports whether the API actually came up. A certificate or listener failure
// (including an installed certificate it cannot read, which it never
// overwrites) is logged, not fatal (the appliance keeps capturing and serving
// RTSP), and ok is false so the caller does not mistake a configured-but-dead
// API for an available diagnostic surface when deciding whether to stay alive
// with no serving device. Either way the handle's supervisor keeps the API up
// from then on, retrying a failed start and restarting an API that stops at
// runtime (see superviseManagement); the handle's serving method follows it.
func startManagement(ctx context.Context, p *mgmtParams) (handle *mgmt, ok bool) {
	return startManagementWith(ctx, p, mgmtRetryBackoff[:])
}

// startManagementWith is startManagement with the retry delays as a parameter,
// so tests can retry without waiting out the real backoff. A failed start also
// raises the management-unavailable notification, which a successful attempt
// clears. A serving API publishes its endpoint in the run lock.
func startManagementWith(ctx context.Context, p *mgmtParams, delays []time.Duration) (handle *mgmt, ok bool) {
	p.useConfig(p.cfg)
	m := &mgmt{done: make(chan struct{})}
	srv, err := serveManagement(ctx, p)
	if err == nil {
		m.cur.Store(srv)
		p.runLock.publish(&srv.mgmtEndpoint)
	} else {
		log.Printf("management API unavailable: %v (retrying in the background)", err)
		p.onsetDown(fmt.Sprintf("The web UI and API could not start: %v; retrying in the background", err))
	}
	go superviseManagement(ctx, m, srv, err, mgmtRetry{
		attempt: func() (*mgmtServer, error) { return recoverManagement(ctx, p) },
		died:    p.died,
		delays:  delays,
		stable:  delays[len(delays)-1],
	})
	return m, err == nil
}

// onsetDown raises the management-unavailable condition with msg. It is raised
// at once so the outage is in the history the UI shows once the API is back;
// a successful attempt clears it. A nil center is a no-op.
func (p *mgmtParams) onsetDown(msg string) {
	p.center.Onset(notify.Notification{
		Severity: notify.SeverityError,
		Category: notify.CategorySystem,
		Key:      mgmtDownKey,
		Source:   "management",
		Title:    "Management API unavailable",
		Message:  msg,
	})
}

// died handles an API that stopped on its own (its listener failed), once the
// server has drained: the next attempt seeds its store from this API's config,
// which carries every PATCH since it came up; the run lock stops advertising
// the dead address, so the token commands edit the file again; and the outage
// is raised.
func (p *mgmtParams) died(s *mgmtServer, err error) {
	log.Printf("management API stopped: %v (restarting it in the background; RTSP serving continues)", err)
	cfg := s.store.Config()
	p.storeCfg = &cfg
	p.runLock.publish(nil)
	p.onsetDown(fmt.Sprintf("The web UI and API stopped: %v; restarting in the background", err))
}

// prepareCertificate loads or creates the certificate pair at the configured
// paths and publishes it as the snapshot the TLS GetCertificate callback
// serves and the certificate endpoints read. It holds certMu, as Regenerate
// and Install do, because a handler of a previous API can outlive its forced
// Close and write the same files. setCertificate publishes only on success; if
// the metadata cannot be described, a raw-certificate fallback still gives the
// listener a certificate to present (the API stays up) while the certificate
// endpoints stay unmounted and return 501, which mounted reports. A metadata
// fault never takes the appliance down.
func (p *mgmtParams) prepareCertificate() (mounted bool, err error) {
	p.prov.certMu.Lock()
	defer p.prov.certMu.Unlock()
	cert, err := mgmtcert.Ensure(p.certPath, p.keyPath, certHosts())
	if err != nil {
		return false, fmt.Errorf("cannot prepare TLS certificate: %w", err)
	}
	if serr := p.prov.setCertificate(&cert); serr != nil {
		log.Printf("management certificate metadata unavailable: %v (certificate endpoints disabled)", serr)
		p.prov.cert.Store(&certState{tls: &cert})
		return false, nil
	}
	return true, nil
}

// serveManagement makes one attempt to bring the API up: prepare the
// certificate, bind the listener, and serve until ctx is cancelled or the
// listener fails, which the returned server's wait reports.
//
// An installed (pinned) certificate that exists but cannot be read comes back as
// *mgmtcert.PinnedReadError and fails the attempt: the API stays off rather than
// regenerating over the operator's certificate, and the token CLI falls back to
// editing the config file because no API address is published. Serving a
// throwaway certificate instead would leave the token CLI pinning a file the
// listener does not present.
func serveManagement(ctx context.Context, p *mgmtParams) (*mgmtServer, error) {
	certMounted, err := p.prepareCertificate()
	if err != nil {
		return nil, err
	}

	// Bind synchronously so a listen failure (for example the port already in
	// use) is observed here and reported, rather than being swallowed
	// asynchronously inside the serve goroutine.
	ln, err := net.Listen("tcp", p.cfg.Management.Listen)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", p.cfg.Management.Listen, err)
	}

	store := mgmtserver.NewFileConfigStore(p.cfgPath, p.storeCfg)
	opts := []mgmtserver.Option{
		mgmtserver.WithConfigStore(store),
		mgmtserver.WithSystemInfo(p.prov),
		mgmtserver.WithRestart(p.restartFn),
		mgmtserver.WithAuth(p.guard),
		mgmtserver.WithChannelProbe(probeChannelLevels),
	}
	if certMounted {
		opts = append(opts, mgmtserver.WithCertificateManager(p.prov))
	}
	if p.reloader != nil {
		opts = append(opts, mgmtserver.WithReloader(p.reloader))
	}
	if dfs, err := web.DistFS(); err == nil {
		opts = append(opts, mgmtserver.WithStaticAssets(dfs))
	} else {
		log.Printf("web UI assets unavailable: %v (management API still serves JSON endpoints)", err)
	}
	if p.events != nil {
		opts = append(opts, mgmtserver.WithEventStream(p.events))
	}
	// The notification center is both the snapshot source for GET /notifications
	// and the publisher the management-side emitters (config reload, restart,
	// auth change) write to, so wire both faces from the one object.
	if p.center != nil {
		opts = append(opts, mgmtserver.WithNotifications(p.center), mgmtserver.WithNotifier(p.center))
	}

	srv := &http.Server{
		Handler:           mgmtserver.New(p.prov, opts...).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout bounds slow-client writes on the management listener. The
		// /events SSE stream is long-lived and overrides this per-connection with
		// http.ResponseController write deadlines.
		WriteTimeout: 30 * time.Second,
		// A client that does not trust the appliance's self-signed certificate
		// rejects the handshake and reconnects, and net/http's default logger
		// would emit one "http: TLS handshake error ... remote error: tls:" line
		// per attempt. Filter that client-rejection spam while passing every
		// other server error (including a local TLS misconfiguration) through.
		ErrorLog: mgmtserver.NewFilteredErrorLog(log.Default()),
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: p.prov.tlsCertificate,
			// Disable TLS session resumption so a certificate swap (regenerate or
			// install) reaches every connection. A resumed session would keep the
			// certificate context established before the swap, and GetCertificate is
			// not consulted for it. The management listener is low-traffic, so the
			// lost resumption is negligible.
			SessionTicketsDisabled: true,
		},
	}

	s := &mgmtServer{
		mgmtEndpoint: mgmtEndpoint{addr: ln.Addr().String(), certPath: p.certPath},
		store:        store,
		ln:           ln,
		stopped:      make(chan struct{}),
	}
	died := make(chan error, 1)
	go func() {
		defer close(s.stopped)
		select {
		case <-ctx.Done():
		case s.err = <-died:
		}
		// Drain on either path, after a runtime fault too, before the
		// supervisor brings up the next API. Shutdown waits for connections to
		// go idle; an open /events stream never does, so after 5 s Close cuts
		// the rest without waiting for their handlers to return.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
	}()

	go func() {
		// Serve over the already-bound listener wrapped for TLS from the provider's
		// current certificate (via TLSConfig.GetCertificate), so an operator
		// regenerate or install reaches new connections without a restart. Serve
		// closes the listener when it returns, so a restart can bind the port.
		if serr := srv.Serve(tls.NewListener(ln, srv.TLSConfig)); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			died <- serr
		}
	}()

	log.Printf("management API on https://%s%s (certificate at %s)", p.cfg.Management.Listen, mgmtserver.BasePath, p.certPath)
	return s, nil
}

// toCertInfo adapts the mgmtcert metadata into the mgmtserver domain type, so
// mgmtcert stays free of any dependency on the HTTP-server package. It takes info
// by pointer because mgmtcert.Info is a large value.
func toCertInfo(info *mgmtcert.Info) mgmtserver.CertificateInfo {
	return mgmtserver.CertificateInfo{
		Subject:           info.Subject,
		Issuer:            info.Issuer,
		SelfSigned:        info.SelfSigned,
		DNSNames:          info.DNSNames,
		IPAddresses:       info.IPAddresses,
		NotBefore:         info.NotBefore,
		NotAfter:          info.NotAfter,
		FingerprintSHA256: info.FingerprintSHA256,
	}
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

// appendUniqueHosts returns base followed by the entries of extra not already
// present, preserving order and dropping duplicates and blanks. Regenerate uses
// it to add operator-supplied SANs on top of the auto-detected set without
// repeating a name the detection already found.
func appendUniqueHosts(base, extra []string) []string {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for _, h := range base {
		add(h)
	}
	for _, h := range extra {
		add(h)
	}
	return out
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
