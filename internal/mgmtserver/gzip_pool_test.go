package mgmtserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
)

// TestGzipReusesPooledWriter pins the pooling: a flate compressor costs about
// 800 KB of tables, so sequential requests must share writers rather than
// build one each. sync.Pool promises no reuse, so the test takes away the two
// things that empty it: a GC (turned off for the test, which is therefore not
// parallel) and, under the race detector, a Put dropped at random about one
// time in four, which the bound leaves room for while still failing a
// handler that builds a writer per request.
func TestGzipReusesPooledWriter(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const requests = 100
	body := strings.Repeat(`{"k":"v"}`, 100)
	h := newGzipHandler("/x", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	for range requests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
		req.Header.Set("Accept-Encoding", encGzip)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := h.news.Load(); n > requests/2 {
		t.Errorf("built %d gzip writers for %d sequential requests, want at most %d", n, requests, requests/2)
	}
}
