package mgmtserver

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

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
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
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
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("GET /status Content-Encoding = %q, want none", got)
	}
	if got := rec.Header().Get("Vary"); strings.Contains(got, "Accept-Encoding") {
		t.Errorf("GET /status Vary = %q, want no Accept-Encoding", got)
	}
}
