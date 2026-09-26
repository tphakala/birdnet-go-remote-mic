package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

func stagedRelease(t *testing.T) (*fakeGitHub, *release, *Release) {
	t.Helper()
	priv, trusted := testKeys(t)
	g := newFakeGitHub(t)
	rel := newRelease(t, priv, vNew, testTarget, g.tarURL(vNew), []byte("the new binary"))
	g.publish(rel)
	f := &Fetcher{Client: g.srv.Client(), Base: g.srv.URL, Trusted: trusted}
	got, err := f.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return g, rel, got
}

func TestStage(t *testing.T) {
	t.Parallel()
	g, rel, verified := stagedRelease(t)
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, StatusFile), Result{Outcome: OutcomeFailed}) // an earlier attempt's
	s := &Stager{Dir: dir, Client: g.srv.Client(), Target: testTarget}
	if err := s.Stage(t.Context(), verified); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if exists(filepath.Join(dir, StatusFile)) {
		t.Error("an earlier attempt's result survived staging")
	}
	bin, err := os.ReadFile(filepath.Join(dir, BinaryFile))
	if err != nil || !bytes.Equal(bin, rel.bin) {
		t.Fatalf("staged binary %q, %v; want %q", bin, err, rel.bin)
	}
	if fi, _ := os.Stat(filepath.Join(dir, BinaryFile)); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("staged binary mode %v is not executable", fi.Mode())
	}
	for name, want := range map[string][]byte{ManifestFile: rel.raw, SignatureFile: rel.sig} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(got, want) {
			t.Errorf("%s does not hold the verified bytes", name)
		}
	}
	var req Request
	b, err := os.ReadFile(filepath.Join(dir, RequestFile))
	if err != nil || json.Unmarshal(b, &req) != nil || req.Version != vNew {
		t.Errorf("request %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, downloadFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("download file left behind: %v", err)
	}
}

func TestStageRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, g *fakeGitHub, rel *release, verified *Release)
		target string
		want   string
	}{
		{name: wantNoTarget, target: "linux/amd64", want: wantNoTarget},
		{
			name: "tarball altered",
			mutate: func(_ *testing.T, g *fakeGitHub, rel *release, _ *Release) {
				b := append([]byte(nil), rel.tarball...)
				b[len(b)-1] ^= 0xff
				g.files["/releases/download/v0.3.0/remote-mic.tar.gz"] = b
			},
			want: "sha256",
		},
		{
			name: "tarball truncated",
			mutate: func(_ *testing.T, g *fakeGitHub, rel *release, _ *Release) {
				g.files["/releases/download/v0.3.0/remote-mic.tar.gz"] = rel.tarball[:len(rel.tarball)-1]
			},
			want: "want",
		},
		{
			name: "binary does not match the manifest",
			mutate: func(t *testing.T, g *fakeGitHub, _ *release, verified *Release) {
				t.Helper()
				tb := tarGz(t, map[string][]byte{releasemanifest.BinaryName: []byte("the evil binary")})
				g.files["/releases/download/v0.3.0/remote-mic.tar.gz"] = tb
				tgt := verified.Manifest.Targets[testTarget]
				tgt.Size, tgt.SHA256 = int64(len(tb)), sha(tb)
				tgt.Binary.Size = int64(len("the evil binary"))
				verified.Manifest.Targets[testTarget] = tgt
			},
			want: "binary sha256",
		},
		{
			name: "binary entry is a symlink",
			mutate: func(t *testing.T, g *fakeGitHub, _ *release, verified *Release) {
				t.Helper()
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				tw := tar.NewWriter(zw)
				if err := tw.WriteHeader(&tar.Header{Name: releasemanifest.BinaryName, Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}); err != nil {
					t.Fatal(err)
				}
				_ = tw.Close()
				_ = zw.Close()
				tb := buf.Bytes()
				g.files["/releases/download/v0.3.0/remote-mic.tar.gz"] = tb
				tgt := verified.Manifest.Targets[testTarget]
				tgt.Size, tgt.SHA256 = int64(len(tb)), sha(tb)
				verified.Manifest.Targets[testTarget] = tgt
			},
			want: "not a regular file",
		},
		{
			name: "binary entry size differs from the manifest",
			mutate: func(_ *testing.T, _ *fakeGitHub, _ *release, verified *Release) {
				tgt := verified.Manifest.Targets[testTarget]
				tgt.Binary.Size++
				verified.Manifest.Targets[testTarget] = tgt
			},
			want: "bytes, want",
		},
		{
			name: "tarball missing",
			mutate: func(_ *testing.T, g *fakeGitHub, _ *release, _ *Release) {
				delete(g.files, "/releases/download/v0.3.0/remote-mic.tar.gz")
			},
			want: "HTTP 404",
		},
		{
			name: "binary missing from the tarball",
			mutate: func(t *testing.T, g *fakeGitHub, _ *release, verified *Release) {
				t.Helper()
				tb := tarGz(t, map[string][]byte{"README": []byte("x")})
				g.files["/releases/download/v0.3.0/remote-mic.tar.gz"] = tb
				tgt := verified.Manifest.Targets[testTarget]
				tgt.Size, tgt.SHA256 = int64(len(tb)), sha(tb)
				verified.Manifest.Targets[testTarget] = tgt
			},
			want: "tarball has no remote-mic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g, rel, verified := stagedRelease(t)
			if tt.mutate != nil {
				tt.mutate(t, g, rel, verified)
			}
			target := tt.target
			if target == "" {
				target = testTarget
			}
			dir := filepath.Join(t.TempDir(), DirName)
			s := &Stager{Dir: dir, Client: g.srv.Client(), Target: target}
			err := s.Stage(t.Context(), verified)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				t.Errorf("failed stage left %s behind", e.Name())
			}
		})
	}
}
