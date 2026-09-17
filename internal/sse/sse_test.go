package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Event type names used across the tests, named to keep goconst quiet and the
// assertions self-documenting.
const (
	evLevels       = "levels"
	evNotification = "notification"
)

// fakeSource is a test Source that mints a fresh buffered channel per Subscribe
// and fans every emit out to all live subscriptions, so one fakeSource can back
// several concurrent Handler connections (like the real levels.Hub). It tracks the
// live subscription count so a test can wait until a connection has subscribed
// before emitting, and records whether any cancel ran so a test can assert the
// Handler unsubscribed when a stream ended.
type fakeSource struct {
	buf          int
	mu           sync.Mutex
	subs         map[chan Event]struct{}
	cancelCalled atomic.Bool
}

func newFakeSource(buf int) *fakeSource {
	return &fakeSource{buf: buf, subs: make(map[chan Event]struct{})}
}

func (f *fakeSource) Subscribe() (events <-chan Event, cancel func()) {
	ch := make(chan Event, f.buf)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
			f.cancelCalled.Store(true)
		})
	}
}

// emit fans one event out to every live subscription. It snapshots the channels
// under the lock and sends outside it, so a concurrent cancel never blocks behind
// a send. Each send is non-blocking: a subscriber whose buffer is full drops the
// event, matching the real Hub.broadcast rather than stalling the producer. The
// test buffers are ample, so nothing is actually dropped here. An emit with no
// live subscription is dropped, so a test that needs the event delivered calls
// waitSubscribed first.
func (f *fakeSource) emit(ev Event) {
	f.mu.Lock()
	chs := make([]chan Event, 0, len(f.subs))
	for ch := range f.subs {
		chs = append(chs, ch)
	}
	f.mu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *fakeSource) wasCancelled() bool { return f.cancelCalled.Load() }

func (f *fakeSource) subCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// waitSubscribed blocks until at least n subscriptions are live, so an emit that
// must reach a connection is not sent before the Handler has subscribed (the
// Handler subscribes after flushing the response header, which is what unblocks
// the client's GET).
func (f *fakeSource) waitSubscribed(t *testing.T, n int) {
	t.Helper()
	waitFor(t, func() bool { return f.subCount() >= n }, 2*time.Second)
}

var _ Source = (*fakeSource)(nil)

// ctrlWriter is a controllable ResponseWriter for unit tests: it records what
// was written, can fail a write, records a flush, and records that a write
// deadline was set. It implements http.Flusher and SetWriteDeadline so
// http.NewResponseController drives it.
type ctrlWriter struct {
	hdr           http.Header
	code          int
	buf           bytes.Buffer
	writeErr      error
	flushErr      error
	flushErrAfter int // let this many flushes succeed before flushErr kicks in
	flushes       int
	flushed       bool
	deadlineSet   bool
}

func (c *ctrlWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}
func (c *ctrlWriter) WriteHeader(code int) { c.code = code }
func (c *ctrlWriter) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.buf.Write(p)
}
func (c *ctrlWriter) Flush() { c.flushed = true }

// FlushError makes http.NewResponseController(c).Flush() observe a flush error
// (the native net/http writers expose FlushError, which the controller prefers
// over Flush). It counts flushes so a test can let the initial post-header flush
// succeed and fail a later per-event flush via flushErrAfter.
func (c *ctrlWriter) FlushError() error {
	c.flushed = true
	c.flushes++
	if c.flushErr != nil && c.flushes > c.flushErrAfter {
		return c.flushErr
	}
	return nil
}
func (c *ctrlWriter) SetWriteDeadline(time.Time) error { c.deadlineSet = true; return nil }

// noFlushRecorder is a ResponseWriter that does NOT implement http.Flusher, to
// exercise the streaming-unsupported branch.
type noFlushRecorder struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func (n *noFlushRecorder) Header() http.Header {
	if n.hdr == nil {
		n.hdr = http.Header{}
	}
	return n.hdr
}
func (n *noFlushRecorder) WriteHeader(code int)        { n.code = code }
func (n *noFlushRecorder) Write(p []byte) (int, error) { return n.buf.Write(p) }

// sseReader reads SSE events one at a time off a persistent scanner.
type sseReader struct{ sc *bufio.Scanner }

