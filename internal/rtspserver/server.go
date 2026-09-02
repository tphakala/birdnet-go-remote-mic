// Package rtspserver is the appliance's minimal TCP-interleaved RTSP server. It
// serves one or more audio tracks (L16 or Opus), routed by URL path, each to
// one playing client at a time, reusing go-audio-stream's RTSP message layer
// and send primitives.
package rtspserver

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// Config configures the server.
type Config struct {
	Listen     string        // RTSP listen address (host:port)
	Timeout    time.Duration // session/idle timeout; defaults to 60s
	SRInterval time.Duration // RTCP sender report interval; defaults to 5s
	// Auth is the shared-token guard. A nil or disabled guard means open access;
	// an enabled one makes every method except OPTIONS require a Digest answer
	// (RFC 7616, MD5) with the token as the password. A connection stays
	// authenticated only while the guard's generation is unchanged: enabling or
	// rotating the token via a hot reload advances the generation, so a
	// connection authenticated under the old token (or one serving under open
	// access) is torn down. A session actively streaming is dropped by its
	// writer as soon as the change lands (proactive, not waiting on the client),
	// and any other connection is re-challenged on its next request.
	Auth *auth.Guard
}

// FrameSource is how the server pulls media: the pipeline pushes frames in, the
// playing session's writer drains them. ChanSource (feed.go) is the
// implementation; tests pass their own.
type FrameSource interface {
	Next(ctx context.Context) (pipeline.Frame, error)
}

// activator is implemented by frame sources whose delivery can be gated
// (ChanSource): PLAY activates, teardown deactivates.
type activator interface{ SetActive(active bool) }

// Track is one device's stream: its RTSP path, DESCRIBE body, payload type and
// frame source. Each track serves one playing client at a time.
type Track struct {
	Path        string // session path, e.g. "/stream"; SETUP matches Path+"/trackID=0"
	SDP         []byte // the DESCRIBE body, built at startup
	PayloadType int    // RTP payload type (96 L16, 97 Opus)
	Frames      FrameSource

	slotMu    sync.Mutex
	slotTaken bool
}

// acquireSlot claims the track's single session slot; false if taken.
func (t *Track) acquireSlot() bool {
	t.slotMu.Lock()
	defer t.slotMu.Unlock()
	if t.slotTaken {
		return false
	}
	t.slotTaken = true
	return true
}

func (t *Track) releaseSlot() {
	t.slotMu.Lock()
	t.slotTaken = false
	t.slotMu.Unlock()
}

// ClientConnected reports whether a session currently holds the track's single
// playing slot. Safe to call concurrently with SETUP/TEARDOWN.
func (t *Track) ClientConnected() bool {
	t.slotMu.Lock()
	defer t.slotMu.Unlock()
	return t.slotTaken
}

// Server accepts RTSP connections and routes each request to a track by URL
// path.
type Server struct {
	cfg Config

	mu     sync.RWMutex
	tracks map[string]*Track
}

// New builds a Server over tracks. Track paths must be unique (config
// validation guarantees this); a duplicate would silently keep the last one.
//
//nolint:gocritic // Config by value is the constructor API; New runs once at startup.
func New(cfg Config, tracks ...*Track) *Server {
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.SRInterval == 0 {
		cfg.SRInterval = 5 * time.Second
	}
	m := make(map[string]*Track, len(tracks))
	for _, tr := range tracks {
		m[tr.Path] = tr
	}
	return &Server{cfg: cfg, tracks: m}
}

// AuthEnabled reports whether the stream currently requires a token, i.e. the
// configured guard is enabled. It reads the guard live, so a hot reload that
// sets or clears the token is reflected at once. A nil guard reports false.
func (s *Server) AuthEnabled() bool {
	return s.cfg.Auth.Enabled()
}

// lookup returns the track registered at path, or nil.
func (s *Server) lookup(path string) *Track {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tracks[path]
}

// RemoveTrack unregisters a dead device's path: later requests get 404. The
// caller also closes the track's frame source so a playing writer tears down.
func (s *Server) RemoveTrack(path string) {
	s.mu.Lock()
	delete(s.tracks, path)
	s.mu.Unlock()
}

// AddTrack registers a track at its path so the running server routes to it
// without rebinding the listener. It is how a hot-reloaded device joins a
// serving appliance. Registering a path that already exists replaces it (the
// restart-on-param-change case removes the old track and adds the new one on the
// same path); the caller is responsible for tearing the old track's frame
// source down first so no writer keeps draining it.
func (s *Server) AddTrack(t *Track) {
	s.mu.Lock()
	s.tracks[t.Path] = t
	s.mu.Unlock()
}

// HasTrack reports whether a track is currently registered at path. It lets a
// caller confirm a hot-reload add or remove took effect without opening an RTSP
// session.
func (s *Server) HasTrack(path string) bool {
	return s.lookup(path) != nil
}

// ListenAndServe listens on cfg.Listen and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // Accept failed because ctx cancellation closed the listener: a clean shutdown, not an error.
			}
			return err
		}
		go s.serveConn(ctx, conn)
	}
}
