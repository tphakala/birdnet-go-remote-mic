//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/filesource"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// TestApplianceHungFileOpen walks a file device whose open never returns (a
// hung network mount) through the appliance: the run loop waits for it at most
// fileOpenTimeout, then neither a reload nor a retry waits again or starts a
// second open of that path, moving the device to another path opens at once,
// and the source the hung open finally returns is closed, not adopted.
func TestApplianceHungFileOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, _, cancel := newTestAppliance(t)
		defer shutdownApp(app, cancel)
		app.open = app.openRuntime
		// Inside the bubble, so the blocked open counts as durably blocked.
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		defer releaseOnce() // runs before shutdownApp, so a failed assertion still frees the worker

		const hungPath = "/mnt/hung"
		late := newBlockingSource(48000, 1)
		var mu sync.Mutex
		calls := map[string]int{}
		swapOpenFileSource(t, func(cfg filesource.Config) (audio.Source, error) {
			mu.Lock()
			calls[cfg.Path]++
			first := cfg.Path == hungPath && calls[cfg.Path] == 1
			mu.Unlock()
			if first {
				<-release
				return late, nil
			}
			return newBlockingSource(cfg.Rate, cfg.MinChannels), nil
		})
		callsTo := func(path string) int {
			mu.Lock()
			defer mu.Unlock()
			return calls[path]
		}
		cfgWith := func(hungID string) *config.Config {
			return &config.Config{Devices: []config.Device{
				testDevice("hung", hungID, "/h", 48000),
				testDevice("local", "file:/srv/local", "/l", 48000),
			}}
		}
		reconcile := func(cfg *config.Config) time.Duration {
			start := time.Now()
			app.reconcile(cfg)
			return time.Since(start)
		}
		wantServing := func(phase, name string) {
			t.Helper()
			if st := app.devices[name].status(); st.State != mgmtserver.StateServing {
				t.Errorf("%s: %s is %s (%s), want serving", phase, name, st.State, st.Error)
			}
		}

		// Startup: the loop waits out the bound once, then opens the rest.
		if got := reconcile(cfgWith("file:" + hungPath)); got != fileOpenTimeout {
			t.Errorf("startup reconcile took %v, want %v", got, fileOpenTimeout)
		}
		st := app.devices["hung"].status()
		if st.State != mgmtserver.StateSkipped || st.DownCause != downOpenFailed ||
			!strings.Contains(st.Error, "no answer from the filesystem within 5s") {
			t.Fatalf("after startup: hung is %s/%s %q, want skipped/%s naming the timeout", st.State, st.DownCause, st.Error, downOpenFailed)
		}
		if app.retries["hung"] == nil {
			t.Fatal("after startup: no retry scheduled for the hung device")
		}
		wantServing("startup", "local")

		// A reload while the open is stuck neither waits nor opens the path again,
		// and rewords the one active condition in place.
		if got := reconcile(cfgWith("file:" + hungPath)); got != 0 {
			t.Errorf("reload took %v, want no wait", got)
		}
		if got := callsTo(hungPath); got != 1 {
			t.Errorf("after reload: %d opens of %s, want 1", got, hungPath)
		}
		const pendingText = "an earlier open has not returned"
		if st := app.devices["hung"].status(); !strings.Contains(st.Error, pendingText) {
			t.Errorf("after reload: hung error %q, want it to contain %q", st.Error, pendingText)
		}
		if active := applianceCenter(t, app).Active(); len(active) != 1 || !strings.Contains(active[0].Message, pendingText) {
			t.Errorf("after reload: active notifications %+v, want one containing %q", active, pendingText)
		}
		wantServing("reload", "local")

		// The backoff retry finds the open still pending: no second open, no
		// second onset.
		runFor(t, app, retryBackoff[0]+time.Second)
		if rs := app.retries["hung"]; rs == nil || rs.failures != 2 {
			t.Errorf("after the retry: retry state %+v, want 2 failures (the retry ran)", rs)
		}
		if got := callsTo(hungPath); got != 1 {
			t.Errorf("after the retry: %d opens of %s, want 1", got, hungPath)
		}
		if got := countDown(t, app, "hung", notify.KindOnset); got != 1 {
			t.Errorf("down onsets = %d, want 1", got)
		}

		// Another path is not held by the stuck one.
		if got := reconcile(cfgWith("file:/srv/other")); got != 0 {
			t.Errorf("path change took %v, want no wait", got)
		}
		if got := callsTo("/srv/other"); got != 1 {
			t.Errorf("%d opens of /srv/other, want 1", got)
		}
		wantServing("path change", "hung")

		// The hung open returns at last: its source is closed, not adopted.
		releaseOnce()
		synctest.Wait()
		select {
		case <-late.closed:
		default:
			t.Error("the late source was not closed")
		}
		wantServing("late open", "hung")

		// The returned open cleared its path, so the path opens afresh.
		if got := reconcile(cfgWith("file:" + hungPath)); got != 0 {
			t.Errorf("path change back took %v, want no wait", got)
		}
		if got := callsTo(hungPath); got != 2 {
			t.Errorf("%d opens of %s, want 2", got, hungPath)
		}
		wantServing("path change back", "hung")
	})
}

// TestFileOpensStopsWaitingOnShutdown pins that a stuck open does not delay
// shutdown: the wait ends when the context does, and a done context starts no
// further open.
func TestFileOpensStopsWaitingOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{}) // inside the bubble, so the blocked open counts as durably blocked
		var calls atomic.Int32
		swapOpenFileSource(t, func(filesource.Config) (audio.Source, error) {
			calls.Add(1)
			<-release
			return nil, errors.New("released")
		})
		defer func() {
			close(release)
			synctest.Wait()
		}()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(time.Second, cancel)
		var f fileOpens
		start := time.Now()
		if _, err := f.open(ctx, filesource.Config{Path: "/mnt/hung"}); !errors.Is(err, context.Canceled) {
			t.Errorf("open = %v, want context.Canceled", err)
		}
		if got := time.Since(start); got != time.Second {
			t.Errorf("open waited %v, want %v (until the cancel)", got, time.Second)
		}

		start = time.Now()
		if _, err := f.open(ctx, filesource.Config{Path: "/mnt/other"}); !errors.Is(err, context.Canceled) {
			t.Errorf("open after cancel = %v, want context.Canceled", err)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("open after cancel waited %v, want no wait", got)
		}
		synctest.Wait() // let a wrongly started open reach the seam before counting
		if got := calls.Load(); got != 1 {
			t.Errorf("%d opens started, want 1 (none after the cancel)", got)
		}
	})
}
