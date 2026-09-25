package mgmtserver

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// encGzip is the gzip content coding, as requested and as echoed back.
const encGzip = "gzip"

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZIP", true},
		{"deflate, gzip;q=0.8, br", true},
		{"gzip;q=0", false},
		{"gzip; q=0.0", false},
		{"*", true},
		{"*;q=0", false},
		{"gzip;q=0, *", false},
		{"*;q=0, gzip", true},
		{"identity", false},
		{"br, deflate", false},
		{"gzip;q=bogus", true},
	}
	for _, c := range cases {
		if got := acceptsGzip(c.header); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

// gzipTestServer mounts a snapshot large enough that compression is worth
// checking for, behind the real Handler so the route wiring is exercised too.
func gzipTestServer(t *testing.T) http.Handler {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	snap := notify.Snapshot{BootID: testBootID, ServerTime: now, Capacity: 500, NextID: 51}
	for i := range 50 {
		snap.Notifications = append(snap.Notifications, notify.Notification{
			ID: uint64(i + 1), BootID: testBootID, Time: now, UptimeMs: int64(i) * 1000,
			Severity: notify.SeverityInfo, Category: notify.CategoryStream, Kind: notify.KindEvent,
			Title: "Client connected", Message: "A client started playing the stream",
		})
	}
	return New(&fakeProvider{}, WithNotifications(&fakeSnapshotter{snap: snap})).Handler()
}

func TestNotificationsGzipWhenAccepted(t *testing.T) {
	t.Parallel()
	h := gzipTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, BasePath+"/notifications", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != encGzip {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to name Accept-Encoding", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	var snap mgmtapi.NotificationSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decompressed body is not a snapshot: %v", err)
	}
	if len(snap.Notifications) != 50 || snap.Capacity != 500 {
		t.Errorf("snapshot = %d entries, capacity %d; want 50, 500", len(snap.Notifications), snap.Capacity)
	}
}

func TestNotificationsPlainWithoutGzip(t *testing.T) {
	t.Parallel()
	h := gzipTestServer(t)
	for _, enc := range []string{"", "gzip;q=0", "br"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, BasePath+"/notifications", http.NoBody)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want none", enc, got)
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
			t.Errorf("Accept-Encoding %q: Vary = %q, want it to name Accept-Encoding", enc, got)
		}
		var snap mgmtapi.NotificationSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
			t.Errorf("Accept-Encoding %q: plain body is not a snapshot: %v", enc, err)
		}
	}
}

func TestGzipLeavesOtherRoutesAlone(t *testing.T) {
	t.Parallel()
	h := gzipTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, BasePath+"/status", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("GET /status Content-Encoding = %q, want none", got)
	}
	if got := rec.Header().Get("Vary"); strings.Contains(got, "Accept-Encoding") {
		t.Errorf("GET /status Vary = %q, want no Accept-Encoding", got)
	}
}

// TestNotificationsGzipTwiceFromPool serves two compressed responses in a row,
// so the second one runs on a pooled writer: each body must decode on its own,
// with nothing of the first response carried into the second.
func TestNotificationsGzipTwiceFromPool(t *testing.T) {
	t.Parallel()
	h := gzipTestServer(t)
	for i := range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, BasePath+"/notifications", http.NoBody)
		req.Header.Set("Accept-Encoding", encGzip)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("request %d: body is not gzip: %v", i, err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("request %d: decompress: %v", i, err)
		}
		var snap mgmtapi.NotificationSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatalf("request %d: not a single snapshot: %v", i, err)
		}
		if len(snap.Notifications) != 50 {
			t.Errorf("request %d: got %d entries, want 50", i, len(snap.Notifications))
		}
	}
}

