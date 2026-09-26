package update

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// repoPath is the repository part of the fake's base URL, as on GitHub.
const repoPath = "/tphakala/birdnet-go-remote-mic"

// fakeGitHub serves the release pages the way GitHub does:
// {base}/releases/latest redirects (absolute URL) to the tag page, or to the
// release list with no release, and {base}/releases/download/<tag>/<file>
// redirects to a separate storage host, which serves the asset or a 404.
// (GitHub answers a missing asset with a 404 without redirecting, and its
// storage URLs are opaque; the Fetcher looks at neither.) files is keyed by
// the path below the base.
type fakeGitHub struct {
	srv     *httptest.Server
	storage *httptest.Server
	latest  atomic.Pointer[string] // the tag /releases/latest redirects to; nil means no release
	files   map[string][]byte      // path below the base -> body
	// assetRedirect, when set, replaces the storage host in asset redirects.
	assetRedirect string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{files: map[string][]byte{}}
	g.storage = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := g.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(g.storage.Close)
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := strings.CutPrefix(r.URL.Path, repoPath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch {
		case p == "/releases/latest":
			if tag := g.latest.Load(); tag != nil {
				http.Redirect(w, r, g.base()+"/releases/tag/"+*tag, http.StatusFound)
			} else {
				http.Redirect(w, r, g.base()+"/releases", http.StatusFound)
			}
		case strings.HasPrefix(p, "/releases/download/"):
			host := g.storage.URL
			if g.assetRedirect != "" {
				host = g.assetRedirect
			}
			http.Redirect(w, r, host+p, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// base is the repository page, the Fetcher's Base.
func (g *fakeGitHub) base() string { return g.srv.URL + repoPath }

// fetcher is a Fetcher for this fake.
func (g *fakeGitHub) fetcher(trusted map[string]ed25519.PublicKey) *Fetcher {
	return &Fetcher{Client: g.srv.Client(), Base: g.base(), Trusted: trusted}
}

// publish serves rel as the latest release.
func (g *fakeGitHub) publish(rel *release) {
	dir := "/releases/download/" + rel.version + "/"
	g.files[dir+releasemanifest.FileName] = rel.raw
	g.files[dir+releasemanifest.SignatureFileName] = rel.sig
	g.files[dir+"remote-mic.tar.gz"] = rel.tarball
	g.latest.Store(&rel.version)
}

func (g *fakeGitHub) tarURL(version string) string {
	return g.base() + "/releases/download/" + version + "/remote-mic.tar.gz"
}

func TestFetcherLatest(t *testing.T) {
	t.Parallel()
	priv, trusted := testKeys(t)
	g := newFakeGitHub(t)
	rel := newRelease(t, priv, vNew, testTarget, g.tarURL(vNew), []byte("new binary"))
	g.publish(rel)

	f := g.fetcher(trusted)
	got, err := f.Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got.Manifest.Version != vNew {
		t.Errorf("got version %q, want v0.3.0", got.Manifest.Version)
	}
	if !bytes.Equal(got.Raw, rel.raw) || !bytes.Equal(got.Signature, rel.sig) {
		t.Error("Release does not carry the exact verified bytes")
	}
}

func TestFetcherLatestRefuses(t *testing.T) {
	t.Parallel()
	priv, trusted := testKeys(t)
	otherPriv, _ := testKeys(t)

	tests := []struct {
		name  string
		setup func(g *fakeGitHub)
		check func(error) bool
	}{
		{
			name:  "no release",
			setup: func(*fakeGitHub) {},
			check: func(err error) bool { return errors.Is(err, ErrNoRelease) },
		},
		{
			name: wantUntrusted,
			setup: func(g *fakeGitHub) {
				g.publish(newRelease(t, otherPriv, vNew, testTarget, g.tarURL(vNew), []byte("x")))
			},
			check: func(err error) bool { return errors.Is(err, releasemanifest.ErrUntrustedKey) },
		},
		{
			name: "manifest moved to another tag",
			setup: func(g *fakeGitHub) {
				old := newRelease(t, priv, vOld, testTarget, g.tarURL(vOld), []byte("x"))
				dir := "/releases/download/v0.3.0/"
				g.files[dir+releasemanifest.FileName] = old.raw
				g.files[dir+releasemanifest.SignatureFileName] = old.sig
				tag := vNew
				g.latest.Store(&tag)
			},
			check: func(err error) bool {
				return errors.Is(err, releasemanifest.ErrInvalid) && strings.Contains(err.Error(), "carries the manifest of v0.2.0")
			},
		},
		{
			name: "missing signature",
			setup: func(g *fakeGitHub) {
				rel := newRelease(t, priv, vNew, testTarget, g.tarURL(vNew), []byte("x"))
				g.publish(rel)
				delete(g.files, "/releases/download/v0.3.0/"+releasemanifest.SignatureFileName)
			},
			check: func(err error) bool {
				se, ok := errors.AsType[*StatusError](err)
				return ok && se.Code == http.StatusNotFound
			},
		},
		{
			name: "oversize manifest",
			setup: func(g *fakeGitHub) {
				rel := newRelease(t, priv, vNew, testTarget, g.tarURL(vNew), []byte("x"))
				g.publish(rel)
				g.files["/releases/download/v0.3.0/"+releasemanifest.FileName] = make([]byte, releasemanifest.MaxManifestSize+1)
			},
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), "body over") },
		},
		{
			name: "tag not a version",
			setup: func(g *fakeGitHub) {
				tag := "nightly"
				g.latest.Store(&tag)
			},
			check: func(err error) bool { return errors.Is(err, releasemanifest.ErrInvalid) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newFakeGitHub(t)
			tt.setup(g)
			f := g.fetcher(trusted)
			_, err := f.Latest(t.Context())
			if !tt.check(err) {
				t.Errorf("got error %v", err)
			}
		})
	}
}

// TestFetcherRefusesHTTPDowngrade pins that a redirect from an asset to plain
// http is not followed.
func TestFetcherRefusesHTTPDowngrade(t *testing.T) {
	t.Parallel()
	priv, trusted := testKeys(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the plain http server was contacted")
	}))
	t.Cleanup(plain.Close)
	g := newFakeGitHub(t)
	g.publish(newRelease(t, priv, vNew, testTarget, g.tarURL(vNew), []byte("x")))
	g.assetRedirect = plain.URL // the asset host answers on plain http
	f := g.fetcher(trusted)
	if _, err := f.Latest(t.Context()); err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Errorf("got %v, want a refused redirect", err)
	}
}

