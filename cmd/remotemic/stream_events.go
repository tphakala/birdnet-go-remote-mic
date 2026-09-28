//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
)

// Flap thresholds for a repeatedly reconnecting client. BirdNET-Go retries a
// dead upstream with backoff, so a broken path produces a connect/disconnect
// pair every few seconds; more than flapMax connects within flapWindow raises
// one warning and suppresses the per-connection infos for that path until
// flapQuiet passes with no reconnect. These describe protocol behaviour, not a
// per-site condition, so they are constants rather than configurable thresholds.
const (
	flapMax    = 3
	flapWindow = 60 * time.Second
	flapQuiet  = 5 * time.Minute
	// flapSweepInterval is how often Run ages out flap warnings whose client has
	// settled or gone away. Flap.Event only clears on the next connect after the
	// quiet window, and a client that recovers into a steady connection (its
	// reconnect lands inside the quiet window and is suppressed) or gives up
	// entirely sends no such connect, so without this sweep the warning would stay
	// pinned. The interval only bounds how long past flapQuiet a stale warning
	// lingers, so it is coarse.
	flapSweepInterval = 30 * time.Second
)

// streamEvents adapts rtspserver's playing-client callbacks into notification
// center entries. It implements rtspserver.Listener. Per RTSP path it keeps a
// flap detector per path and client host: while a client is flapping it
// publishes a single warning instead of the churn of connect/disconnect infos.
// Several clients play one path at once, so the detector is keyed by host too:
// two hosts reconnecting in turn do not add up to a flap. Two clients on one
// host share a detector, since RTSP carries no stable client identity. Its
// methods run on RTSP session read goroutines, so they only touch
// mutex-guarded per-client state and publish (a non-blocking send inside the
// Center); they never block.
type streamEvents struct {
	pub   notify.Publisher
	clock func() time.Time
	// sweepInterval is how often Run sweeps; a field so a test can shorten it
	// rather than wait the production flapSweepInterval.
	sweepInterval time.Duration

	mu sync.Mutex
	// paths holds each path and host's flap detector. Whether a client is
	// currently flapping (and so per-connection infos are suppressed) is read
	// straight from the detector via Flap.Active.
	paths map[flapID]*notify.Flap
}

// flapID identifies one flap detector: a stream path and the host (no port) a
// client connects from.
type flapID struct{ path, host string }

// flapIDOf builds the detector id for a client connecting to path from remote
// (host:port). An address that does not split is used whole.
func flapIDOf(path, remote string) flapID {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return flapID{path: path, host: host}
}

// newStreamEvents builds the adapter over pub. A nil clock uses time.Now; tests
// inject a fake clock so the flap window and quiet period are deterministic. A
// nil pub becomes a typed-nil *notify.Center (a no-op Publisher) so the emission
// sites can call it unconditionally, matching newAppliance's convention.
func newStreamEvents(pub notify.Publisher, clock func() time.Time) *streamEvents {
	if pub == nil {
		pub = (*notify.Center)(nil)
	}
	if clock == nil {
		clock = time.Now
	}
	return &streamEvents{pub: pub, clock: clock, sweepInterval: flapSweepInterval, paths: map[flapID]*notify.Flap{}}
}

// streamFlapKey is the condition key for a flapping-client warning on a path from
// a host. It pairs the onset with its clear, so one host settling cannot clear
// another host's warning.
func streamFlapKey(id flapID) string { return "stream:" + id.path + ":" + id.host + ":flap" }

// streamSource renders the notification Source chip: the path and the remote
// address the client connected from.
func streamSource(path, remote string) string { return path + " from " + remote }

// pathFlap returns id's flap detector, creating it on first use. The caller
// holds s.mu.
func (s *streamEvents) pathFlap(id flapID) *notify.Flap {
	f := s.paths[id]
	if f == nil {
		f = notify.NewFlap(flapMax, flapWindow, flapQuiet)
		s.paths[id] = f
	}
	return f
}

