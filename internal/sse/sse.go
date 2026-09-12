// Package sse multiplexes named application events onto a single
// text/event-stream HTTP response. It is a platform-neutral leaf: it depends
// only on the standard library and knows nothing about audio, ALSA, or the
// management API. Producers (the levels hub, later the notification center)
// implement Source; one Handler fans any number of sources onto one
// connection, with a per-connection heartbeat and an optional ?events= name
// filter. Coupling runs one way: producers import sse, never the reverse.
package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Event is one SSE event: a type name and its already-marshaled JSON data. It
// is written to the wire as "event: <Name>\ndata: <Data>\n\n", so Data must be a
// single line with no embedded newlines; producers satisfy this by marshaling
// compact JSON (encoding/json.Marshal never emits a newline).
type Event struct {
	Name string
	Data []byte
}

// Source is a producer of events a Handler can stream. Subscribe registers a
// consumer and returns its event channel plus a cancel func that unregisters
// it; the channel is never closed, so a consumer stops by calling cancel (the
// Handler does so when the request ends). An implementation must not stall the
// producer on a slow consumer: a full subscriber buffer drops events instead.
type Source interface {
	Subscribe() (events <-chan Event, cancel func())
}

const (
	// defaultHeartbeat is how often an idle connection emits a heartbeat so
	// clients can detect a dead server and proxies keep the connection open.
	defaultHeartbeat = 15 * time.Second
	// defaultWriteTimeout bounds a single event write so a stuck client cannot
	// park the writer forever, without killing the long-lived stream the way the
	// server's WriteTimeout would.
	defaultWriteTimeout = 5 * time.Second
	// defaultMergeBuffer is the depth of the per-connection channel that every
	// source forwards into. It absorbs a burst across sources; a slow client
	// that fills it causes drops at the source rather than blocking a producer.
	defaultMergeBuffer = 32
	// heartbeatName is the reserved event type that always passes the filter.
	heartbeatName = "heartbeat"
)

// Handler returns an http.Handler that streams every source's events onto one
// text/event-stream response, with a 15 s heartbeat and the ?events= name
// filter (heartbeats always pass). Mount it beside the generated API handler;
// each request runs until the client disconnects.
func Handler(sources ...Source) http.Handler {
	return &handler{
		sources:      sources,
		heartbeat:    defaultHeartbeat,
		writeTimeout: defaultWriteTimeout,
		mergeBuffer:  defaultMergeBuffer,
	}
}

// handler is the concrete Handler. Its tunable knobs are unexported fields so
// tests in this package can drive it with a short heartbeat, a short write
// deadline, and a shallow merge buffer; the exported Handler constructor fixes
// them at the production defaults.
type handler struct {
	sources      []Source
	heartbeat    time.Duration
	writeTimeout time.Duration
	mergeBuffer  int
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		// A real HTTP/1.1 or HTTP/2 server always supports flushing; this branch
		// only guards a ResponseWriter that cannot stream, reported as the same
		// RFC 9457 problem+json shape as the rest of the API.
		writeProblem(w, http.StatusInternalServerError, "streaming unsupported",
			"this server cannot stream server-sent events")
		return
	}
	filter := parseEventFilter(r.URL.Query().Get("events"))

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Flush through the ResponseController so a failed flush is observed rather
	// than dropped: the native net/http writers implement FlushError, so this
	// reports a client that has already gone before the first event.
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}

	// Bind the forwarders to a context we cancel on return, so every forward
	// goroutine is torn down when ServeHTTP exits, regardless of how the caller
	// manages r.Context(). Under net/http the request context is already
	// cancelled on return; deriving our own also covers a direct ServeHTTP call.
	ctx, cancelForward := context.WithCancel(r.Context())
	defer cancelForward()
	merged := make(chan Event, h.mergeBuffer)
	// One forwarding goroutine per source drains it into the shared merge
	// channel; each returns on ctx.Done() (a source never closes its channel).
	// Unsubscribe every source when the handler returns, in one deferred cleanup.
	var cancels []func()
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for _, src := range h.sources {
		ch, cancel := src.Subscribe()
		cancels = append(cancels, cancel)
		go forward(ctx, ch, merged)
	}

	hb := time.NewTicker(h.heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-merged:
			if !filter.allows(ev.Name) {
				continue
			}
			if !h.writeEvent(w, rc, ev) {
				return
			}
		case <-hb.C:
			if !h.writeEvent(w, rc, Event{Name: heartbeatName, Data: []byte("{}")}) {
				return
			}
		}
	}
}

// forward drains one source channel into the shared merge channel until the
// request context is cancelled. When the client is slow and merged is full the
// send blocks here, so the source drops for this consumer (its own buffer
// fills) rather than the producer stalling.
func forward(ctx context.Context, in <-chan Event, out chan<- Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-in:
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}

// writeEvent writes one event with a bounded per-write deadline and flushes,
// both through the ResponseController so a broken connection surfaces as an
// error. It returns false on any write or flush error so the caller ends the
// stream.
func (h *handler) writeEvent(w http.ResponseWriter, rc *http.ResponseController, ev Event) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(h.writeTimeout))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
		return false
	}
	if err := rc.Flush(); err != nil {
		return false
	}
	return true
}

// problemDetail is the subset of an RFC 9457 problem document this leaf emits.
// It mirrors the management API's Problem shape (status, title, detail) so the
// one non-streaming error path stays consistent with the rest of the API,
// without this package importing the generated types.
type problemDetail struct {
	Status int    `json:"status,omitempty"`
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// writeProblem renders an RFC 9457 problem detail. It is used only for the
// non-Flusher branch, which a real server never reaches.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDetail{Status: status, Title: title, Detail: detail})
}

// eventFilter decides which event types a client receives. Heartbeats are
// always delivered so an idle connection stays alive regardless of the filter.
type eventFilter struct {
	all   bool
	names map[string]bool
}

// parseEventFilter reads the ?events= query value: empty means every type, and
// otherwise a comma-separated allow-list of type names.
func parseEventFilter(q string) eventFilter {
	if strings.TrimSpace(q) == "" {
		return eventFilter{all: true}
	}
	names := make(map[string]bool)
	for _, p := range strings.Split(q, ",") {
		if p = strings.TrimSpace(p); p != "" {
			names[p] = true
		}
	}
	if len(names) == 0 {
		return eventFilter{all: true}
	}
	return eventFilter{names: names}
}

func (f eventFilter) allows(name string) bool {
	if name == heartbeatName {
		return true
	}
	return f.all || f.names[name]
}
