package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// ErrNoTarget reports a release that ships no build for this platform.
var ErrNoTarget = errors.New("the release has no build for this platform")

// Stager downloads a verified release for the running platform into the
// staging directory and asks the root updater to install it. It runs as the
// unprivileged appliance: everything it writes, the updater checks again.
type Stager struct {
	// Dir is the staging directory (DirName inside the state directory).
	Dir string
	// Client downloads the tarball; use a context deadline.
	Client *http.Client
	// UserAgent is sent with the download.
	UserAgent string
	// Target is the running build's target key (RunningTarget).
	Target string
}

// Stage downloads rel's tarball for s.Target, checks its size and SHA-256,
// extracts the binary and checks it against the manifest, writes the manifest
// pair beside it, and writes the request file last. Leftovers of an earlier
// attempt, its result included, are removed first, and a failure removes what
// it staged.
func (s *Stager) Stage(ctx context.Context, rel *Release) (err error) {
	t, ok := rel.Manifest.Targets[s.Target]
	if !ok {
		return fmt.Errorf("%w (%s)", ErrNoTarget, s.Target)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	s.clean()
	// A result left from an earlier attempt must not pass for this one's.
	_ = os.Remove(filepath.Join(s.Dir, StatusFile))
	defer func() {
		if err != nil {
			s.clean()
		}
	}()

	part := filepath.Join(s.Dir, downloadFile)
	if err := s.download(ctx, &t, part); err != nil {
		return err
	}
	if err := extractBinary(part, t.Binary, filepath.Join(s.Dir, BinaryFile)); err != nil {
		return err
	}
	_ = os.Remove(part)
	if err := atomicfile.Write(filepath.Join(s.Dir, ManifestFile), rel.Raw, 0o644); err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(s.Dir, SignatureFile), rel.Signature, 0o644); err != nil {
		return err
	}
	req, err := json.Marshal(Request{Version: rel.Manifest.Version})
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.Dir, RequestFile), req, 0o644)
}

// clean removes every staged file and request, leaving status and health
// alone.
func (s *Stager) clean() {
	for _, name := range []string{RequestFile, TakenFile, downloadFile, BinaryFile, BinaryFile + ".part", ManifestFile, SignatureFile} {
		_ = os.Remove(filepath.Join(s.Dir, name))
	}
}

// download streams the tarball to path, refusing a body of any length but
// t.Size and a SHA-256 other than t.SHA256.
func (s *Stager) download(ctx context.Context, t *releasemanifest.Target, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, http.NoBody)
	if err != nil {
		return err
	}
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	c := http.Client{}
	if s.Client != nil {
		c = *s.Client
	}
	c.CheckRedirect = httpsOnly
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return &StatusError{URL: t.URL, Code: resp.StatusCode}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, t.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", t.URL, err)
	}
	if n != t.Size {
		return fmt.Errorf("download %s: got %d bytes, want %d", t.URL, n, t.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != t.SHA256 {
		return fmt.Errorf("download %s: sha256 %s, want %s", t.URL, got, t.SHA256)
	}
	return nil
}

// extractBinary copies the tar entry named b.Path out of the gzip tarball at
// src into dst (mode 0755), refusing an entry that is not a regular file or
// whose size or SHA-256 differs from the manifest.
func extractBinary(src string, b releasemanifest.Binary, dst string) error {
	f, err := os.Open(src) //nolint:gosec // src is the download path this stager chose
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("tarball: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("tarball has no %s", b.Path)
		}
		if err != nil {
			return fmt.Errorf("tarball: %w", err)
		}
		if hdr.Name != b.Path {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("tarball entry %s is not a regular file", b.Path)
		}
		if hdr.Size != b.Size {
			return fmt.Errorf("tarball entry %s is %d bytes, want %d", b.Path, hdr.Size, b.Size)
		}
		return writeChecked(dst, tr, b)
	}
}

// writeChecked streams r to dst through a temporary file, checking size and
// SHA-256 before the rename, so dst never holds an unchecked binary.
func writeChecked(dst string, r io.Reader, b releasemanifest.Binary) error {
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // an executable, installed by the root updater
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, b.Size+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != b.Size {
		err = fmt.Errorf("binary is %d bytes, want %d", n, b.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); err == nil && got != b.SHA256 {
		err = fmt.Errorf("binary sha256 %s, want %s", got, b.SHA256)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
