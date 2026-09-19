package sse_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sse"
)

// This external test package binds the real levels.Hub and notify.Center as
// sse.Sources behind sse.Handler, the production wiring sse.Handler(hub, center)
// (see cmd/remotemic/main.go). It lives in package sse_test so it may
// import sse, levels and notify without an import cycle (levels and notify both
// import sse), and it exercises what the in-package unit tests over a fake Source
// cannot: the Hub-and-Center-to-sse integration and per-connection fan-out across
// two live connections.

// readEventName reads SSE lines until a full event terminates and returns its
// event name, failing the test if the stream ends first (for example on a request
// timeout, which bounds a stuck stream instead of hanging).
func readEventName(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case line == "":
			if name != "" {
				return name
			}
		}
	}
	t.Fatal("stream ended before a full event")
	return ""
}

// requireEvent reads until an event named want arrives, skipping any others
// (heartbeats, and levels while awaiting a notification). It caps the reads so a
// stream that never carries want fails rather than looping.
func requireEvent(t *testing.T, who string, sc *bufio.Scanner, want string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if readEventName(t, sc) == want {
			return
		}
	}
	t.Fatalf("connection %s never received a %s event", who, want)
}

// openLevelsStream issues the GET with a bounded request timeout and returns the
// response for the caller to read and close, plus the cancel to tear it down.
func openLevelsStream(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET events: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	return resp, cancel
}

// TestHandlerStreamsRealHubToConcurrentConnections opens two concurrent
// connections to one sse.Handler backed by the real levels.Hub and notify.Center.
// Each connection first receives a levels event, which proves it has subscribed to
// every source (the Handler subscribes to all sources before streaming any event),
// so both are subscribed to the center before anything is published. Publishing a
// single notification and then observing it on BOTH connections is the fan-out
// proof: the Handler mints a private forward path per request, so one published
// event reaches every live connection rather than being load-balanced to one.
func TestHandlerStreamsRealHubToConcurrentConnections(t *testing.T) {
	hub := levels.NewHub()
	hub.Meter("mic", 1) // a registered silent meter reports the floor each window

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	center := notify.NewCenter()
	srv := httptest.NewServer(sse.Handler(hub, center))
	defer srv.Close()

	respA, cancelA := openLevelsStream(t, srv.URL)
	defer func() { cancelA(); _ = respA.Body.Close() }()
	respB, cancelB := openLevelsStream(t, srv.URL)
	defer func() { cancelB(); _ = respB.Body.Close() }()

	scA, scB := bufio.NewScanner(respA.Body), bufio.NewScanner(respB.Body)

	// A levels event proves each connection has subscribed to every source, so both
	// are subscribed to the center before the single notification is published.
	requireEvent(t, "A", scA, "levels")
	requireEvent(t, "B", scB, "levels")

	center.Publish(notify.Started("integration-test"))

	// Both connections receive the one published notification: the fan-out proof.
	requireEvent(t, "A", scA, "notification")
	requireEvent(t, "B", scB, "notification")
}
