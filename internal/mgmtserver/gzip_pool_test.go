package mgmtserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGzipReusesPooledWriter pins the pooling: a flate compressor costs about
// 800 KB of tables, so sequential requests must share writers rather than
// build one each. The race detector makes sync.Pool drop a Put at random
// (about one in four), so the bound leaves room for that and still fails a
// handler that builds a writer per request.
func TestGzipReusesPooledWriter(t *testing.T) {
	t.Parallel()
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
