package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// DefaultBase is the GitHub repository page releases are published under.
const DefaultBase = "https://github.com/" + releasemanifest.Repository

// ErrNoRelease reports a repository with no published stable release.
var ErrNoRelease = errors.New("no published release")

// StatusError reports an unexpected HTTP status from a release download.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Code)
}

// Release is a verified manifest together with the exact bytes it was
// verified from, which the stager hands on so the root updater can verify the
// same pair again with its own keys.
type Release struct {
	Manifest  *releasemanifest.Manifest
	Raw       []byte
	Signature []byte
}

// Fetcher downloads and verifies the newest release manifest. It makes plain
// downloads from the release pages, never a REST API call, so no API rate
// limit applies.
type Fetcher struct {
	// Client makes the requests. Its redirect policy is replaced per request;
	// set a timeout or use a context deadline.
	Client *http.Client
	// Base is the repository page, DefaultBase in production.
	Base string
	// Trusted are the release signing keys, releasemanifest.TrustedKeys in
	// production.
	Trusted map[string]ed25519.PublicKey
	// UserAgent is sent with every request.
	UserAgent string
}

// Latest resolves the newest stable release tag once, then downloads that
// tag's manifest and signature and verifies them. Resolving the tag first
// keeps the pair from one release: two "latest" downloads could straddle a
// publish and fetch a manifest and signature that do not belong together.
func (f *Fetcher) Latest(ctx context.Context) (*Release, error) {
	tag, err := f.latestTag(ctx)
	if err != nil {
		return nil, err
	}
	dir := f.Base + "/releases/download/" + url.PathEscape(tag) + "/"
	raw, err := f.download(ctx, dir+releasemanifest.FileName, releasemanifest.MaxManifestSize)
	if err != nil {
		return nil, err
	}
	sig, err := f.download(ctx, dir+releasemanifest.SignatureFileName, releasemanifest.MaxSignatureSize)
	if err != nil {
		return nil, err
	}
	m, err := releasemanifest.Verify(raw, sig, f.Trusted)
	if err != nil {
		return nil, err
	}
	// The signature proves the manifest came from some release, not from this
	// tag; a manifest naming another version was moved there.
	if m.Version != tag {
		return nil, fmt.Errorf("%w: release %s carries the manifest of %s", releasemanifest.ErrInvalid, tag, m.Version)
	}
	return &Release{Manifest: m, Raw: raw, Signature: sig}, nil
}

// latestTag asks the repository's "latest release" page where it redirects:
// GitHub answers with a redirect to /releases/tag/<tag>, or to the release
// list when there is no stable release.
func (f *Fetcher) latestTag(ctx context.Context) (string, error) {
	u := f.Base + "/releases/latest"
	req, err := f.newRequest(ctx, u)
	if err != nil {
		return "", err
	}
	c := f.client(func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse })
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		if resp.StatusCode == http.StatusNotFound {
			return "", ErrNoRelease
		}
		return "", &StatusError{URL: u, Code: resp.StatusCode}
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("GET %s: redirect without a location: %w", u, err)
	}
	_, tag, found := strings.Cut(loc.EscapedPath(), "/releases/tag/")
	if !found {
		return "", ErrNoRelease
	}
	tag, err = url.PathUnescape(tag)
	if err != nil || !releasemanifest.ValidVersion(tag) {
		return "", fmt.Errorf("%w: latest release tag %q is not a version", releasemanifest.ErrInvalid, tag)
	}
	return tag, nil
}

// download fetches u, following redirects only to https, and refuses a body
// longer than limit. It reads one byte past the limit to tell: a plain
// LimitReader would truncate silently.
func (f *Fetcher) download(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := f.newRequest(ctx, u)
	if err != nil {
		return nil, err
	}
	resp, err := f.client(httpsOnly).Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{URL: u, Code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: body over %d bytes", u, limit)
	}
	return b, nil
}

func (f *Fetcher) newRequest(ctx context.Context, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	return req, nil
}

// client returns a shallow copy of f.Client with the given redirect policy,
// so the caller's client is never mutated.
func (f *Fetcher) client(redirect func(*http.Request, []*http.Request) error) *http.Client {
	c := http.Client{}
	if f.Client != nil {
		c = *f.Client
	}
	c.CheckRedirect = redirect
	return &c
}

// maxRedirects matches net/http's default redirect cap.
const maxRedirects = 10

// httpsOnly follows at most maxRedirects redirects, and only to https URLs:
// a release asset redirects to a storage host, and a downgrade to plain http
// on the way would let the network rewrite the body.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
	}
	return nil
}

// drain discards what is left of a response body and closes it, so the
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
