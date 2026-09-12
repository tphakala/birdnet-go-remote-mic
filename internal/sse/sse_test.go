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

// fakeSource is a test Source with a manually driven channel and cancel
// tracking, so a test can assert the Handler unsubscribed when the stream ends.
// Every Subscribe returns the SAME channel and shares one cancel flag, so it is
// for single-subscription use only; a two-connection test needs one fakeSource
// per connection, or the real levels.Hub (which mints an independent channel per
// Subscribe).
type fakeSource struct {
	ch           chan Event
	cancelCalled atomic.Bool
}

func newFakeSource(buf int) *fakeSource { return &fakeSource{ch: make(chan Event, buf)} }

func (f *fakeSource) Subscribe() (events <-chan Event, cancel func()) {
	return f.ch, func() { f.cancelCalled.Store(true) }
}

func (f *fakeSource) emit(ev Event)      { f.ch <- ev }
func (f *fakeSource) wasCancelled() bool { return f.cancelCalled.Load() }

var _ Source = (*fakeSource)(nil)

// ctrlWriter is a controllable ResponseWriter for unit tests: it records what
// was written, can fail a write, records a flush, and records that a write
// deadline was set. It implements http.Flusher and SetWriteDeadline so
// http.NewResponseController drives it.
type ctrlWriter struct {
	hdr         http.Header
	code        int
	buf         bytes.Buffer
	writeErr    error
	flushed     bool
	deadlineSet bool
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
func (c *ctrlWriter) Flush()                           { c.flushed = true }
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

func TestWriteEventSuccess(t *testing.T) {
	cw := &ctrlWriter{}
	rc := http.NewResponseController(cw)
	h := &handler{writeTimeout: time.Second}

	if !h.writeEvent(cw, rc, cw, Event{Name: evLevels, Data: []byte(`{"a":1}`)}) {
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

	if h.writeEvent(cw, rc, cw, Event{Name: "x", Data: []byte("{}")}) {
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