// TestNotificationsHeadMatchesGet checks that HEAD answers with the headers GET
// would send (RFC 9110 section 9.3.2), so a cache probing with HEAD sees the
// same representation.
func TestNotificationsHeadMatchesGet(t *testing.T) {
	t.Parallel()
	h := gzipTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodHead, BasePath+"/notifications", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != encGzip {
		t.Errorf("HEAD Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("HEAD Vary = %q, want it to name Accept-Encoding", got)
	}
	// The headers above would also pass on an error answer with a body; HEAD
	// must reach the snapshot handler.
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("HEAD Content-Type = %q, want the GET's application/json", got)
	}
}

// TestGzipDropsContentLengthOnWrite covers a handler that sets Content-Length
// and writes without calling WriteHeader, so only Write can drop the header.
func TestGzipDropsContentLengthOnWrite(t *testing.T) {
	t.Parallel()
	body := strings.Repeat(`{"k":"v"}`, 100)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	gzipGET("/x", inner).ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want none on a gzip body", got)
	}
}

// TestNotificationsUnauthorizedNotCompressed pins the wiring order: the bearer
// gate wraps compression, so a rejected request gets a plain 401.
func TestNotificationsUnauthorizedNotCompressed(t *testing.T) {
	t.Parallel()
	s := New(&fakeProvider{}, WithAuth(auth.NewGuard(testAuthToken)), WithNotifications(&fakeSnapshotter{}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, BasePath+"/notifications", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("401 Content-Encoding = %q, want none", got)
	}
}

// TestGzipDropsContentLength uses a handler that declares the uncompressed
// length: the header must not survive, since it would describe the wrong body.
func TestGzipDropsContentLength(t *testing.T) {
	t.Parallel()
	body := strings.Repeat(`{"k":"v"}`, 100)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	gzipGET("/x", inner).ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want none on a gzip body", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil || string(raw) != body {
		t.Errorf("decompressed body = %q (err %v), want the handler's body", raw, err)
	}
}

// TestGzipBodylessStatusStaysPlain pins that a status that carries no body
// (204, 304) goes out without a Content-Encoding header or a gzip trailer,
// while a handler that writes nothing at all still answers a valid (empty)
// gzip body.
func TestGzipBodylessStatusStaysPlain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
		req.Header.Set("Accept-Encoding", encGzip)
		rec := httptest.NewRecorder()
		gzipGET("/x", inner).ServeHTTP(rec, req)
		if rec.Code != status {
			t.Errorf("status = %d, want %d", rec.Code, status)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%d: Content-Encoding = %q, want none", status, got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%d: body has %d bytes, want none", status, rec.Body.Len())
		}
	}

	silent := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	gzipGET("/x", silent).ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); rec.Code != http.StatusOK || got != encGzip {
		t.Fatalf("silent handler: status %d, Content-Encoding %q; want 200 gzip", rec.Code, got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("silent handler body is not gzip: %v", err)
	}
	if raw, err := io.ReadAll(zr); err != nil || len(raw) != 0 {
		t.Errorf("silent handler body = %q (err %v), want empty", raw, err)
	}
}

// TestGzipFlushBeforeWrite pins that a flush before the first write still
// sends a gzip-encoded response: the status goes out with Content-Encoding,
// and the body that follows decodes.
func TestGzipFlushBeforeWrite(t *testing.T) {
	t.Parallel()
	body := strings.Repeat(`{"k":"v"}`, 100)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		_, _ = io.WriteString(w, body)
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set("Accept-Encoding", encGzip)
	rec := httptest.NewRecorder()
	gzipGET("/x", inner).ServeHTTP(rec, req)
	// Result holds the headers as they were sent; rec.Header is the live map
	// a late WriteHeader could still change.
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	if got := res.Header.Get("Content-Encoding"); got != encGzip {
		t.Fatalf("Content-Encoding sent = %q after an early flush, want gzip", got)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	if raw, err := io.ReadAll(zr); err != nil || string(raw) != body {
		t.Errorf("decompressed body = %q (err %v), want the handler's body", raw, err)
	}
}
