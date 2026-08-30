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
			name:        "asterisk element inside list returns 304",
			path:        "/",
			ifNoneMatch: `"other-tag", *, "another-tag"`,
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
			name:        "static asset exact match returns 304",
			path:        "/styles.css",
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

func TestEtagMatches(t *testing.T) {
	tests := []struct {
		name        string
		ifNoneMatch string
		etag        string
		want        bool
	}{
		{
			name:        "exact match returns true",
			ifNoneMatch: `"abc123"`,
			etag:        `"abc123"`,
			want:        true,
		},
		{
			name:        "list with matching tag returns true",
			ifNoneMatch: `"tag1", "abc123", "tag2"`,
			etag:        `"abc123"`,
			want:        true,
		},
		{
			name:        "asterisk returns true",
			ifNoneMatch: "*",
			etag:        `"abc123"`,
			want:        true,
		},
		{
			name:        "asterisk element inside list returns true",
			ifNoneMatch: `"tag1", *, "tag2"`,
			etag:        `"abc123"`,
			want:        true,
		},
		{
			name:        "weak prefix matching strong returns true",
			ifNoneMatch: `W/"abc123"`,
			etag:        `"abc123"`,
			want:        true,
		},
		{
			name:        "weak target etag matching strong header returns true",
			ifNoneMatch: `"abc123"`,
			etag:        `W/"abc123"`,
			want:        true,
		},
		{
			name:        "no match returns false",
			ifNoneMatch: `"nomatch1", "nomatch2"`,
			etag:        `"abc123"`,
			want:        false,
		},
		{
			name:        "empty header returns false",
			ifNoneMatch: "",
			etag:        `"abc123"`,
			want:        false,
		},
		{
			name:        "empty target etag returns false",
			ifNoneMatch: `"abc123"`,
			etag:        "",
			want:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := etagMatches(tc.ifNoneMatch, tc.etag)
			if got != tc.want {
				t.Errorf("etagMatches(%q, %q) = %v, want %v", tc.ifNoneMatch, tc.etag, got, tc.want)
			}
		})
	}
}
