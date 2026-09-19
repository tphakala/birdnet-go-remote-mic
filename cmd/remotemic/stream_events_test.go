//go:build linux

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
)

const (
	opPublish = "publish"
	opOnset   = "onset"
	opClear   = "clear"
	opResolve = "resolve"

	titleConnected = "Client connected"

	evPath   = "/stream"
	evRemote = "10.0.0.9:41000"
)

// recordedNote is one publish the adapter made, captured by recordedPub.
type recordedNote struct {
	op  string // opPublish | opOnset | opClear | opResolve
	key string
	n   notify.Notification
}

// recordedPub is a notify.Publisher that records every call, so a test can
// assert both what was published and, by their absence, what was suppressed. Its
// Onset and Clear always report success, matching a fresh key transitioning. It
// is mutex-guarded so the Run sweep test can drive it from Run's goroutine while
// the test polls; the single-threaded tests are unaffected.
type recordedPub struct {
	mu    sync.Mutex
	notes []recordedNote
}

//nolint:gocritic // Notification by value is the Publisher contract; the fake must match the interface signature.
func (p *recordedPub) Publish(n notify.Notification) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes = append(p.notes, recordedNote{op: opPublish, n: n})
}

//nolint:gocritic // Notification by value is the Publisher contract; the fake must match the interface signature.
func (p *recordedPub) Onset(n notify.Notification) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes = append(p.notes, recordedNote{op: opOnset, key: n.Key, n: n})
	return true
}

//nolint:gocritic // Notification by value is the Publisher contract; the fake must match the interface signature.
func (p *recordedPub) Clear(key string, n notify.Notification) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes = append(p.notes, recordedNote{op: opClear, key: key, n: n})
	return true
}

func (p *recordedPub) Resolve(key, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes = append(p.notes, recordedNote{op: opResolve, key: key})
	return true
}

// countTitles tallies notes with the given op whose Title matches.
func (p *recordedPub) countTitles(op, title string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.notes {
		if p.notes[i].op == op && p.notes[i].n.Title == title {
			n++
		}
	}
	return n
}

// lastByOp returns a copy of the last recorded note with the given op, or nil.
func (p *recordedPub) lastByOp(op string) *recordedNote {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.notes) - 1; i >= 0; i-- {
		if p.notes[i].op == op {
			cp := p.notes[i]
			return &cp
		}
	}
	return nil
}

func TestStreamEventsNormalConnectDisconnect(t *testing.T) {
	pub := &recordedPub{}
	se := newStreamEvents(pub, func() time.Time { return time.Unix(0, 0) })

	se.ClientConnected(evPath, evRemote)
	se.ClientDisconnected(evPath, evRemote, rtspserver.DisconnectTeardown)

	if len(pub.notes) != 2 {
		t.Fatalf("published %d notes, want 2: %+v", len(pub.notes), pub.notes)
	}
	first := pub.notes[0]
	if first.op != opPublish || first.n.Title != titleConnected {
		t.Errorf("first note = %+v, want a client-connected publish", first)
	}
	if first.n.Category != notify.CategoryStream || first.n.Severity != notify.SeverityInfo || first.n.Kind != notify.KindEvent {
		t.Errorf("connect note = %+v, want info/stream/event", first.n)
	}
	if want := evPath + " from " + evRemote; first.n.Source != want {
		t.Errorf("connect source = %q, want %q", first.n.Source, want)
	}
	second := pub.notes[1]
	if second.op != opPublish || second.n.Title != "Client disconnected" || second.n.Kind != notify.KindEvent {
		t.Errorf("second note = %+v, want a client-disconnected event publish", second)
	}
	if !strings.Contains(second.n.Message, "disconnected") {
		t.Errorf("teardown disconnect message = %q, want it to say the client disconnected", second.n.Message)
	}
}

// TestStreamEventsReadErrorDisconnect covers a dropped connection reported while
// the path is not flapping, and constructs the adapter with a nil clock so the
// time.Now default is exercised.
func TestStreamEventsReadErrorDisconnect(t *testing.T) {
	pub := &recordedPub{}
	se := newStreamEvents(pub, nil)

	se.ClientConnected(evPath, evRemote)
	se.ClientDisconnected(evPath, evRemote, rtspserver.DisconnectReadError)

	last := pub.lastByOp(opPublish)
	if last == nil || last.n.Title != "Client disconnected" {
		t.Fatalf("last publish = %+v, want a client-disconnected publish", last)
	}
	if !strings.Contains(last.n.Message, "connection was lost") {
		t.Errorf("read-error disconnect message = %q, want it to mention the lost connection", last.n.Message)
	}
}

