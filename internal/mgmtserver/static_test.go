package mgmtserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
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
			path:       "/styles.css",
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
			path:       "/styles.css",
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

	// Second request with matching If-None-Match gets 304 Not Modified
	req2 := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req2.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304 Not Modified", rr2.Code)
	}
}
