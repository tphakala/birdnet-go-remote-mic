package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

const testTag = "v1.2.3"

// Command-line words the run tests repeat.
const (
	makeLatestCmd = "make-latest"
	tagFlag       = "-tag"
)

// tarEntry is one file in a fake release archive.
type tarEntry struct {
	name, content string
	typeflag      byte
}

// writeTarGz writes a gzipped tar archive holding entries.
func writeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: e.typeflag}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, buf.String())
}

// binaryContent is the fake executable stored in the archive for suffix.
func binaryContent(suffix string) string { return "binary for " + suffix }

// hashArchive returns the hex SHA-256 of the file at path.
func hashArchive(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeDist writes a GoReleaser-shaped dist directory, in the layout of a real
// `goreleaser release --snapshot` run: metadata.json, artifacts.json (with
// non-archive entries such as the ones a real run lists for checksums.txt and
// each .deb), checksums.txt, and an archive for each release target. mutate may
// edit the artifacts or checksums before they are written.
func fakeDist(t *testing.T, mutate func(arts []artifact, sums map[string]string)) string {
	t.Helper()
	dist := t.TempDir()
	meta := `{"project_name":"birdnet-go-remote-mic","tag":"` + testTag + `","date":"2026-09-26T13:57:30.076499321+03:00"}`
	writeFile(t, filepath.Join(dist, "metadata.json"), meta)

	const checksumsFile, goos, amd64 = "checksums.txt", "linux", "amd64"
	targets := []struct{ goarch, goarm string }{{amd64, ""}, {"arm64", ""}, {"arm", "6"}}
	arts := make([]artifact, 0, 2+len(targets))
	arts = append(arts,
		artifact{Name: checksumsFile, Path: filepath.Join(dist, checksumsFile), Type: "Checksum"},
		artifact{Name: "x_linux_amd64.deb", Path: filepath.Join(dist, "x_linux_amd64.deb"), GOOS: goos, GOARCH: amd64, Type: "Linux Package"},
	)
	sums := map[string]string{}
	for _, tgt := range targets {
		// GoReleaser names the 32-bit arm archive armv6, the same suffix TargetKey uses.
		suffix := strings.TrimPrefix(releasemanifest.TargetKey(goos, tgt.goarch, tgt.goarm), goos+"/")
		name := "birdnet-go-remote-mic_1.2.3_linux_" + suffix + ".tar.gz"
		path := filepath.Join(dist, name)
		writeTarGz(t, path, []tarEntry{
			{"README.md", "readme", tar.TypeReg},
			{releasemanifest.BinaryName, binaryContent(suffix), tar.TypeReg},
		})
		hexSum := hashArchive(t, path)
		sums[name] = hexSum
		a := artifact{Name: name, Path: path, GOOS: goos, GOARCH: tgt.goarch, GOARM: tgt.goarm, Type: "Archive"}
		a.Extra.Checksum = "sha256:" + hexSum
		arts = append(arts, a)
	}
	if mutate != nil {
		mutate(arts, sums)
	}
	b, err := json.Marshal(arts)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dist, "artifacts.json"), string(b))
	var lines strings.Builder
	for name, sum := range sums {
		fmt.Fprintf(&lines, "%s  %s\n", sum, name)
	}
	writeFile(t, filepath.Join(dist, checksumsFile), lines.String())
	return dist
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testKey(t *testing.T) (priv ed25519.PrivateKey, trusted map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]ed25519.PublicKey{releasemanifest.KeyID(pub): pub}
}

// archiveFor returns the archive artifact whose name ends in _<suffix>.tar.gz.
func archiveFor(t *testing.T, arts []artifact, suffix string) *artifact {
	t.Helper()
	for i := range arts {
		if strings.HasSuffix(arts[i].Name, "_"+suffix+".tar.gz") {
			return &arts[i]
		}
	}
	t.Fatalf("no %s archive", suffix)
	return nil
}