func TestStreamEventsEvictionAlwaysPublished(t *testing.T) {
	pub := &recordedPub{}
	now := time.Unix(0, 0)
	se := newStreamEvents(pub, func() time.Time { return now })

	// Drive the path into a flap so normal disconnects are suppressed.
	for range flapMax + 1 {
		se.ClientConnected(evPath, evRemote)
		now = now.Add(time.Second)
	}
	before := len(pub.notes)

	// A normal disconnect is suppressed while flapping.
	se.ClientDisconnected(evPath, evRemote, rtspserver.DisconnectReadError)
	if len(pub.notes) != before {
		t.Errorf("a normal disconnect published %d notes while flapping, want 0", len(pub.notes)-before)
	}

	// An eviction is published even while flapping: a token rotation is deliberate.
	se.ClientDisconnected(evPath, evRemote, rtspserver.DisconnectEvicted)
	if len(pub.notes) != before+1 {
		t.Fatalf("an eviction published %d notes while flapping, want 1", len(pub.notes)-before)
	}
	last := pub.notes[len(pub.notes)-1]
	if last.op != opPublish || last.n.Title != "Client evicted" || last.n.Category != notify.CategoryStream || last.n.Kind != notify.KindEvent {
		t.Errorf("eviction note = %+v, want a client-evicted stream event publish", last)
	}
}

func TestStreamEventsFlapOnsetSuppressesInfos(t *testing.T) {
	pub := &recordedPub{}
	now := time.Unix(0, 0)
	se := newStreamEvents(pub, func() time.Time { return now })

	// Four connects inside the window: the first three are reported, the fourth
	// crosses the flapMax threshold and raises one warning instead.
	for range flapMax + 1 {
		se.ClientConnected(evPath, evRemote)
		now = now.Add(time.Second)
	}
	if got := pub.countTitles(opPublish, titleConnected); got != 3 {
		t.Errorf("client-connected infos = %d, want 3 before the flap onset suppresses", got)
	}
	if got := pub.countTitles(opOnset, "Client reconnecting repeatedly"); got != 1 {
		t.Errorf("flap onsets = %d, want 1", got)
	}
	onset := pub.lastByOp(opOnset)
	if onset == nil {
		t.Fatal("no flap onset was published")
	}
	if onset.key != streamFlapKey(evPath) {
		t.Errorf("flap onset key = %q, want %q", onset.key, streamFlapKey(evPath))
	}
	if onset.n.Severity != notify.SeverityWarning || onset.n.Category != notify.CategoryStream {
		t.Errorf("flap onset = %+v, want warning/stream", onset.n)
	}

	// A further reconnect while flapping is suppressed entirely.
	before := len(pub.notes)
	se.ClientConnected(evPath, evRemote)
	if len(pub.notes) != before {
		t.Errorf("a reconnect while flapping published %d notes, want 0", len(pub.notes)-before)
	}
}

func TestStreamEventsFlapClearsAfterQuiet(t *testing.T) {
	pub := &recordedPub{}
	now := time.Unix(0, 0)
	se := newStreamEvents(pub, func() time.Time { return now })

	for range flapMax + 1 {
		se.ClientConnected(evPath, evRemote)
		now = now.Add(time.Second)
	}
	// Quiet longer than flapQuiet, then a reconnect: the flap clears and this
	// connect is reported as a fresh session.
	now = now.Add(flapQuiet + time.Second)
	se.ClientConnected(evPath, evRemote)

	if got := pub.countTitles(opClear, "Client stopped reconnecting"); got != 1 {
		t.Errorf("flap clears = %d, want 1", got)
	}
	last := pub.notes[len(pub.notes)-1]
	if last.op != opPublish || last.n.Title != titleConnected {
		t.Errorf("note after the clear = %+v, want a fresh client-connected publish", last)
	}
	// The clear must carry the same key as the onset so the two pair.
	clearNote := pub.lastByOp(opClear)
	if clearNote == nil || clearNote.key != streamFlapKey(evPath) {
		t.Fatalf("clear = %+v, want key %q", clearNote, streamFlapKey(evPath))
	}
}

