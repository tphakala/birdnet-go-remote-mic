//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// mgmtDownKey keys the management-API-unavailable condition in the notification
// center.
const mgmtDownKey = "management-api-down"

// errMgmtDisabled fails a background attempt whose reloaded config disables the
// management API, which ends the supervisor's retry.
var errMgmtDisabled = errors.New("management API disabled in the config file")

// mgmtRetryBackoff is the delay before each background attempt to bring up a
// management API that failed to start or stopped at runtime, indexed by the
// number of attempts made since an API last served for the longest delay (so
// an API that keeps dying soon after it comes back does not restart the
// backoff); the last entry repeats. The causes it waits out are
// slow (a certificate volume that mounts late, a permission fixed by hand, a
// full or read-only filesystem freed up, a port held by another process), so
// it starts at 30 s and caps at 10 minutes: a permanent fault then costs one
// attempt every 10 minutes (a config file read, a certificate check that may
// try to write a new pair, a listen, and two run-lock writes).
var mgmtRetryBackoff = [...]time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

// mgmtRetry is what superviseManagement needs to keep the API up.
type mgmtRetry struct {
	// attempt makes one background attempt to bring the API up.
	attempt func() (*mgmtServer, error)
	// halted handles an API that stopped on its own, as soon as it stops and
	// before it drains; nil does nothing.
	halted func(*mgmtServer, error)
	// died handles an API that stopped on its own, after it drained.
	died func(*mgmtServer, error)
	// delays[n] is the wait before attempt n+1; the last delay repeats.
	delays []time.Duration
	// stable is how long an API must serve before its death restarts the
	// backoff from delays[0]. An API that dies sooner resumes the backoff where
	// it left off, so one that dies right after every start settles at the
	// slowest delay instead of restarting every delays[0].
	stable time.Duration
}

// superviseManagement keeps the management API up until ctx is cancelled,
// starting from srv (the API startManagement brought up) or, when that start
// failed with startErr, from a retry. While an API serves it waits for it to
// stop: a shutdown on ctx ends the supervisor once the API drained (as does a
// listener failure once ctx is cancelled, since the two can race), and a
// runtime failure hands the dead API to r.halted at once and to r.died once it
// drained, then retries. A retry waits out r.delays before each attempt and
// logs a failure only when its message differs from the previous one, so a
// permanent fault logs once rather than every attempt. An attempt that finds
// management disabled in the config file ends the supervisor. m's serving
// method follows the API that serves, m's lost channel is signalled whenever
// an API stops on its own or the retry gives up (so run() can retake its exit
// decision), and m.done closes on return.
func superviseManagement(ctx context.Context, m *mgmt, srv *mgmtServer, startErr error, r mgmtRetry) {
	defer close(m.done)
	var last string
	if startErr != nil {
		last = startErr.Error()
	}
	n := 0 // attempts since an API last served for r.stable, indexing r.delays
	for {
		if srv != nil {
			since := time.Now()
			err := srv.halt()
			m.cur.Store(nil)
			// A listener failure racing shutdown can reach the server before
			// the cancellation does; it is not a runtime death.
			if err == nil || ctx.Err() != nil {
				_ = srv.wait()
				return
			}
			if time.Since(since) >= r.stable {
				n = 0
			}
			if r.halted != nil {
				r.halted(srv, err)
			}
			m.signalLost()
			_ = srv.wait()
			r.died(srv, err)
			last = err.Error()
		}
		srv = retryManagement(ctx, r, &n, &last)
		if srv == nil {
			if ctx.Err() == nil {
				m.signalLost()
			}
			return
		}
		m.cur.Store(srv)
	}
}

// retryManagement runs superviseManagement's background attempts until one
// brings the API up, which it returns. It returns nil when ctx is cancelled or
// an attempt finds management disabled in the config file. n and last carry
// the backoff position and the last logged failure across outages.
func retryManagement(ctx context.Context, r mgmtRetry, n *int, last *string) *mgmtServer {
	for tries := 1; ; tries++ {
		t := time.NewTimer(backoffAt(r.delays, *n))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		*n++
		s, err := r.attempt()
		if errors.Is(err, errMgmtDisabled) {
			log.Print("management API: management.enabled is now false in the config file; stopped retrying")
			return nil
		}
		if err != nil {
			if msg := err.Error(); msg != *last {
				log.Printf("management API still unavailable: %v (retrying in the background)", err)
				*last = msg
			}
			continue
		}
		log.Printf("management API recovered after %d background attempt(s)", tries)
		return s
	}
}