// TestGenerate pins the happy path: each required target is listed with the
// right URL, size and digest, a non-archive artifact is not, and the written
// files verify as an appliance would check them.
func TestGenerate(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	dist := fakeDist(t, nil)
	m, err := generate(dist, testTag, priv, trusted)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(m.Targets) != len(requiredTargets) {
		t.Errorf("got %d targets, want %d (the .deb must not be listed)", len(m.Targets), len(requiredTargets))
	}
	armv6 := m.Targets["linux/armv6"]
	wantURL := "https://github.com/" + releasemanifest.Repository + "/releases/download/" + testTag + "/birdnet-go-remote-mic_1.2.3_linux_armv6.tar.gz"
	if armv6.URL != wantURL {
		t.Errorf("armv6 url = %q, want %q", armv6.URL, wantURL)
	}
	archivePath := filepath.Join(dist, "birdnet-go-remote-mic_1.2.3_linux_armv6.tar.gz")
	info, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if armv6.Size != info.Size() {
		t.Errorf("armv6 size = %d, want %d", armv6.Size, info.Size())
	}
	if want := hashArchive(t, archivePath); armv6.SHA256 != want {
		t.Errorf("armv6 sha256 = %s, want %s", armv6.SHA256, want)
	}
	binSum := sha256.Sum256([]byte(binaryContent("armv6")))
	wantBin := releasemanifest.Binary{Path: releasemanifest.BinaryName, Size: int64(len(binaryContent("armv6"))), SHA256: hex.EncodeToString(binSum[:])}
	if armv6.Binary != wantBin {
		t.Errorf("armv6 binary = %+v, want %+v", armv6.Binary, wantBin)
	}
	if got, want := m.Date.Format("2006-01-02T15:04:05.999999999Z07:00"), "2026-09-26T10:57:30Z"; got != want {
		t.Errorf("date = %s, want %s (UTC, whole seconds)", got, want)
	}
	got, err := verifyFiles(dist, trusted)
	if err != nil {
		t.Fatalf("verifyFiles: %v", err)
	}
	if got.Version != testTag {
		t.Errorf("verified version = %q, want %q", got.Version, testTag)
	}
}

// TestGenerateRefuses covers each condition that must stop a manifest from
// being written.
func TestGenerateRefuses(t *testing.T) {
	t.Parallel()
	const amd64Archive = "birdnet-go-remote-mic_1.2.3_linux_amd64.tar.gz"
	for _, tc := range []struct {
		name   string
		tag    string
		mutate func([]artifact, map[string]string)
		want   error
	}{
		{"tag mismatch", "v9.9.9", nil, ErrTagMismatch},
		{"missing target", testTag, func(arts []artifact, _ map[string]string) {
			archiveFor(t, arts, "arm64").Type = "Binary"
		}, ErrMissingTarget},
		{"duplicate target", testTag, func(arts []artifact, _ map[string]string) {
			archiveFor(t, arts, "arm64").GOARCH = "amd64"
		}, ErrDuplicateTarget},
		{"checksums.txt disagrees", testTag, func(_ []artifact, sums map[string]string) {
			sums[amd64Archive] = strings.Repeat("0", 64)
		}, ErrChecksum},
		{"not in checksums.txt", testTag, func(_ []artifact, sums map[string]string) {
			delete(sums, amd64Archive)
		}, ErrChecksum},
		{"goreleaser record disagrees", testTag, func(arts []artifact, _ map[string]string) {
			archiveFor(t, arts, "armv6").Extra.Checksum = "sha256:" + strings.Repeat("0", 64)
		}, ErrChecksum},
		{"archive without the binary", testTag, rewriteArchive(t, []tarEntry{{"README.md", "readme", tar.TypeReg}}), ErrNoBinary},
		{"archive with two binaries", testTag, rewriteArchive(t, []tarEntry{
			{releasemanifest.BinaryName, "one", tar.TypeReg},
			{"sub/" + releasemanifest.BinaryName, "two", tar.TypeReg},
		}), ErrNoBinary},
		{"unclean entry name", testTag, rewriteArchive(t, []tarEntry{{"./" + releasemanifest.BinaryName, "x", tar.TypeReg}}), ErrNoBinary},
		{"escaping entry name", testTag, rewriteArchive(t, []tarEntry{{"../" + releasemanifest.BinaryName, "x", tar.TypeReg}}), ErrNoBinary},
		{"binary is a hardlink", testTag, rewriteArchive(t, []tarEntry{{releasemanifest.BinaryName, "", tar.TypeLink}}), ErrNoBinary},
		{"binary is a symlink", testTag, rewriteArchive(t, []tarEntry{{releasemanifest.BinaryName, "", tar.TypeSymlink}}), ErrNoBinary},
		{"archive is not gzip", testTag, rewriteArchiveRaw(t, []byte("plain bytes")), gzip.ErrHeader},
		{"gzip checksum is corrupt", testTag, rewriteArchiveRaw(t, corruptGzipCRC(t)), gzip.ErrChecksum},
		{"archive is truncated", testTag, rewriteArchiveRaw(t, truncatedTarGz(t)), io.ErrUnexpectedEOF},
	} {
		priv, trusted := testKey(t)
		dist := fakeDist(t, tc.mutate)
		if _, err := generate(dist, tc.tag, priv, trusted); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if _, err := os.Stat(filepath.Join(dist, releasemanifest.FileName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: manifest written despite the error (stat err = %v)", tc.name, err)
		}
	}
}

// rewriteArchive returns a fakeDist mutation that replaces the arm64 archive
// with one holding entries, keeping checksums.txt and GoReleaser's record in
// step so only the archive content is wrong.
func rewriteArchive(t *testing.T, entries []tarEntry) func([]artifact, map[string]string) {
	t.Helper()
	return func(arts []artifact, sums map[string]string) {
		a := archiveFor(t, arts, "arm64")
		writeTarGz(t, a.Path, entries)
		sum := hashArchive(t, a.Path)
		sums[a.Name] = sum
		a.Extra.Checksum = "sha256:" + sum
	}
}

// rewriteArchiveRaw is rewriteArchive for raw file content.
func rewriteArchiveRaw(t *testing.T, content []byte) func([]artifact, map[string]string) {
	t.Helper()
	return func(arts []artifact, sums map[string]string) {
		a := archiveFor(t, arts, "arm64")
		writeFile(t, a.Path, string(content))
		sum := hashArchive(t, a.Path)
		sums[a.Name] = sum
		a.Extra.Checksum = "sha256:" + sum
	}
}

// corruptGzipCRC returns a well-formed tar.gz holding the binary whose gzip
// CRC-32 trailer is wrong, which only a reader that consumes the whole gzip
// stream notices.
func corruptGzipCRC(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	writeTarGz(t, path, []tarEntry{{releasemanifest.BinaryName, "binary", tar.TypeReg}})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-8] ^= 0xff // first byte of the CRC-32 trailer
	return b
}