func newSSEReader(r io.Reader) *sseReader { return &sseReader{sc: bufio.NewScanner(r)} }

func (s *sseReader) next(t *testing.T) (name, data string) {
	t.Helper()
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if name != "" {
				return name, data
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	t.Fatal("stream ended before a full event")
	return "", ""
}

func waitFor(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// openStream issues the GET, checks the content type, and returns the response
// for the caller to read and close (returning the *http.Response keeps the body
// the caller's to close, which bodyclose is happy with).
func openStream(t *testing.T, url string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		_ = resp.Body.Close()
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	return resp
}

func TestHandlerStreamsEventsInOrder(t *testing.T) {
	fake := newFakeSource(8)
	h := &handler{sources: []Source{fake}, heartbeat: time.Hour, writeTimeout: time.Second, mergeBuffer: 32}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := openStream(t, srv.URL)
	defer func() { _ = resp.Body.Close() }()
	rd := newSSEReader(resp.Body)

	fake.waitSubscribed(t, 1)
	fake.emit(Event{Name: evLevels, Data: []byte(`{"n":1}`)})
	fake.emit(Event{Name: evLevels, Data: []byte(`{"n":2}`)})

	if n, d := rd.next(t); n != evLevels || d != `{"n":1}` {
		t.Fatalf("event 1 = (%q,%q), want (levels,{\"n\":1})", n, d)
	}
	if n, d := rd.next(t); n != evLevels || d != `{"n":2}` {
		t.Fatalf("event 2 = (%q,%q), want (levels,{\"n\":2})", n, d)
	}
}

func TestHandlerHeartbeatOnIdleStream(t *testing.T) {
	h := &handler{sources: []Source{newFakeSource(1)}, heartbeat: 20 * time.Millisecond, writeTimeout: time.Second, mergeBuffer: 8}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := openStream(t, srv.URL)
	defer func() { _ = resp.Body.Close() }()
	rd := newSSEReader(resp.Body)

	if n, d := rd.next(t); n != heartbeatName || d != "{}" {
		t.Fatalf("idle event = (%q,%q), want (heartbeat,{})", n, d)
	}
}

func TestHandlerHeartbeatSurvivesFilter(t *testing.T) {
	fake := newFakeSource(4)
	h := &handler{sources: []Source{fake}, heartbeat: 20 * time.Millisecond, writeTimeout: time.Second, mergeBuffer: 8}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Subscribe to a type that never fires, and emit a levels event that the
	// filter must drop; the heartbeat must still arrive.
	resp := openStream(t, srv.URL+"?events=nonexistent")
	defer func() { _ = resp.Body.Close() }()
	rd := newSSEReader(resp.Body)
	fake.waitSubscribed(t, 1)
	fake.emit(Event{Name: evLevels, Data: []byte("{}")})

	if n, _ := rd.next(t); n != heartbeatName {
		t.Fatalf("first event = %q, want heartbeat (levels filtered out)", n)
	}
}

func TestHandlerTwoSourcesInterleaveWithoutLoss(t *testing.T) {
	a, b := newFakeSource(8), newFakeSource(8)
	h := &handler{sources: []Source{a, b}, heartbeat: time.Hour, writeTimeout: time.Second, mergeBuffer: 32}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := openStream(t, srv.URL)
	defer func() { _ = resp.Body.Close() }()
	rd := newSSEReader(resp.Body)

	a.waitSubscribed(t, 1)
	b.waitSubscribed(t, 1)
	const each = 4
	for i := 0; i < each; i++ {
		a.emit(Event{Name: evLevels, Data: []byte("{}")})
		b.emit(Event{Name: evNotification, Data: []byte("{}")})
	}

	counts := map[string]int{}
	for i := 0; i < each*2; i++ {
		n, _ := rd.next(t)
		counts[n]++
	}
	if counts[evLevels] != each || counts[evNotification] != each {
		t.Fatalf("counts = %v, want %d each", counts, each)
	}
}

// TestHandlerFansOutToConcurrentConnections opens two connections to one Handler
// over one multi-subscription source and asserts each connection independently
// receives every event: the Handler mints a private forward path per request, so
// one connection cannot starve or steal another's events.
func TestHandlerFansOutToConcurrentConnections(t *testing.T) {
	fake := newFakeSource(8)
	h := &handler{sources: []Source{fake}, heartbeat: time.Hour, writeTimeout: time.Second, mergeBuffer: 32}
	srv := httptest.NewServer(h)
	defer srv.Close()

	respA := openStream(t, srv.URL)
	defer func() { _ = respA.Body.Close() }()
	respB := openStream(t, srv.URL)
	defer func() { _ = respB.Body.Close() }()
	rdA, rdB := newSSEReader(respA.Body), newSSEReader(respB.Body)

	// Both connections must be subscribed before the first emit, or the fan-out would
	// miss the connection that had not subscribed yet.
	fake.waitSubscribed(t, 2)

	const n = 3
	for i := 0; i < n; i++ {
		fake.emit(Event{Name: evLevels, Data: []byte("{}")})
	}
	for i := 0; i < n; i++ {
		if name, _ := rdA.next(t); name != evLevels {
			t.Fatalf("connection A event %d = %q, want levels", i, name)
		}
		if name, _ := rdB.next(t); name != evLevels {
			t.Fatalf("connection B event %d = %q, want levels", i, name)
		}
	}
}

func TestHandlerCancelUnsubscribesSources(t *testing.T) {
	fake := newFakeSource(4)
	h := &handler{sources: []Source{fake}, heartbeat: 20 * time.Millisecond, writeTimeout: time.Second, mergeBuffer: 8}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET events: %v", err)
	}
	// Read one heartbeat so the handler is definitely looping and subscribed.
	if n, _ := newSSEReader(resp.Body).next(t); n != heartbeatName {
		t.Fatalf("first event = %q, want heartbeat", n)
	}
	cancel()
	_ = resp.Body.Close()

	waitFor(t, fake.wasCancelled, time.Second)
}

func TestServeHTTPNonFlusherWritesProblem(t *testing.T) {
	w := &noFlushRecorder{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", http.NoBody)
	Handler(newFakeSource(1)).ServeHTTP(w, req)

	if w.code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type = %q, want application/problem+json", ct)
	}
	var pd problemDetail
	if err := json.Unmarshal(w.buf.Bytes(), &pd); err != nil {
		t.Fatalf("body is not problem+json: %v", err)
	}
	if pd.Status != http.StatusInternalServerError || pd.Title != "streaming unsupported" {
		t.Fatalf("problem = %+v, want status 500 title 'streaming unsupported'", pd)
	}
}

func TestServeHTTPEndsStreamOnWriteError(t *testing.T) {
	fake := newFakeSource(4)
	cw := &ctrlWriter{writeErr: errors.New("client gone")}
	h := &handler{sources: []Source{fake}, heartbeat: time.Hour, writeTimeout: time.Second, mergeBuffer: 8}
	// ServeHTTP derives its own cancellable context from the request, so it tears
	// down the forward goroutine on return even for this direct (server-less) call.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", http.NoBody)

	done := make(chan struct{})
	go func() { h.ServeHTTP(cw, req); close(done) }()

	fake.waitSubscribed(t, 1)
	fake.emit(Event{Name: evLevels, Data: []byte("{}")})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end on write error")
	}
	if !fake.wasCancelled() {
		t.Fatal("source must be unsubscribed when the stream ends")
	}
}

