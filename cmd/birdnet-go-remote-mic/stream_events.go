//go:build linux

package main

import (
	"fmt"
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
)

// streamEvents adapts rtspserver's playing-client callbacks into notification
// center entries. It implements rtspserver.Listener. Per RTSP path it keeps a
// flap detector: while a path is flapping it publishes a single warning instead
// of the churn of connect/disconnect infos. Its methods run on RTSP session read
// goroutines, so they only touch mutex-guarded per-path state and publish (a
// non-blocking send inside the Center); they never block.
type streamEvents struct {
	pub   notify.Publisher
	clock func() time.Time

	mu    sync.Mutex
	paths map[string]*pathFlap
}

// pathFlap is one path's flap detector plus the derived active flag. notify.Flap
// exposes no accessor for its active state, so the adapter tracks it here to
// decide whether a disconnect should be suppressed.
type pathFlap struct {
	flap     *notify.Flap
	flapping bool
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
	return &streamEvents{pub: pub, clock: clock, paths: map[string]*pathFlap{}}
}

// streamFlapKey is the condition key for a path's flapping-client warning. It
// pairs the onset with its clear.
func streamFlapKey(path string) string { return "stream:" + path + ":flap" }

// streamSource renders the notification Source chip: the path and the remote
// address the client connected from.
func streamSource(path, remote string) string { return path + " from " + remote }

// pathState returns path's flap state, creating it on first use. The caller
// holds s.mu.
func (s *streamEvents) pathState(path string) *pathFlap {
	st := s.paths[path]
	if st == nil {
		st = &pathFlap{flap: notify.NewFlap(flapMax, flapWindow, flapQuiet)}
		s.paths[path] = st
	}
	return st
}

// ClientConnected records a client starting to play on path. It feeds the path's
// flap detector: a flap onset raises one warning and suppresses this connect; an
// ongoing flap suppresses it silently; otherwise a client_connected info is
// published. A flap clear (the first reconnect after a quiet gap) resolves the
// warning and this connect is reported normally as the start of a fresh session.
func (s *streamEvents) ClientConnected(path, remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.pathState(path)
	switch st.flap.Event(s.clock()) {
	case notify.TransitionOnset:
		st.flapping = true
		s.pub.Onset(notify.Notification{
			Severity: notify.SeverityWarning,
			Category: notify.CategoryStream,
			Key:      streamFlapKey(path),
			Source:   path,
			Title:    "Client reconnecting repeatedly",
			Message:  fmt.Sprintf("A client keeps reconnecting to %s; the per-connection notifications are suppressed until it settles", path),
		})
	case notify.TransitionClear:
		st.flapping = false
		s.pub.Clear(streamFlapKey(path), notify.Notification{
			Severity: notify.SeverityInfo,
			Title:    "Client stopped reconnecting",
			Message:  fmt.Sprintf("The client on %s has settled", path),
		})
		s.pub.Publish(clientConnected(path, remote))
	case notify.TransitionNone:
		if !st.flapping {
			s.pub.Publish(clientConnected(path, remote))
		}
	}
}

// ClientDisconnected records a playing client's session ending. An eviction is
// always reported (a deliberate token rotation, unrelated to the flap pattern);
// a normal teardown or read-error disconnect is suppressed while the path is
// flapping and reported otherwise.
func (s *streamEvents) ClientDisconnected(path, remote string, reason rtspserver.DisconnectReason) {
	if reason == rtspserver.DisconnectEvicted {
		s.pub.Publish(clientEvicted(path, remote))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pathState(path).flapping {
		return
	}
	s.pub.Publish(clientDisconnected(path, remote, reason))
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