// ClientConnected records a client starting to play on path. It feeds the
// detector for the path and the client's host: a flap onset raises one warning and suppresses this connect; an
// ongoing flap suppresses it silently; otherwise a client_connected info is
// published. A flap clear (the first reconnect after a quiet gap) resolves the
// warning and this connect is reported normally as the start of a fresh session.
func (s *streamEvents) ClientConnected(path, remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := flapIDOf(path, remote)
	flap := s.pathFlap(id)
	switch flap.Event(s.clock()) {
	case notify.TransitionOnset:
		s.pub.Onset(notify.Notification{
			Severity: notify.SeverityWarning,
			Category: notify.CategoryStream,
			Key:      streamFlapKey(id),
			Source:   streamSource(path, id.host),
			Title:    "Client reconnecting repeatedly",
			Message:  fmt.Sprintf("A client at %s keeps reconnecting to %s; the per-connection notifications are suppressed until it settles", id.host, path),
		})
	case notify.TransitionClear:
		s.pub.Clear(streamFlapKey(id), flapSettled(id))
		s.pub.Publish(clientConnected(path, remote))
	case notify.TransitionNone:
		if !flap.Active() {
			s.pub.Publish(clientConnected(path, remote))
		}
	}
}

// ClientDisconnected records a playing client's session ending. An eviction is
// always reported (a deliberate token rotation, unrelated to the flap pattern);
// a normal teardown or read-error disconnect is suppressed while the client's
// host is flapping on the path and reported otherwise.
func (s *streamEvents) ClientDisconnected(path, remote string, reason rtspserver.DisconnectReason) {
	if reason == rtspserver.DisconnectEvicted {
		s.pub.Publish(clientEvicted(path, remote))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pathFlap(flapIDOf(path, remote)).Active() {
		return
	}
	s.pub.Publish(clientDisconnected(path, remote, reason))
}

// Run ages out flap warnings whose client has settled or gone away, and prunes
// idle detectors, on a fixed interval until ctx is done. Flap.Event only clears on
// the next connect after the quiet window; a client that recovers into a steady
// connection or gives up never sends that connect, so without this sweep the
// warning would stay pinned for the rest of the process. A device removed by a
// hot reload while flapping is handled the same way: no more connects arrive, the
// quiet window elapses, and the sweep clears and drops the detector.
func (s *streamEvents) Run(ctx context.Context) {
	ticker := time.NewTicker(s.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(s.clock())
		}
	}
}

// sweep clears any flap whose quiet window has elapsed with no further connect
// and drops detectors that have gone idle.
func (s *streamEvents) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, flap := range s.paths {
		if flap.Sweep(now) == notify.TransitionClear {
			s.pub.Clear(streamFlapKey(id), flapSettled(id))
		}
		if flap.Idle(now) {
			delete(s.paths, id)
		}
	}
}

// flapSettled builds the info body for a flap clear; Center.Clear fills the key,
// category, and source from the matching onset.
func flapSettled(id flapID) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityInfo,
		Title:    "Client stopped reconnecting",
		// Neutral wording: the flap ends either by the client settling into a
		// steady connection or by giving up entirely, and the sweep clear cannot
		// tell which, so it must not imply the client is still connected.
		Message: fmt.Sprintf("The client at %s on %s stopped rapidly reconnecting", id.host, id.path),
	}
}

// clientConnected builds the info entry for a client that started playing. Kind
// is set explicitly: the Center only stamps Kind for Onset/Clear, so a discrete
// Publish must carry KindEvent itself (an empty Kind is out of the API enum).
func clientConnected(path, remote string) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategoryStream,
		Kind:     notify.KindEvent,
		Source:   streamSource(path, remote),
		Title:    "Client connected",
		Message:  fmt.Sprintf("A client connected to %s from %s", path, remote),
	}
}

// clientDisconnected builds the info entry for a client whose session ended
// without eviction, naming the cause.
func clientDisconnected(path, remote string, reason rtspserver.DisconnectReason) notify.Notification {
	var msg string
	switch reason {
	case rtspserver.DisconnectTeardown:
		msg = fmt.Sprintf("The client on %s from %s disconnected", path, remote)
	default: // DisconnectReadError
		msg = fmt.Sprintf("The client on %s from %s dropped; the connection was lost", path, remote)
	}
	return notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategoryStream,
		Kind:     notify.KindEvent,
		Source:   streamSource(path, remote),
		Title:    "Client disconnected",
		Message:  msg,
	}
}

// clientEvicted builds the info entry for a client the server dropped because
// the access token changed (a deliberate rotation, always reported).
func clientEvicted(path, remote string) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategoryStream,
		Kind:     notify.KindEvent,
		Source:   streamSource(path, remote),
		Title:    "Client evicted",
		Message:  "Client dropped because the access token changed",
	}
}