func TestServeHTTPEndsStreamOnHeartbeatWriteError(t *testing.T) {
	fake := newFakeSource(1)
	cw := &ctrlWriter{writeErr: errors.New("client gone")}
	h := &handler{sources: []Source{fake}, heartbeat: 10 * time.Millisecond, writeTimeout: time.Second, mergeBuffer: 8}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", http.NoBody)

	done := make(chan struct{})
	go func() { h.ServeHTTP(cw, req); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end when the heartbeat write failed")
	}
	if !fake.wasCancelled() {
		t.Fatal("source must be unsubscribed when the stream ends")
	}
}

func TestServeHTTPEndsStreamOnFlushError(t *testing.T) {
	fake := newFakeSource(4)
	// Let the initial post-header flush succeed, then fail the first event flush.
	cw := &ctrlWriter{flushErr: errors.New("flush failed"), flushErrAfter: 1}
	h := &handler{sources: []Source{fake}, heartbeat: time.Hour, writeTimeout: time.Second, mergeBuffer: 8}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", http.NoBody)

	done := make(chan struct{})
	go func() { h.ServeHTTP(cw, req); close(done) }()

	fake.waitSubscribed(t, 1)
	fake.emit(Event{Name: evLevels, Data: []byte("{}")})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end when a flush failed")
	}
	if !fake.wasCancelled() {
		t.Fatal("source must be unsubscribed when a flush fails")
	}
}