func TestFetcherLatestStatusAndUserAgent(t *testing.T) {
	t.Parallel()
	var ua atomic.Pointer[string]
	status := http.StatusNotFound
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("User-Agent")
		ua.Store(&v)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	f := &Fetcher{Client: srv.Client(), Base: srv.URL, UserAgent: "remote-mic/v0.2.0"}
	if _, err := f.Latest(t.Context()); !errors.Is(err, ErrNoRelease) {
		t.Errorf("404: got %v, want ErrNoRelease", err)
	}
	if got := ua.Load(); got == nil || *got != "remote-mic/v0.2.0" {
		t.Errorf("User-Agent %v", got)
	}
	status = http.StatusBadGateway
	_, err := f.Latest(t.Context())
	if se, ok := errors.AsType[*StatusError](err); !ok || se.Code != http.StatusBadGateway {
		t.Errorf("502: got %v", err)
	}
}

// TestFetcherLatestUnexpectedRedirect pins that only a redirect to the
// release list reads as "no release": one elsewhere (a renamed or moved
// repository) is an error of its own.
func TestFetcherLatestUnexpectedRedirect(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/someone/renamed/releases/latest", http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)
	f := &Fetcher{Client: srv.Client(), Base: srv.URL}
	_, err := f.Latest(t.Context())
	if err == nil || errors.Is(err, ErrNoRelease) || !strings.Contains(err.Error(), "unexpected redirect") {
		t.Errorf("got %v, want an unexpected-redirect error", err)
	}
}
