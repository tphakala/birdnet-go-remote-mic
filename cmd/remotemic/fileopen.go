//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/filesource"
)

// fileOpenTimeout bounds the wait for one open of a file source (the path's
// stat and listing, then each tried file's header and first read), which a
// hung network mount can block indefinitely. The appliance's run loop and
// serve --check (the installed unit's ExecStartPre, which would otherwise
// hold the start and keep the management API down) both stop waiting after
// it. A slower open (a disk spinning up) is abandoned and retried; the
// abandoned open still runs to the end, which may warm the caches for the
// retry.
const fileOpenTimeout = 5 * time.Second

var (
	errFileOpenTimeout = fmt.Errorf("no answer from the filesystem within %v", fileOpenTimeout)
	errFileOpenPending = errors.New("no answer from the filesystem yet: an earlier open has not returned")
)

// fileOpens opens file sources off the caller's goroutine and waits for each
// at most fileOpenTimeout, with at most one open in flight per path: a call
// for a path whose earlier open has not returned starts nothing and fails at
// once with errFileOpenPending.
//
// An open blocked in a syscall cannot be interrupted, so its goroutine has no
// owner that stops or waits for it: it ends when the syscall returns (closing
// the source if its caller stopped waiting) or with the process. The per-path
// bound keeps such goroutines to one per path. The zero value is ready to use.
type fileOpens struct {
	mu      sync.Mutex
	pending map[string]bool // paths with an open in flight
}

// open opens cfg's source, giving up after fileOpenTimeout or once ctx is
// done. A source that opens after its caller gave up is closed, never handed
// to a later call.
func (f *fileOpens) open(ctx context.Context, cfg filesource.Config) (audio.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.pending[cfg.Path] {
		f.mu.Unlock()
		return nil, errFileOpenPending
	}
	if f.pending == nil {
		f.pending = make(map[string]bool)
	}
	f.pending[cfg.Path] = true
	f.mu.Unlock()

	open := openFileSource // read here: the goroutine may outlive a swap of the seam
	type result struct {
		src audio.Source
		err error
	}
	res := make(chan result)       // unbuffered: only a caller still waiting takes the source
	abandon := make(chan struct{}) // closed once the caller stopped waiting
	// Not waited for: an open blocked in a syscall cannot be interrupted. The
	// pending entry bounds these goroutines to one per path, and a source that
	// arrives after the caller gave up is closed here.
	go func() {
		src, err := open(cfg)
		// Cleared before the handoff, so the caller's next open of this path
		// never finds it pending.
		f.mu.Lock()
		delete(f.pending, cfg.Path)
		f.mu.Unlock()
		select {
		case res <- result{src, err}:
		case <-abandon:
			if src != nil {
				_ = src.Close()
			}
		}
	}()
	ctx, cancel := context.WithTimeoutCause(ctx, fileOpenTimeout, errFileOpenTimeout)
	defer cancel()
	select {
	case r := <-res:
		return r.src, r.err
	case <-ctx.Done():
		close(abandon)
		return nil, context.Cause(ctx) // errFileOpenTimeout, or why the caller's ctx ended
	}
}
