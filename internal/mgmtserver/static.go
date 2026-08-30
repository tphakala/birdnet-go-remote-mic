package mgmtserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// Security headers applied to all static asset responses.
const (
	// Fonts are embedded and served from the binary, so no external hosts are
	// permitted. 'unsafe-inline' for style-src covers the UI's inline style
	// attributes only; scripts remain 'self' with no inline allowance.
	headerCSP                 = "default-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none';"
	headerXContentTypeOptions = "nosniff"
	headerReferrerPolicy      = "same-origin"
)

// WithStaticAssets configures the Server to serve the embedded web UI from staticFS.
func WithStaticAssets(staticFS fs.FS) Option {
	return func(s *Server) {
		s.staticFS = staticFS
	}
}

// staticAsset is a fully-precomputed embedded file: its bytes, its content-addressed
// ETag, and its content type, all derived once at construction. The embedded assets
// are immutable for the life of the process, so nothing here is recomputed per request.
type staticAsset struct {
	data        []byte
	etag        string
	contentType string
}

// staticHandler serves the embedded web UI with SPA fallback (serving index.html
// for unknown navigation paths) and security headers. All assets are precomputed
// into assets at construction; ServeHTTP never re-reads or re-hashes the FS.
type staticHandler struct {
	assets    map[string]staticAsset
	indexFile []byte
	indexETag string
	modTime   time.Time
}

func newStaticHandler(staticFS fs.FS) http.Handler {
	sh := &staticHandler{
		assets:  make(map[string]staticAsset),
		modTime: time.Now().UTC(),
	}

	if staticFS == nil {
		return sh
	}

	// Walk the embedded FS once, reading and hashing every file up front so that
	// each request is a map lookup rather than an Open + ReadAll + SHA-256.
	_ = fs.WalkDir(staticFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			//nolint:nilerr // skip an unreadable entry or directory and keep walking the trusted embed FS
			return nil
		}
		data, rerr := fs.ReadFile(staticFS, p)
		if rerr != nil {
			//nolint:nilerr // skip an unreadable embedded file rather than aborting asset precompute
			return nil
		}
		sum := sha256.Sum256(data)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		asset := staticAsset{
			data:        data,
			etag:        etag,
			contentType: mime.TypeByExtension(path.Ext(p)),
		}
		sh.assets[p] = asset
		if p == "index.html" {
			sh.indexFile = data
			sh.indexETag = etag
		}
		return nil
	})

	return sh
}

func (sh *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Security-Policy", headerCSP)
	w.Header().Set("X-Content-Type-Options", headerXContentTypeOptions)
	w.Header().Set("Referrer-Policy", headerReferrerPolicy)

	cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if cleanPath == "" || cleanPath == "." {
		cleanPath = "index.html"
	}

	// Serve a precomputed asset directly.
	if asset, ok := sh.assets[cleanPath]; ok {
		if asset.contentType != "" {
			w.Header().Set("Content-Type", asset.contentType)
		}
		w.Header().Set("ETag", asset.etag)
		// http.ServeContent evaluates If-None-Match against the ETag set above,
		// handling "*", entity-tag lists, and weak validators per RFC 9110, so no
		// separate conditional check is needed here.
		http.ServeContent(w, r, cleanPath, sh.modTime, bytes.NewReader(asset.data))
		return
	}

	// SPA fallback: serve index.html only for navigation routes (no file
	// extension, e.g. /dashboard, /system). A path with an extension is a
	// missing asset (e.g. a mistyped /styles.css) and must 404 rather than
	// silently return HTML with a 200.
	if len(sh.indexFile) > 0 && path.Ext(cleanPath) == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", sh.indexETag)
		http.ServeContent(w, r, "index.html", sh.modTime, bytes.NewReader(sh.indexFile))
		return
	}

	http.NotFound(w, r)
}
