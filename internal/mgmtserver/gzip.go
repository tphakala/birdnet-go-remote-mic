package mgmtserver

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// gzipGET compresses the response to a GET (or HEAD) for exactly path when the
// client accepts gzip, and passes every other request through untouched. It is
// scoped to one route on purpose: the notification snapshot is by far the
// largest JSON body (about 165 KB for a full ring at the default 500 entries),
// while the SSE stream must never be buffered by a compressor and the small
// endpoints gain little from it. BestSpeed keeps the CPU cost low on a Pi Zero;
// JSON this repetitive compresses well even at that level.
//
// A flate compressor allocates about 800 KB of tables even at BestSpeed, so
// writers are pooled per handler and Reset onto each response rather than
// built per request. Reset also clears the error a previous response left
// behind when its client went away mid-body.
func gzipGET(path string, next http.Handler) http.Handler {
	return newGzipHandler(path, next)
}

// gzipHandler is gzipGET's handler. news counts the writers the pool built,
// so a test can see that a writer is reused.
type gzipHandler struct {
	path string
	next http.Handler
	pool sync.Pool
	news atomic.Int64
}

func newGzipHandler(path string, next http.Handler) *gzipHandler {
	h := &gzipHandler{path: path, next: next}
	h.pool.New = func() any {
		h.news.Add(1)
		// Only an invalid level fails, and BestSpeed is valid.
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gz
	}
	return h
}

func (h *gzipHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// HEAD answers with GET's headers (RFC 9110 section 9.3.2), so it takes the
	// same branch; net/http discards the body bytes of a HEAD response.
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.Path != h.path {
		h.next.ServeHTTP(w, r)
		return
	}
	// A cache between client and appliance must key on the encoding, whichever
	// one this request gets.
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.next.ServeHTTP(w, r)
		return
	}
	// The pool's New only ever returns *gzip.Writer.
	gz := h.pool.Get().(*gzip.Writer)
	gz.Reset(w)
	gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
	h.next.ServeHTTP(gw, r)
	if !gw.wroteHeader {
		// A handler that wrote nothing still answers 200, with an empty
		// compressed body.
		gw.WriteHeader(http.StatusOK)
	}
	if !gw.plain {
		// Close flushes the trailer. A write error here means the client went
		// away mid-body; there is nobody left to report it to, and the next
		// Reset clears it.
		_ = gz.Close()
	}
	// Detach from the finished response before pooling, so an idle pool does
	// not keep it (and its request, bearer token included) reachable.
	gz.Reset(io.Discard)
	h.pool.Put(gz)
}

// gzipResponseWriter routes the body through the gzip writer. The encoding is
// decided when the status is written: a status that carries no body (204,
// 304) is sent as is, since a Content-Encoding header and a gzip trailer on it
// would be wrong; any other drops the Content-Length the handler set, which
// would describe the uncompressed body.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	plain       bool // the status carries no body, so nothing is compressed
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wroteHeader {
		g.ResponseWriter.WriteHeader(status)
		return
	}
	// An informational status is not the final response.
	if status >= http.StatusContinue && status < http.StatusOK {
		g.ResponseWriter.WriteHeader(status)
		return
	}
	g.wroteHeader = true
	if status == http.StatusNoContent || status == http.StatusNotModified {
		g.plain = true
	} else {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.plain {
		return g.ResponseWriter.Write(p)
	}
	return g.gz.Write(p)
}

// FlushError is what http.ResponseController's Flush calls (it looks for it
// before Unwrap). The encoding is decided when the status is written, so a
// flush before the first write writes the status first; otherwise the
// underlying writer would commit a 200 with no Content-Encoding and the body
// that follows would be gzip bytes in a response declared plain. It then
// flushes what the compressor holds before flushing the connection.
func (g *gzipResponseWriter) FlushError() error {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if !g.plain {
		if err := g.gz.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(g.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer for what
// the wrapper does not implement itself (deadlines, hijacking). The one
// wrapped route is a single buffered JSON body; a streaming route must not be
// wrapped.
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
