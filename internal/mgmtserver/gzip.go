package mgmtserver

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipGET compresses the response to a GET (or HEAD) for exactly path when the
// client accepts gzip, and passes every other request through untouched. It is
// scoped to one route on purpose: the notification snapshot is by far the
// largest JSON body (about 165 KB for a full ring at the default 500 entries),
// while the SSE stream must never be buffered by a compressor and the small
// endpoints gain little from it. BestSpeed keeps the CPU cost low on a Pi Zero;
// JSON this repetitive compresses well even at that level.
//
// A flate compressor allocates about 800 KB of tables whatever the level, so
// writers are pooled per handler and Reset onto each response rather than
// built per request. Reset also clears the error a previous response left
// behind when its client went away mid-body.
func gzipGET(path string, next http.Handler) http.Handler {
	pool := sync.Pool{New: func() any {
		// Only an invalid level fails, and BestSpeed is valid.
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gz
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HEAD answers with GET's headers (RFC 9110 section 9.3.2), so it takes the
		// same branch; net/http discards the body bytes of a HEAD response.
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.Path != path {
			next.ServeHTTP(w, r)
			return
		}
		// A cache between client and appliance must key on the encoding, whichever
		// one this request gets.
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		// The pool's New only ever returns *gzip.Writer.
		gz := pool.Get().(*gzip.Writer)
		gz.Reset(w)
		w.Header().Set("Content-Encoding", "gzip")
		gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		next.ServeHTTP(gw, r)
		// Close flushes the trailer. A write error here means the client went
		// away mid-body; there is nobody left to report it to, and the next
		// Reset clears it.
		_ = gz.Close()
		pool.Put(gz)
	})
}

// gzipResponseWriter routes the body through the gzip writer. WriteHeader drops
// any Content-Length the handler set, since it would describe the uncompressed
// body.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	g.Header().Del("Content-Length")
	return g.gz.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer. A Flush
// through it is not gzip-aware (bytes still inside the compressor stay there),
// which is fine for the one wrapped route, a single buffered JSON body; a
// streaming route must not be wrapped.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// acceptsGzip reports whether an Accept-Encoding header allows gzip: a gzip or
// "*" coding whose quality is not zero. An explicit gzip entry wins over "*".
func acceptsGzip(header string) bool {
	star := false
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding != "gzip" && coding != "*" {
			continue
		}
		ok := qualityNonZero(params)
		if coding == "gzip" {
			return ok
		}
		star = ok
	}
	return star
}

// qualityNonZero parses the parameters after a coding ("q=0.5") and reports
// whether the quality is above zero. A missing or unparseable q counts as 1,
// the RFC 9110 default, so an odd header does not turn compression off. An
// out-of-range value that still parses (NaN, a negative) turns it off, which
// only costs bandwidth: a plain response is always valid.
func qualityNonZero(params string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return true
		}
		return q > 0
	}
	return true
}