// recoverManagement is one background attempt to bring up a management API that
// failed to start or stopped at runtime. While the run lock shows this
// appliance with no API address, the token CLI edits the config file directly
// (the appliance has no config writer), so the file may no longer match the
// config that seeds the API's store (the startup snapshot, or the config of the
// API that stopped). Seeding the store from that stale config would let the
// next web UI save revert the operator's edit, so the attempt reloads the file
// first and, when it changed, applies it live through the reloader exactly as
// a PATCH would, then seeds the store from it. A file that no longer loads
// fails the attempt, as it would fail the next start; a file that is missing
// keeps the config the attempt already has. Applying an edited file publishes
// a config event, and a successful attempt clears the management-unavailable
// condition.
//
// The listener address and certificate directory come from that config with
// the serve flags applied, as they would on a restart, so editing the file
// fixes a port in use or an unreadable cert_dir without one. A file that now
// disables management fails the attempt with errMgmtDisabled, which ends the
// retry.
//
// The attempt marks the appliance as starting up in the run lock before it
// reloads the file, so a token command that reads the lock during the attempt
// asks the operator to try again rather than editing a file the attempt may
// already have read. It publishes the new API's endpoint itself once the API
// serves, and the lock without one when the attempt fails. What remains is a
// token command that read the lock just before the attempt began and writes
// the file just after the reload: the window between its own read and write.
func recoverManagement(ctx context.Context, p *mgmtParams) (*mgmtServer, error) {
	p.runLock.starting()
	s, err := reloadAndServe(ctx, p)
	if err != nil {
		p.runLock.publish(nil)
		return nil, err
	}
	p.runLock.publish(&s.mgmtEndpoint)
	return s, nil
}

// reloadAndServe is recoverManagement's attempt: reload the config file,
// apply it when it changed, and bring the API up as that config says.
func reloadAndServe(ctx context.Context, p *mgmtParams) (*mgmtServer, error) {
	// LoadQuiet, not LoadOrDefault: a file missing now (a volume that went away,
	// the very kind of fault this retry waits out) must not read as Default()
	// and be applied live, tearing down every stream. It keeps the config the
	// attempt already has instead, as a missing file has no newer content to
	// adopt. Quiet because the startup load already warned about the file's
	// permissions.
	fresh, err := config.LoadQuiet(p.cfgPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("cannot reload config: %w", err)
	case !sameOnDisk(&fresh, p.storeCfg):
		if err := p.applyEdited(ctx, &fresh); err != nil {
			return nil, err
		}
		// A fresh pointer, not a write through p.storeCfg: run() still holds the
		// startup snapshot it points at.
		p.storeCfg = &fresh
	}

	running := p.storeCfg.Clone()
	applyServeOverrides(&running, p.overrides)
	if !running.ManagementEnabled() {
		p.center.Clear(mgmtDownKey, notify.Notification{
			Severity: notify.SeverityInfo,
			Title:    "Management API disabled",
			Message:  "The config file disables the web UI and API; they stay off until it enables them and the appliance restarts",
		})
		return nil, errMgmtDisabled
	}
	p.useConfig(&running)

	s, err := serveManagement(ctx, p)
	if err != nil {
		return nil, err
	}
	p.center.Clear(mgmtDownKey, notify.Notification{
		Severity: notify.SeverityInfo,
		Title:    "Management API recovered",
		Message:  "The web UI and API are now serving",
	})
	return s, nil
}

// sameOnDisk reports whether a and b are written as the same config file.
// config.Save writes yaml.Marshal's output, so two configs that differ only in
// memory compare equal: a saved nil device list reloads as an empty one, which
// reflect.DeepEqual would read as an edit. A marshal failure reads as a
// difference, so the reloaded file is applied rather than skipped.
func sameOnDisk(a, b *config.Config) bool {
	ab, aerr := yaml.Marshal(a)
	bb, berr := yaml.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(ab, bb)
}

// applyEdited applies a config file edited while the API was down live
// through the reloader, and records it in the history as a PATCH would: a
// config event, the access-control event when the apply changed the token
// (its main case is a remote-mic token edit), and the end of any pending
// restart-required condition, since the running pipeline now matches the
// file.
func (p *mgmtParams) applyEdited(ctx context.Context, fresh *config.Config) error {
	if p.reloader == nil {
		return nil
	}
	wasEnabled, genBefore := p.guard.Snapshot()
	if err := p.reloader(ctx, fresh.Clone()); err != nil {
		return fmt.Errorf("cannot apply the config file edited while the API was down: %w", err)
	}
	log.Print("management API: applied the config file edited while the API was down")
	p.center.Publish(notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategoryConfig,
		Kind:     notify.KindEvent,
		Title:    "Config file applied",
		Message:  "Applied the config file edited while the management API was down (for example by remote-mic token)",
	})
	// The reload set the guard from the file; a moved generation means the
	// token changed and live RTSP sessions were evicted.
	if nowEnabled, genAfter := p.guard.Snapshot(); genAfter != genBefore {
		p.center.Publish(mgmtserver.AuthChangedNotification(wasEnabled, nowEnabled))
	}
	mgmtserver.ClearRestartRequired(p.center)
	return nil
}
