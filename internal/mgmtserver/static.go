package mgmtserver

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
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

// gzipMinSize is the smallest asset worth compressing; below it the saving
// does not pay for the encoding header and the CPU.
const gzipMinSize = 1024

// staticAsset is a precomputed embedded file: its bytes, its content-addressed
// ETag, and its content type, derived once at construction. The embedded assets
// are immutable for the life of the process, so none of this is recomputed per
// request. gz is the gzip encoding of a compressible asset (nil for one that is
// not), made on the first request that accepts it rather than at construction.
type staticAsset struct {
	data        []byte
	etag        string
	contentType string
	gz          *gzipped
}

// gzipped is an asset's gzip encoding, made on the first request that accepts
// it and kept: an appliance nobody opens the UI on never spends the CPU or the
// memory. data stays nil when compressing did not make the asset smaller.
type gzipped struct {
	once sync.Once
	data []byte
	etag string
}

// get returns the encoding of a and its ETag, compressing with c on the first
// call, or nil when the encoding is no smaller.
func (g *gzipped) get(a *staticAsset, c *compressor) (data []byte, etag string) {
	g.once.Do(func() {
		if gz := c.compress(a.data); gz != nil {
			g.data = gz
			// A different representation needs a different validator.
			g.etag = strings.TrimSuffix(a.etag, `"`) + `-gz"`
		}
	})
	return g.data, g.etag
}

// compressor gzips assets one at a time. A first page load asks for dozens of
// assets at once, and a flate writer allocates about a megabyte of tables, so
// compressing them side by side would spike the heap on a Pi; one at a time,
// each writer is garbage before the next is made, and nothing is kept once the
// UI goes quiet. The default level is used: the best level costs twice the CPU
// for a few percent.
type compressor struct {
	mu sync.Mutex
}

// compress returns data gzipped, or nil when that is no smaller.
func (c *compressor) compress(data []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var buf bytes.Buffer
	// Only an invalid level fails, and DefaultCompression is valid; writes
	// into a bytes.Buffer do not fail either.
	zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	_, _ = zw.Write(data)
	if zw.Close() != nil || buf.Len() >= len(data) {
		return nil
	}
	// A copy at its own length: the buffer grew in steps and is spare beyond it.
	return bytes.Clone(buf.Bytes())
}

// compressible reports whether an asset of this type and size is worth
// gzipping: text, scripts, JSON and +xml types such as SVG. Fonts (woff2)
// and raster images are compressed already. An asset with no known type is left alone, since ServeContent
// would sniff its type from the compressed bytes.
func compressible(contentType string, size int) bool {
	if size < gzipMinSize || contentType == "" {
		return false
	}
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.HasPrefix(mt, "text/") || strings.HasSuffix(mt, "javascript") ||
		strings.HasSuffix(mt, "json") || strings.HasSuffix(mt, "+xml")
}

// staticHandler serves the embedded web UI with SPA fallback (serving index.html
// for unknown navigation paths) and security headers. All assets are read and
// hashed into assets at construction; ServeHTTP never re-reads or re-hashes the
// FS, and only a compressible asset's first gzip request does any encoding.
type staticHandler struct {
	assets  map[string]*staticAsset
	index   *staticAsset
	modTime time.Time
	gz      compressor
}

func newStaticHandler(staticFS fs.FS) http.Handler {
	sh := &staticHandler{
		assets:  make(map[string]*staticAsset),
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
		asset := &staticAsset{
			data:        data,
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
			contentType: mime.TypeByExtension(path.Ext(p)),
		}
		if p == "index.html" {
			// The SPA fallback serves it for paths without an extension too.
			sh.index = asset
		}
		if compressible(asset.contentType, len(data)) {
			asset.gz = &gzipped{}
		}
		sh.assets[p] = asset
		return nil
	})

	return sh
}

// serve writes asset a, gzipped when it is compressible and the request
// accepts that (acceptsGzip in gzip.go), leaving the length of a gzipped body
// to net/http. Vary tells a cache the body depends on Accept-Encoding.
func (sh *staticHandler) serve(w http.ResponseWriter, r *http.Request, name string, a *staticAsset) {
	if a.contentType != "" {
		w.Header().Set("Content-Type", a.contentType)
	}
	body, etag := a.data, a.etag
	if a.gz != nil {
		w.Header().Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			if gz, gzETag := a.gz.get(a, &sh.gz); gz != nil {
				body, etag = gz, gzETag
				w.Header().Set("Content-Encoding", "gzip")
				// No Content-Length: ServeContent leaves it out when a
				// Content-Encoding is set, and setting it here would also go out
				// on a 412, which carries no body. net/http sizes a small body
				// itself and chunks a larger one.
			}
		}
	}
	w.Header().Set("ETag", etag)
	// http.ServeContent evaluates If-None-Match against the ETag set above,
	// handling "*", entity-tag lists, and weak validators per RFC 9110, so no
	// separate conditional check is needed here.
	http.ServeContent(w, r, name, sh.modTime, bytes.NewReader(body))
}

func (sh *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Security-Policy", headerCSP)
	w.Header().Set("X-Content-Type-Options", headerXContentTypeOptions)
	w.Header().Set("Referrer-Policy", headerReferrerPolicy)
	// Revalidate on every load. Without an explicit policy browsers cache by
	// heuristic (a fraction of the time since Last-Modified), so a freshly
	// deployed build could keep showing the previous stylesheet or script.
	// The ETag makes each revalidation a cheap 304 when nothing changed.
	w.Header().Set("Cache-Control", "no-cache")

	cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if cleanPath == "" || cleanPath == "." {
		cleanPath = "index.html"
	}

	// Serve a precomputed asset directly.
	if asset, ok := sh.assets[cleanPath]; ok {
		sh.serve(w, r, cleanPath, asset)
		return
	}

	// SPA fallback: serve index.html only for navigation routes (no file
	// extension, e.g. /dashboard, /system). A path with an extension is a
	// missing asset (e.g. a mistyped /styles.css) and must 404 rather than
	// silently return HTML with a 200.
	if sh.index != nil && path.Ext(cleanPath) == "" {
		sh.serve(w, r, "index.html", sh.index)
		return
	}

	http.NotFound(w, r)
}