// truncatedTarGz returns a valid gzip stream holding a tar whose binary entry
// is cut short of its declared size.
func truncatedTarGz(t *testing.T) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := strings.Repeat("x", 4096)
	if err := tw.WriteHeader(&tar.Header{Name: releasemanifest.BinaryName, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	cut := tarBuf.Bytes()[:512+1024] // header block plus part of the data
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(cut); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestGenerateWrappedBinary pins that a binary inside one wrapping directory
// is found and recorded under its entry name, and a deeper one is not.
func TestGenerateWrappedBinary(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	wrapped := "birdnet-go-remote-mic_1.2.3_linux_arm64/" + releasemanifest.BinaryName
	dist := fakeDist(t, rewriteArchive(t, []tarEntry{
		{"a/b/" + releasemanifest.BinaryName, "too deep", tar.TypeReg},
		{wrapped, "wrapped", tar.TypeReg},
	}))
	m, err := generate(dist, testTag, priv, trusted)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := m.Targets["linux/arm64"].Binary; got.Path != wrapped || got.Size != int64(len("wrapped")) {
		t.Errorf("binary = %+v, want path %q size %d", got, wrapped, len("wrapped"))
	}
}

// TestGenerateRefusesUntrustedKey pins that a signing key missing from the
// compiled-in list fails before anything is written, since no appliance could
// verify the result.
func TestGenerateRefusesUntrustedKey(t *testing.T) {
	t.Parallel()
	priv, _ := testKey(t)
	_, otherTrusted := testKey(t)
	dist := fakeDist(t, nil)
	if _, err := generate(dist, testTag, priv, otherTrusted); !errors.Is(err, releasemanifest.ErrUntrustedKey) {
		t.Errorf("err = %v, want ErrUntrustedKey", err)
	}
	if _, err := os.Stat(filepath.Join(dist, releasemanifest.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("manifest written despite an untrusted key (stat err = %v)", err)
	}
}

// TestCheckKey pins the pre-publish key check for a trusted and an untrusted key.
func TestCheckKey(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	if err := checkKey(priv, trusted, io.Discard); err != nil {
		t.Errorf("trusted key: %v", err)
	}
	other, _ := testKey(t)
	if err := checkKey(other, trusted, io.Discard); !errors.Is(err, releasemanifest.ErrUntrustedKey) {
		t.Errorf("untrusted key: err = %v, want ErrUntrustedKey", err)
	}
}

// TestKeygen pins that keygen writes a private key readable by
// ParsePrivateKey, with owner-only permissions, and refuses to overwrite a file.
func TestKeygen(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "key")
	if err := keygen(out, io.Discard); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releasemanifest.ParsePrivateKey(string(b)); err != nil {
		t.Errorf("ParsePrivateKey on keygen output: %v", err)
	}
	if err := keygen(out, io.Discard); !errors.Is(err, os.ErrExist) {
		t.Errorf("second keygen to the same file: err = %v, want os.ErrExist", err)
	}
}

// TestRunCommands drives the command line: dispatch, flag handling, and the
// key checks that read RELEASE_MANIFEST_KEY. It sets the environment, so it
// does not run in parallel.
func TestRunCommands(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := run([]string{"keygen", "-out", keyFile}, io.Discard); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	seed, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	const checkKeyCmd = "check-key"
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want error // nil means any error
	}{
		{"no command", "", nil, nil},
		{"unknown command", "", []string{"sign"}, nil},
		{"keygen without -out", "", []string{"keygen"}, nil},
		{"bad flag", "", []string{"verify", "-nope"}, nil},
		{"check-key without key", "", []string{checkKeyCmd}, ErrNoKey},
		{"generate without key", "", []string{"generate"}, ErrNoKey},
		{"check-key with malformed key", "!!!", []string{checkKeyCmd}, nil},
		{"check-key with a bad tag", string(seed), []string{checkKeyCmd, tagFlag, "1.2.3"}, ErrBadTag},
		// A fresh key is not in keys.go, so it must be refused.
		{"check-key with untrusted key", string(seed), []string{checkKeyCmd}, releasemanifest.ErrUntrustedKey},
		{"generate without dist", string(seed), []string{"generate", "-dist", empty}, os.ErrNotExist},
		{"verify without dist", "", []string{"verify", "-dist", empty}, os.ErrNotExist},
		{"make-latest with a bad tag", "", []string{makeLatestCmd, tagFlag, "1.2.3"}, ErrBadTag},
		{"make-latest with a bad flag", "", []string{makeLatestCmd, "-nope"}, nil},
	} {
		t.Setenv(keyEnv, tc.env)
		err := run(tc.args, io.Discard)
		if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestReadChecksumsRejectsMalformedLine pins that a line not in sha256sum
// format fails the run instead of being skipped.
func TestReadChecksumsRejectsMalformedLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "checksums.txt")
	writeFile(t, path, strings.Repeat("a", 64)+" one-space-only.tar.gz\n")
	if _, err := readChecksums(path); err == nil {
		t.Error("got nil error for a malformed line")
	}
}

// TestVerifyFilesRejectsBrokenPair pins that verify fails on a missing
// signature file and on unparsable GoReleaser metadata.
func TestVerifyFilesRejectsBrokenPair(t *testing.T) {
	t.Parallel()
	priv, trusted := testKey(t)
	dist := fakeDist(t, nil)
	if _, err := generate(dist, testTag, priv, trusted); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dist, releasemanifest.SignatureFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyFiles(dist, trusted); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing signature: err = %v, want os.ErrNotExist", err)
	}
	writeFile(t, filepath.Join(dist, "metadata.json"), "{")
	if _, err := generate(dist, testTag, priv, trusted); err == nil {
		t.Error("unparsable metadata.json: got nil error")
	}
}