// TestStreamEventsSweepClearsSettledFlap covers the flap-clear fix: a client that
// stops reconnecting (settles into a steady connection or gives up) sends no
// further connect, so Flap.Event never clears the warning. The periodic sweep
// ages it out once the quiet window elapses, and drops the path.
func TestStreamEventsSweepClearsSettledFlap(t *testing.T) {
	pub := &recordedPub{}
	now := time.Unix(0, 0)
	se := newStreamEvents(pub, func() time.Time { return now })

	for range flapMax + 1 {
		se.ClientConnected(evPath, evRemote)
		now = now.Add(time.Second)
	}
	// A sweep before the quiet window elapses does nothing.
	se.sweep(now)
	if got := pub.countTitles(opClear, "Client stopped reconnecting"); got != 0 {
		t.Fatalf("premature sweep clear = %d, want 0", got)
	}
	// After the quiet window with no reconnect, the sweep clears the warning.
	now = now.Add(flapQuiet + time.Second)
	se.sweep(now)
	if got := pub.countTitles(opClear, "Client stopped reconnecting"); got != 1 {
		t.Fatalf("sweep clear = %d, want 1", got)
	}
	clearNote := pub.lastByOp(opClear)
	if clearNote == nil || clearNote.key != streamFlapKey(evPath) {
		t.Fatalf("sweep clear = %+v, want key %q", clearNote, streamFlapKey(evPath))
	}
	// The now-idle path is pruned so the map does not retain it.
	if len(se.paths) != 0 {
		t.Errorf("path not pruned after sweep clear: paths = %d, want 0", len(se.paths))
	}
}

// TestStreamEventsSweepPrunesIdlePaths proves the sweep drops a path whose flap
// detector has gone idle (its window aged out) even though it never flapped, so a
// removed device does not leave an entry behind.
func TestStreamEventsSweepPrunesIdlePaths(t *testing.T) {
	pub := &recordedPub{}
	now := time.Unix(0, 0)
	se := newStreamEvents(pub, func() time.Time { return now })

	se.ClientConnected(evPath, evRemote) // one connect: a partial window, no flap
	if len(se.paths) != 1 {
		t.Fatalf("paths = %d, want 1 after a connect", len(se.paths))
	}
	// Sweeping while the window is still live keeps the path.
	se.sweep(now)
	if len(se.paths) != 1 {
		t.Fatalf("path pruned too early: paths = %d, want 1", len(se.paths))
	}
	// Once the window has aged out the idle path is pruned, and nothing is cleared
	// (it never flapped).
	now = now.Add(flapWindow + time.Second)
	se.sweep(now)
	if len(se.paths) != 0 {
		t.Errorf("idle path not pruned: paths = %d, want 0", len(se.paths))
	}
	if got := pub.countTitles(opClear, "Client stopped reconnecting"); got != 0 {
		t.Errorf("a never-flapped path published %d clears, want 0", got)
	}
}

// TestStreamEventsRunSweepsSettledFlap drives the periodic Run loop end to end: a
// settled flap is cleared by the sweep on a tick, and the loop stops on cancel.
func TestStreamEventsRunSweepsSettledFlap(t *testing.T) {
	pub := &recordedPub{}
	var mu sync.Mutex
	cur := time.Unix(0, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	advance := func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }

	se := newStreamEvents(pub, clock)
	se.sweepInterval = 5 * time.Millisecond

	// Onset a flap synchronously (no Run goroutine yet), then let the quiet window
	// pass with no further connect so the sweep will clear it.
	for range flapMax + 1 {
		se.ClientConnected(evPath, evRemote)
		advance(time.Second)
	}
	advance(flapQuiet + time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { se.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for pub.countTitles(opClear, "Client stopped reconnecting") == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run did not sweep the settled flap")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

// TestStreamEventsPathsAreIndependent proves two paths keep separate flap state:
// flapping one does not suppress the other's infos.
func TestStreamEventsPathsAreIndependent(t *testing.T) {
	pub := &recordedPub{}
	now := time.Unix(0, 0)
	se := newStreamEvents(pub, func() time.Time { return now })

	for range flapMax + 1 {
		se.ClientConnected("/a", evRemote)
		now = now.Add(time.Second)
	}
	// /a is flapping now; /b is untouched, so its connect is reported normally.
	se.ClientConnected("/b", evRemote)
	found := false
	for i := range pub.notes {
		e := &pub.notes[i]
		if e.op == opPublish && e.n.Title == titleConnected && e.n.Source == "/b from "+evRemote {
			found = true
		}
	}
	if !found {
		t.Error("a connect on an unrelated path was suppressed by another path's flap")
	}
}
