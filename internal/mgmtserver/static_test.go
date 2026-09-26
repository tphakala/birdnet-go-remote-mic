package mgmtserver

import (
	"bytes"
	"compress/gzip"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	testStylesPath = "/styles.css"
)

func TestStaticHandlerServesFilesAndFallback(t *testing.T) {
	memFS := fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<!doctype html><html><body>Root SPA</body></html>"),
		},
		"styles.css": &fstest.MapFile{
			Data: []byte("body { background: #0b0f17; }"),
		},
		"app.js": &fstest.MapFile{
			Data: []byte("console.log('remote-mic');"),
		},
	}

	handler := newStaticHandler(memFS)

	tests := []struct {
		name       string
		path       string
		method     string
		wantStatus int
		wantBody   string
		wantType   string
	}{
		{
			name:       "root path serves index.html",
			path:       "/",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "Root SPA",
			wantType:   "text/html",
		},
		{
			name:       "static css file",
			path:       testStylesPath,
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "body { background: #0b0f17; }",
			wantType:   "text/css",
		},
		{
			name:       "static js file",
			path:       "/app.js",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "console.log('remote-mic');",
			wantType:   "text/javascript",
		},
		{
			name:       "spa route falls back to index.html",
			path:       "/dashboard",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "Root SPA",
			wantType:   "text/html",
		},
		{
			name:       "post method rejected",
			path:       testStylesPath,
			method:     http.MethodPost,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}

			if tc.wantStatus == http.StatusOK {
				// Check security headers
				if csp := rr.Header().Get("Content-Security-Policy"); csp != headerCSP {
					t.Errorf("Content-Security-Policy = %q, want %q", csp, headerCSP)
				}
				if nosniff := rr.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
					t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
				}
				if ref := rr.Header().Get("Referrer-Policy"); ref != "same-origin" {
					t.Errorf("Referrer-Policy = %q, want same-origin", ref)
				}
				if etag := rr.Header().Get("ETag"); etag == "" {
					t.Error("missing ETag header")
				}
				if tc.wantType != "" {
					if ctype := rr.Header().Get("Content-Type"); !strings.HasPrefix(ctype, tc.wantType) {
						t.Errorf("Content-Type = %q, want prefix %q", ctype, tc.wantType)
					}
				}
				if tc.wantBody != "" && !strings.Contains(rr.Body.String(), tc.wantBody) {
					t.Errorf("body = %q, want to contain %q", rr.Body.String(), tc.wantBody)
				}
			}
		})
	}
}

func TestStaticHandlerETagCaching(t *testing.T) {
	memFS := fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<!doctype html><html><body>Cached SPA</body></html>"),
		},
		"styles.css": &fstest.MapFile{
			Data: []byte("body { background: #0b0f17; }"),
		},
	}

	handler := newStaticHandler(memFS)

	// First request gets 200 with ETag
	req1 := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)

	etag := rr1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first request had no ETag")
	}

	tests := []struct {
		name        string
		path        string
		ifNoneMatch string
		wantStatus  int
	}{
		{
			name:        "exact match returns 304",
			path:        "/",
			ifNoneMatch: etag,
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "list with matching tag returns 304",
			path:        "/",
			ifNoneMatch: `"other-tag", ` + etag + `, "another-tag"`,
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "asterisk returns 304",
			path:        "/",
			ifNoneMatch: "*",
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "weak prefix matching strong returns 304",
			path:        "/",
			ifNoneMatch: "W/" + etag,
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "no match returns 200",
			path:        "/",
			ifNoneMatch: `"mismatched-etag"`,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "empty header returns 200",
			path:        "/",
			ifNoneMatch: "",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "static asset wildcard returns 304",
			path:        testStylesPath,
			ifNoneMatch: "*",
			wantStatus:  http.StatusNotModified,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, http.NoBody)
			if tc.ifNoneMatch != "" {
				req.Header.Set("If-None-Match", tc.ifNoneMatch)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
		})
	}
}

// TestStaticHandlerRevalidates asserts assets and the SPA fallback are sent
// with Cache-Control: no-cache, so a browser revalidates after a deploy
// instead of reusing a heuristically cached stylesheet.
func TestStaticHandlerRevalidates(t *testing.T) {
	handler := newStaticHandler(fstest.MapFS{
		indexAsset:  &fstest.MapFile{Data: []byte("<!doctype html><title>x</title>")},
		stylesAsset: &fstest.MapFile{Data: []byte("body{}")},
	})
	for _, p := range []string{"/", "/styles.css", "/system"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, http.NoBody))
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", p, got)
		}
	}
}

