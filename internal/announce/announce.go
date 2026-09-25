// Package announce advertises the appliance on the LAN over mDNS / DNS-SD so
// BirdNET-Go can discover it. It advertises only; browsing is the consumer's
// job. The service type is _rtsp._tcp so generic tools (avahi-browse, dns-sd)
// see it too.
package announce

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brutella/dnssd"
)

// serviceType is the DNS-SD service type advertised.
const serviceType = "_rtsp._tcp"

const (
	// labelMax is the longest DNS label in bytes (RFC 1035 section 2.3.4). A
	// DNS-SD service instance name is one label.
	labelMax = 63
	// renameRoom is the longest suffix the dnssd responder appends when it
	// renames an instance on a conflict with another host: " (N)", where N
	// stays at most 101 because it probes at most 100 times.
	renameRoom = len(" (101)")
	// NameBudget is the longest instance name that still fits one label once
	// the responder renames it. Run keeps the names it makes distinct within
	// it; a caller fitting its names to it leaves them for Run to keep apart.
	NameBudget = labelMax - renameRoom
)

// Info is what the appliance advertises.
type Info struct {
	Name     string // instance name (dnssd renames on conflict)
	Path     string // RTSP path of this stream, e.g. "/stream"
	Port     int    // RTSP port
	Codec    string // "L16" or "opus"
	Rate     int    // sample rate in Hz
	Channels int
	Version  string // binary version
	// AuthRequired is whether the appliance demands its shared token: the TXT
	// record advertises auth=token so BirdNET-Go's planned adopt flow can ask for
	// it, or auth=none for open access.
	AuthRequired bool
}

// txtRecords builds the TXT key/value set advertised with the service. The
// schema is intended for the planned BirdNET-Go adopt flow (txtvers 1).
func txtRecords(info *Info) map[string]string {
	authHint := "none"
	if info.AuthRequired {
		authHint = "token"
	}
	return map[string]string{
		"txtvers": "1",
		"model":   "birdnet-go-remote-mic",
		"version": info.Version,
		"codec":   info.Codec,
		"rate":    strconv.Itoa(info.Rate),
		"ch":      strconv.Itoa(info.Channels),
		"path":    info.Path,
		"auth":    authHint,
	}
}

// distinctNames returns the instance names to advertise, one per info in
// order, with every name kept distinct. The responder renames an instance
// only on a conflict with another host (it registers these services one by
// one before it answers probes), so two local services under one name would
// both be advertised under it and a discoverer could see either. A name
// already taken, compared without ASCII case as DNS compares names, gets the
// first free " #N" suffix from 2 on, the name cut to keep it within
// NameBudget. The suffix is not the responder's own " (N)", which a rename on
// a conflict with another host could then collide with. A name not taken is
// kept as it is.
func distinctNames(infos []Info) []string {
	names := make([]string, len(infos))
	taken := make(map[string]bool, len(infos))
	for i := range infos {
		name := infos[i].Name
		for n := 2; taken[strings.ToLower(name)]; n++ {
			suffix := " #" + strconv.Itoa(n)
			name = CutName(infos[i].Name, NameBudget-len(suffix)) + suffix
		}
		taken[strings.ToLower(name)] = true
		names[i] = name
	}
	return names
}

// CutName returns s cut to at most n bytes at a rune boundary (nothing when n
// is not positive), without a trailing space.
func CutName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ")
}

// newResponder builds the mDNS responder; a test seam, dnssd.NewResponder in
// production.
var newResponder = dnssd.NewResponder

// responderBackoff is the delay before each restart of a responder that
// failed, indexed by consecutive failures; the last entry repeats. A responder
// fails when the host has no usable interface yet (a boot before the network
// is up) or its interfaces change under it, both of which fix themselves, so
// it is retried rather than leaving the appliance undiscoverable until the
// next rebuild of its advertisement.
var responderBackoff = [...]time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

const (
	// responderStable is how long a responder must run before a failure starts
	// the backoff over from the shortest delay.
	responderStable = 10 * time.Minute
	// responderLogFirst and responderLogEvery bound the failure logging: the
	// first failures of an outage are logged, then every responderLogEvery-th,
	// about once an hour at the capped delay.
	responderLogFirst = 3
	responderLogEvery = 12
)

// Run advertises every service until ctx is cancelled, at which point dnssd
// sends goodbye packets to flush peer caches, and returns nil. Names that
// would collide are kept distinct (see distinctNames). A service the records
// cannot describe (no services, a bad name) is an error returned at once, so
// the caller can warn and keep serving without discovery. A responder that
// cannot start, or stops, is restarted on a backoff until ctx is cancelled,
// its failures logged without flooding the log, so a network that comes up
// late or changes does not leave the appliance undiscoverable.
//
// Known limitation: a device that dies mid-run keeps its advertisement until
// the caller rebuilds it; clients that discover it get a 404 from the RTSP
// server.
func Run(ctx context.Context, infos []Info) error {
	if len(infos) == 0 {
		return errors.New("announce: no services to advertise")
	}
	names := distinctNames(infos)
	srvs := make([]dnssd.Service, len(infos))
	for i := range infos {
		srv, err := dnssd.NewService(dnssd.Config{
			Name: names[i],
			Type: serviceType,
			Port: infos[i].Port,
			Text: txtRecords(&infos[i]),
		})
		if err != nil {
			return err
		}
		srvs[i] = srv
	}
	attempts, failures := 0, 0
	for {
		started := time.Now()
		err := respond(ctx, srvs)
		select {
		case <-ctx.Done():
			// Cancelled: whatever the responder returned is its clean stop.
			return nil
		default:
		}
		if time.Since(started) >= responderStable {
			attempts, failures = 0, 0
		}
		attempts++
		failures++
		delay := responderBackoff[min(attempts, len(responderBackoff))-1]
		if failures <= responderLogFirst || failures%responderLogEvery == 0 {
			log.Printf("mDNS responder stopped: %v; restarting it in %s (failure %d)", err, delay, failures)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// respond runs one responder over srvs until ctx is cancelled or it fails,
// returning why it stopped. A responder that returns no error while ctx is
// still live stopped all the same, so that is reported as a failure too.
func respond(ctx context.Context, srvs []dnssd.Service) error {
	resp, err := newResponder()
	if err != nil {
		return err
	}
	for i := range srvs {
		if _, err := resp.Add(srvs[i]); err != nil {
			return err
		}
	}
	if err := resp.Respond(ctx); err != nil {
		return err
	}
	return errResponderStopped
}

// errResponderStopped reports a responder that returned without an error
// while it was still meant to run.
var errResponderStopped = errors.New("announce: responder stopped")
