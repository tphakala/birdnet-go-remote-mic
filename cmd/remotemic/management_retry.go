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
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// mgmtDownKey keys the management-API-unavailable condition in the notification
// center.
const mgmtDownKey = "management-api-down"

// mgmtRetryBackoff is the delay before each background attempt to bring up a
// management API that failed to start, indexed by the number of attempts made;
// the last entry repeats. The causes it waits out are slow (a certificate volume
// that mounts late, a permission fixed by hand, a full or read-only filesystem
// freed up, a port held by another process), so it starts at 30 s and caps at
// 10 minutes: a permanent fault then costs one attempt every 10 minutes (a
// config file read, a certificate check that may try to write a new pair, and
// a listen).
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

// pendingRecovery reports, without blocking, an endpoint a background retry has
// delivered on m's Up channel but nobody has received yet. It returns false for
// a nil handle, a handle that never retried, and one with nothing pending.
func pendingRecovery(m *mgmt) (mgmtEndpoint, bool) {
	select {
	case ep := <-m.Up():
		return ep, true
	default:
		return mgmtEndpoint{}, false
	}
}

// adoptPending hands a pending recovered endpoint (see pendingRecovery) to
// adopt, and does nothing when none is pending. Adopting early is always
// correct: it is exactly what receiving from Up would do.
func adoptPending(m *mgmt, adopt func(mgmtEndpoint)) {
	if ep, ok := pendingRecovery(m); ok {
		adopt(ep)
	}
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
// startup snapshot. Applying an edited file publishes a config event, and a
// successful attempt clears the management-unavailable condition.
//
// The attempt marks the appliance as starting up in the run lock before it
// reloads the file, so a token command that reads the lock during the attempt
// asks the operator to try again rather than editing a file the attempt may
// already have read. It publishes the new API's endpoint itself once the API
// serves, and the lock without one when the attempt fails. What remains is a
// token command that read the lock just before the attempt began and writes
// the file just after the reload: the window between its own read and write.
func recoverManagement(ctx context.Context, p *mgmtParams) (*mgmt, error) {
	p.runLock.starting()
	h, err := reloadAndServe(ctx, p)
	if err != nil {
		p.runLock.publish(nil)
		return nil, err
	}
	p.runLock.publish(&mgmtEndpoint{addr: h.addr, certPath: h.certPath})
	return h, nil
}

// reloadAndServe is recoverManagement's attempt: reload the config file,
// apply it when it changed, and bring the API up.
func reloadAndServe(ctx context.Context, p *mgmtParams) (*mgmt, error) {
	// LoadQuiet, not LoadOrDefault: a file missing now (a volume that went away,
	// the very kind of fault this retry waits out) must not read as Default()
	// and be applied live, tearing down every stream. It keeps the startup
	// snapshot instead, as a missing file has no newer content to adopt. Quiet
	// because the startup load already warned about the file's permissions.
	fresh, err := config.LoadQuiet(p.cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return serveRecovered(ctx, p)
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
			// A PATCH leaves a config event in the history; so does this apply,
			// since it can change the access token and end RTSP sessions.
			p.center.Publish(notify.Notification{
				Severity: notify.SeverityInfo,
				Category: notify.CategoryConfig,
				Kind:     notify.KindEvent,
				Title:    "Config file applied",
				Message:  "Applied the config file edited while the management API was down (for example by remote-mic token)",
			})
		}
		// A fresh pointer, not a write through p.storeCfg: run() still holds the
		// startup snapshot it points at.
		p.storeCfg = &fresh
	}
	return serveRecovered(ctx, p)
}

// serveRecovered brings the API up for recoverManagement and, once it serves,
// clears the unavailable condition startManagementWith raised.
func serveRecovered(ctx context.Context, p *mgmtParams) (*mgmt, error) {
	h, err := serveManagement(ctx, p)
	if err != nil {
		return nil, err
	}
	p.center.Clear(mgmtDownKey, notify.Notification{
		Severity: notify.SeverityInfo,
		Title:    "Management API recovered",
		Message:  "The web UI and API are now serving",
	})
	return h, nil
}