// gzipLicenses is a compressible asset body for the gzip tests.
var gzipLicenses = strings.Repeat(`{"name":"module","license":"MIT"},`, 200)

// gzipFixture is a static handler over a compressible index and JSON file, a
// script too small to compress, a font (already compressed), and text gzip
// cannot shrink.
func gzipFixture() http.Handler {
	return newStaticHandler(fstest.MapFS{
		indexAsset:      &fstest.MapFile{Data: []byte("<!doctype html>" + strings.Repeat("<p>x</p>", 300))},
		"licenses.json": &fstest.MapFile{Data: []byte(gzipLicenses)},
		"small.js":      &fstest.MapFile{Data: []byte("console.log(1);")},
		"font.woff2":    &fstest.MapFile{Data: []byte(gzipLicenses)},
		"noise.txt":     &fstest.MapFile{Data: noise(4096)},
	})
}

// gzipGetStatic sends a GET for path with the given Accept-Encoding and
// If-None-Match (either may be empty).
func gzipGetStatic(t *testing.T, h http.Handler, path, accept, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gunzipBody(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return string(out)
}

func TestStaticHandlerGzipsWhenAccepted(t *testing.T) {
	t.Parallel()
	rec := gzipGetStatic(t, gzipFixture(), "/licenses.json", "gzip, br", "")
	if got := rec.Header().Get("Content-Encoding"); got != encGzip {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if rec.Body.Len() >= len(gzipLicenses) {
		t.Errorf("body is %d bytes, want fewer than %d", rec.Body.Len(), len(gzipLicenses))
	}
	if gunzipBody(t, rec.Body.Bytes()) != gzipLicenses {
		t.Errorf("decompressed body differs from the asset")
	}
}

func TestStaticHandlerPlainWithoutGzip(t *testing.T) {
	t.Parallel()
	h := gzipFixture()
	for _, accept := range []string{"", "br", "gzip;q=0.0", "*;q=0"} {
		rec := gzipGetStatic(t, h, "/licenses.json", accept, "")
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want none", accept, got)
		}
		if rec.Body.String() != gzipLicenses {
			t.Errorf("Accept-Encoding %q: body differs from the asset", accept)
		}
		if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary = %q, want Accept-Encoding", accept, got)
		}
	}
}

func TestStaticHandlerGzipETag(t *testing.T) {
	t.Parallel()
	h := gzipFixture()
	plain := gzipGetStatic(t, h, "/licenses.json", "", "").Header().Get("ETag")
	zipped := gzipGetStatic(t, h, "/licenses.json", encGzip, "").Header().Get("ETag")
	if plain == "" || zipped == "" || plain == zipped {
		t.Fatalf("ETags plain %q and gzip %q, want two different ones", plain, zipped)
	}
	if rec := gzipGetStatic(t, h, "/licenses.json", encGzip, zipped); rec.Code != http.StatusNotModified {
		t.Errorf("revalidating the gzip ETag: status %d, want 304", rec.Code)
	}
	if rec := gzipGetStatic(t, h, "/licenses.json", "", zipped); rec.Code != http.StatusOK {
		t.Errorf("gzip ETag on a plain request: status %d, want 200", rec.Code)
	}
}

func TestStaticHandlerGzipsFallback(t *testing.T) {
	t.Parallel()
	rec := gzipGetStatic(t, gzipFixture(), "/system", encGzip, "")
	if got := rec.Header().Get("Content-Encoding"); got != encGzip {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if !strings.HasPrefix(gunzipBody(t, rec.Body.Bytes()), "<!doctype html>") {
		t.Errorf("fallback body is not index.html")
	}
}

func TestStaticHandlerSendsIncompressibleAsIs(t *testing.T) {
	t.Parallel()
	h := gzipFixture()
	rec := gzipGetStatic(t, h, "/noise.txt", encGzip, "")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("noise: Content-Encoding = %q, want none", got)
	}
	if rec.Body.Len() != 4096 {
		t.Errorf("noise: body is %d bytes, want the 4096 of the asset", rec.Body.Len())
	}
	// Too small, or compressed already: not even a Vary.
	for _, path := range []string{"/small.js", "/font.woff2"} {
		rec := gzipGetStatic(t, h, path, encGzip, "")
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%s: Content-Encoding = %q, want none", path, got)
		}
		if got := rec.Header().Get("Vary"); got != "" {
			t.Errorf("%s: Vary = %q, want none", path, got)
		}
	}
}

// noise returns n bytes that gzip cannot shrink, the same on every run.
func noise(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}
