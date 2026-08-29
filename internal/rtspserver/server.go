// Package rtspserver is the appliance's minimal TCP-interleaved RTSP server. It
// serves a single audio track (L16 or Opus) to one playing client at a time,
// reusing go-audio-stream's RTSP message layer and send primitives.
package rtspserver

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// Config configures the server.
type Config struct {
	Listen  string        // RTSP listen address (host:port)
	Path    string        // session path; defaults to "/stream"
	SDP     []byte        // the DESCRIBE body, built at startup
	Timeout time.Duration // session/idle timeout; defaults to 60s
}

// FrameSource is how the server pulls media: the pipeline pushes frames in, the
// playing session's writer drains them. It is implemented in main; the media
// path (Task 5) consumes it.
type FrameSource interface {
	Next(ctx context.Context) (pipeline.Frame, error)
}

// Server accepts RTSP connections and serves the single configured track.
type Server struct {
	cfg    Config
	frames FrameSource

	slotMu    sync.Mutex
	slotTaken bool // true while one connection holds the single PLAY session slot
}

// New builds a Server. frames may be nil for control-only use (tests).
func New(cfg Config, frames FrameSource) *Server {
	if cfg.Path == "" {
		cfg.Path = "/stream"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &Server{cfg: cfg, frames: frames}
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

// acquireSlot claims the single session slot, returning false if it is taken.
func (s *Server) acquireSlot() bool {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	if s.slotTaken {
		return false
	}
	s.slotTaken = true
	return true
}

func (s *Server) releaseSlot() {
	s.slotMu.Lock()
	s.slotTaken = false
	s.slotMu.Unlock()
}
