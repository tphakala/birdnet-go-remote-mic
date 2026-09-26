package update

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// fakeGitHub serves the release pages Fetcher reads: /releases/latest
// redirects to the tag page, and /releases/download/<tag>/<file> serves
// assets. Handlers may be replaced per test.
type fakeGitHub struct {
	srv    *httptest.Server
	latest atomic.Pointer[string] // the tag /releases/latest redirects to; nil means no release
	files  map[string][]byte      // path -> body
	hits   atomic.Int64
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{files: map[string][]byte{}}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		if r.URL.Path == "/releases/latest" {
			tag := g.latest.Load()
			if tag == nil {
				http.Redirect(w, r, "/releases", http.StatusFound)
				return
			}
			http.Redirect(w, r, "/releases/tag/"+*tag, http.StatusFound)
			return
		}
		body, ok := g.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(g.srv.Close)
	return g
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
	return g.srv.URL + "/releases/download/" + version + "/remote-mic.tar.gz"
}

func TestFetcherLatest(t *testing.T) {
	t.Parallel()
	priv, trusted := testKeys(t)
	g := newFakeGitHub(t)
	rel := newRelease(t, priv, vNew, testTarget, g.tarURL(vNew), []byte("new binary"))
	g.publish(rel)

	f := &Fetcher{Client: g.srv.Client(), Base: g.srv.URL, Trusted: trusted}
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
				var se *StatusError
				return errors.As(err, &se) && se.Code == http.StatusNotFound
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
			f := &Fetcher{Client: g.srv.Client(), Base: g.srv.URL, Trusted: trusted}
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
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, releasemanifest.FileName) {
			http.Redirect(w, r, plain.URL+"/manifest.json", http.StatusFound)
			return
		}
		g.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(tlsSrv.Close)
	f := &Fetcher{Client: tlsSrv.Client(), Base: tlsSrv.URL, Trusted: trusted}
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
	var se *StatusError
	if _, err := f.Latest(t.Context()); !errors.As(err, &se) || se.Code != http.StatusBadGateway {
		t.Errorf("502: got %v", err)
	}
}
