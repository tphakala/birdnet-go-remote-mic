//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"reflect"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// mgmtRetryBackoff is the delay before each background attempt to bring up a
// management API that failed to start, indexed by the number of attempts made;
// the last entry repeats. The causes it waits out are slow (a certificate volume
// that mounts late, a permission fixed by hand, a full or read-only filesystem
// freed up, a port held by another process), so it starts at 30 s and caps at
// 10 minutes: a permanent fault costs one certificate check every 10 minutes.
var mgmtRetryBackoff = [...]time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

// retryManagement retries attempt in the background after the management API
// failed to start with firstErr, waiting delays[n] before attempt n+1 (the last
// delay repeats). A failure is logged only when its message differs from the
// previous one, so a permanent fault logs once rather than every attempt. When
// an attempt succeeds, the new API's endpoint is sent once on the returned
// handle's Up channel, and the handle's Wait then follows that API's shutdown.
// Cancelling ctx stops the retry.
func retryManagement(ctx context.Context, attempt func() (*mgmt, error), delays []time.Duration, firstErr error) *mgmt {
	up := make(chan mgmtEndpoint, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		last := firstErr.Error()
		for n := 0; ; n++ {
			t := time.NewTimer(delays[min(n, len(delays)-1)])
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			if ctx.Err() != nil {
				return
			}
			h, err := attempt()
			if err != nil {
				if msg := err.Error(); msg != last {
					log.Printf("management API still disabled: %v (retrying in the background)", err)
					last = msg
				}
				continue
			}
			log.Printf("management API recovered after %d background attempt(s)", n+1)
			up <- mgmtEndpoint{addr: h.addr, certPath: h.certPath}
			h.Wait()
			return
		}
	}()
	return &mgmt{done: done, up: up}
}

// recoverManagement is one background attempt to bring up a management API that
// failed to start. While no API address is published in the run lock, the token
// CLI edits the config file directly (the appliance has no config writer), so
// the file may no longer match the startup snapshot that seeds the API's config
// store. Seeding the store from the stale snapshot would let the next web UI
// save revert the operator's edit, so the attempt reloads the file first and,
// when it changed, applies it live through the reloader exactly as a PATCH
// would, then seeds the store from it. A file that no longer loads fails the
// attempt, as it would fail the next start; a file that is missing keeps the
// startup snapshot.
//
// A token command that reads the lock before the run loop republishes it can
// still edit the file after this reload; that window is the few milliseconds of
// one successful attempt, once per outage.
func recoverManagement(ctx context.Context, p *mgmtParams) (*mgmt, error) {
	// LoadQuiet, not LoadOrDefault: a file missing now (a volume that went away,
	// the very kind of fault this retry waits out) must not read as Default()
	// and be applied live, tearing down every stream. It keeps the startup
	// snapshot instead, as a missing file has no newer content to adopt. Quiet
	// because the startup load already warned about the file's permissions.
	fresh, err := config.LoadQuiet(p.cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return serveManagement(ctx, p)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot reload config: %w", err)
	}
	if !reflect.DeepEqual(&fresh, p.storeCfg) {
		if p.reloader != nil {
			if err := p.reloader(ctx, fresh.Clone()); err != nil {
				return nil, fmt.Errorf("cannot apply the config file edited while the API was down: %w", err)
			}
			log.Print("management API: applied the config file edited while the API was down")
		}
		// A fresh pointer, not a write through p.storeCfg: run() still holds the
		// startup snapshot it points at.
		p.storeCfg = &fresh
	}
	return serveManagement(ctx, p)
}
