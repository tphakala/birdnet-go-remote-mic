//go:build !race

// The race detector makes sync.Pool drop a Put at random, so writer reuse is
// only observable without it.

package mgmtserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGzipReusesPooledWriter pins the pooling: a flate compressor costs about
// 800 KB of tables, so sequential requests must share one writer rather than
// build one each.
func TestGzipReusesPooledWriter(t *testing.T) {
	t.Parallel()
	body := strings.Repeat(`{"k":"v"}`, 100)
	h := newGzipHandler("/x", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	for range 3 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
		req.Header.Set("Accept-Encoding", encGzip)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := h.news.Load(); n != 1 {
		t.Errorf("built %d gzip writers for 3 sequential requests, want 1", n)
	}
}
