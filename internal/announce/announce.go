// Package announce advertises the appliance on the LAN over mDNS / DNS-SD so
// BirdNET-Go can discover it. It advertises only; browsing is the consumer's
// job. The service type is _rtsp._tcp so generic tools (avahi-browse, dns-sd)
// see it too.
package announce

import (
	"context"
	"errors"
	"fmt"
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
	Name     string // instance name; Run adds " #N" to a local duplicate, dnssd renames on a conflict with another host
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
		for n := 2; taken[asciiLower(name)]; n++ {
			suffix := " #" + strconv.Itoa(n)
			name = CutName(infos[i].Name, NameBudget-len(suffix)) + suffix
		}
		taken[asciiLower(name)] = true
		names[i] = name
	}
	return names
}

// asciiLower folds ASCII letters only, as DNS compares names (RFC 4343): two
// names that differ only in the case of a non-ASCII letter are distinct.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// CutName returns s cut to at most n bytes at a rune boundary (nothing when n
// is not positive), with the space left at the cut trimmed. A name that fits
// is returned unchanged.
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

// responderBackoff is the delay before each retry of a responder that failed,
// indexed by consecutive failures; the last entry repeats. A running dnssd
// responder does not stop on its own (a boot before the network is up, or an
// interface change, leaves it running), so what fails is starting it: its
// socket cannot be opened, or registering a service fails (the probe for a
// name gives up). Both can clear, so the start is retried rather than leaving
// the appliance undiscoverable until the next rebuild of its advertisement.
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
	// responderUp is how long a retried responder must run before Run logs it
	// as running again. Registering a service gives up within dnssd's 60 s
	// probe timeout, so one still running after this has registered.
	responderUp = 2 * time.Minute
)

// Run advertises every service until ctx is cancelled, at which point dnssd
// sends goodbye packets to flush peer caches, and returns nil. Names that
// would collide are kept distinct (see distinctNames). A service the records
// cannot describe (no services, a bad name) is an error returned at once, so
// the caller can warn and keep serving without discovery. A responder that
// fails to start (see responderBackoff) is retried on a backoff until ctx is
// cancelled, its failures logged without flooding the log and its recovery
// logged once. The retry reuses the responder once it exists: dnssd has no
// Close, and only a responder that ran to ctx's end releases its socket, so
// building one per attempt would leak a socket each time.
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
	var resp dnssd.Responder
	failures := 0
	for ctx.Err() == nil {
		started := time.Now()
		var up *time.Timer
		if failures > 0 {
			n := failures
			up = time.AfterFunc(responderUp, func() {
				log.Printf("mDNS responder running again after %d failure(s)", n)
			})
		}
		var err error
		resp, err = respond(ctx, resp, srvs)
		if up != nil {
			up.Stop()
		}
		select {
		case <-ctx.Done():
			// Cancelled: whatever the responder returned is its clean stop.
			return nil
		default:
		}
		if errors.Is(err, errAdd) {
			return err
		}
		if time.Since(started) >= responderStable {
			failures = 0
		}
		failures++
		delay := responderBackoff[min(failures, len(responderBackoff))-1]
		if failures <= responderLogFirst || failures%responderLogEvery == 0 {
			log.Printf("mDNS responder could not start: %v; retrying in %s (failure %d)", err, delay, failures)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

// respond runs resp over srvs until ctx is cancelled or it fails, building it
// first when resp is nil, and returns the responder to retry with and why it
// stopped. A responder that was built is kept for the retry, so its socket is
// reused rather than leaked (a Respond called again registers what the failed
// one did not). A responder that returns no error while ctx is still live
// stopped all the same, so that is reported as a failure too.
func respond(ctx context.Context, resp dnssd.Responder, srvs []dnssd.Service) (dnssd.Responder, error) {
	if resp == nil {
		r, err := newResponder()
		if err != nil {
			return nil, err
		}
		for i := range srvs {
			if _, err := r.Add(srvs[i]); err != nil {
				return nil, fmt.Errorf("%w: %w", errAdd, err)
			}
		}
		resp = r
	}
	if err := resp.Respond(ctx); err != nil {
		return resp, err
	}
	return resp, errResponderStopped
}

// errAdd marks a service the responder refused, which no retry can fix: Run
// returns it, as it returns a service the records cannot describe.
var errAdd = errors.New("announce: responder refused a service")

// errResponderStopped reports a responder that returned without an error
// while it was still meant to run.
var errResponderStopped = errors.New("announce: responder stopped")