func TestServeHTTPStopsWhenInitialFlushFails(t *testing.T) {
	fake := newFakeSource(1)
	// flushErrAfter defaults to 0, so the initial post-header flush already fails.
	cw := &ctrlWriter{flushErr: errors.New("client gone")}
	h := &handler{sources: []Source{fake}, heartbeat: time.Hour, writeTimeout: time.Second, mergeBuffer: 8}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", http.NoBody)

	done := make(chan struct{})
	go func() { h.ServeHTTP(cw, req); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return when the initial flush failed")
	}
	// The handler returns before subscribing, so no source was ever registered.
	if fake.wasCancelled() {
		t.Fatal("no source should be subscribed when the initial flush fails")
	}
}

func TestWriteEventSuccess(t *testing.T) {
	cw := &ctrlWriter{}
	rc := http.NewResponseController(cw)
	h := &handler{writeTimeout: time.Second}

	if !h.writeEvent(cw, rc, Event{Name: evLevels, Data: []byte(`{"a":1}`)}) {
		t.Fatal("writeEvent should return true on success")
	}
	if got, want := cw.buf.String(), "event: levels\ndata: {\"a\":1}\n\n"; got != want {
		t.Fatalf("wire format = %q, want %q", got, want)
	}
	if !cw.flushed {
		t.Fatal("writeEvent must flush")
	}
	if !cw.deadlineSet {
		t.Fatal("writeEvent must set a write deadline")
	}
}

func TestWriteEventReturnsFalseOnError(t *testing.T) {
	cw := &ctrlWriter{writeErr: errors.New("boom")}
	rc := http.NewResponseController(cw)
	h := &handler{writeTimeout: time.Second}

	if h.writeEvent(cw, rc, Event{Name: "x", Data: []byte("{}")}) {
		t.Fatal("writeEvent should return false on write error")
	}
	if !cw.deadlineSet {
		t.Fatal("writeEvent must set a write deadline even when the write fails")
	}
}

func TestForwardDeliversThenExitsOnCancelWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan Event, 1)
	out := make(chan Event, 1)
	done := make(chan struct{})
	go func() { forward(ctx, in, out); close(done) }()

	in <- Event{Name: evLevels}
	if got := <-out; got.Name != evLevels {
		t.Fatalf("forwarded event = %q, want levels", got.Name)
	}
	cancel() // no pending input: the outer select must exit on ctx.Done
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward did not exit on cancel while waiting for input")
	}
}

func TestForwardExitsOnCancelMidSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan Event)  // unbuffered: send returns only once forward has it
	out := make(chan Event) // unbuffered, never read: forward parks on the send
	done := make(chan struct{})
	go func() { forward(ctx, in, out); close(done) }()

	in <- Event{Name: evLevels} // unbuffered: returns only once forward has received it
	cancel()                    // forward is now at the inner send on out (no reader); it must exit on ctx.Done
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward did not exit on cancel during a blocked send")
	}
}

func TestParseEventFilter(t *testing.T) {
	for _, q := range []string{"", "   ", " , ,"} {
		if f := parseEventFilter(q); !f.all {
			t.Errorf("parseEventFilter(%q) should allow all", q)
		}
	}
	f := parseEventFilter("levels, notification")
	if f.all {
		t.Fatal("a named filter must not be all")
	}
	if !f.allows(evLevels) || !f.allows(evNotification) {
		t.Error("named types must pass their own filter")
	}
	if f.allows("other") {
		t.Error("an unlisted type must not pass")
	}
	if !f.allows(heartbeatName) {
		t.Error("heartbeat must always pass a named filter")
	}
	if !parseEventFilter("").allows("anything") {
		t.Error("the all filter must pass any type")
	}
}