// TestMakeLatest pins which releases become "latest": a newer or equal
// version does, an older line's release does not, and a bad tag fails.
func TestMakeLatest(t *testing.T) {
	t.Parallel()
	const v130 = "v1.3.0"
	for _, tc := range []struct {
		tag, latest string
		want        bool
	}{
		{v130, "", true},
		{v130, "v1.2.9", true},
		{v130, v130, true},
		{"v1.2.5", v130, false},
		{"v1.10.0", "v1.9.0", true},
	} {
		got, err := makeLatest(tc.tag, tc.latest)
		if err != nil {
			t.Fatalf("makeLatest(%s, %s): %v", tc.tag, tc.latest, err)
		}
		if got != tc.want {
			t.Errorf("makeLatest(%s, %s) = %v, want %v", tc.tag, tc.latest, got, tc.want)
		}
	}
	if _, err := makeLatest("1.2.3", ""); !errors.Is(err, ErrBadTag) {
		t.Errorf("bad tag: err = %v, want ErrBadTag", err)
	}
	if _, err := makeLatest("v1.2.3", "latest"); err == nil {
		t.Error("unparsable latest: got nil error")
	}
	var out strings.Builder
	if err := run([]string{makeLatestCmd, tagFlag, "v1.2.5", "-latest", v130}, &out); err != nil || out.String() != "false\n" {
		t.Errorf("run make-latest: out %q, err %v; want \"false\\n\"", out.String(), err)
	}
}
