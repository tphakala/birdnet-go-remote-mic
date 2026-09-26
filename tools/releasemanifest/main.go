// Command releasemanifest writes and signs the release manifest
// (manifest.json and manifest.json.sig) that update checks read; see
// internal/releasemanifest for the format.
//
// The release workflow runs it around GoReleaser: check-key before anything is
// published, so a missing or untrusted signing key fails the release early
// rather than after the release is live; generate after GoReleaser has built
// the archives. The signing key comes from the RELEASE_MANIFEST_KEY
// environment variable (a base64 Ed25519 seed), never from a flag, so it cannot
// land in a process listing or a shell history.
//
// Usage, from the repository root:
//
//	go run ./tools/releasemanifest keygen -out <file>    # new signing key pair
//	go run ./tools/releasemanifest check-key             # is RELEASE_MANIFEST_KEY trusted?
//	go run ./tools/releasemanifest generate [-tag vX.Y.Z] [-dist dist]
//	go run ./tools/releasemanifest verify [-dist dist]
package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// keyEnv holds the base64 Ed25519 seed that signs the manifest.
const keyEnv = "RELEASE_MANIFEST_KEY"

// requiredTargets mirrors the release builds in .goreleaser.yaml. A release
// missing one would leave those appliances without an update, so generate
// refuses it.
var requiredTargets = []string{"linux/amd64", "linux/arm64", "linux/armv6"}

// Sentinel errors, for errors.Is in tests.
var (
	ErrNoKey           = errors.New(keyEnv + " is not set")
	ErrTagMismatch     = errors.New("tag does not match the GoReleaser build")
	ErrChecksum        = errors.New("archive checksum mismatch")
	ErrMissingTarget   = errors.New("release is missing a target")
	ErrDuplicateTarget = errors.New("release has two archives for one target")
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "releasemanifest:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: releasemanifest keygen|check-key|generate|verify [flags]")
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dist := fs.String("dist", "dist", "GoReleaser output directory")
	switch cmd {
	case "keygen":
		out := fs.String("out", "", "file to write the private key to (created, mode 0600; must not exist)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("keygen: -out is required")
		}
		return keygen(*out, stdout)
	case "check-key":
		if err := fs.Parse(args); err != nil {
			return err
		}
		priv, trusted, err := signingKey()
		if err != nil {
			return err
		}
		return checkKey(priv, trusted, stdout)
	case "generate":
		tag := fs.String("tag", "", "expected release tag; must match the GoReleaser build when set")
		if err := fs.Parse(args); err != nil {
			return err
		}
		priv, trusted, err := signingKey()
		if err != nil {
			return err
		}
		m, err := generate(*dist, *tag, priv, trusted)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "wrote %s and %s for %s (%s)\n", releasemanifest.FileName,
			releasemanifest.SignatureFileName, m.Version, strings.Join(slices.Sorted(maps.Keys(m.Targets)), ", "))
		return err
	case "verify":
		if err := fs.Parse(args); err != nil {
			return err
		}
		trusted, err := releasemanifest.TrustedKeys()
		if err != nil {
			return err
		}
		m, err := verifyFiles(*dist, trusted)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s verifies: %s, %d targets\n", releasemanifest.FileName, m.Version, len(m.Targets))
		return err
	default:
		return fmt.Errorf("unknown command %q (want keygen, check-key, generate or verify)", cmd)
	}
}

// keygen writes a new private key (base64 seed) to out and prints the public
// key to add to internal/releasemanifest/keys.go.
func keygen(out string, stdout io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "private key written to %s (store it as the %s secret, then keep it offline)\npublic key: %s\nkey id:     %s\n",
		out, keyEnv, base64.StdEncoding.EncodeToString(pub), releasemanifest.KeyID(pub))
	return err
}

// signingKey reads the private key from the environment and the trusted keys
// compiled into this build.
func signingKey() (ed25519.PrivateKey, map[string]ed25519.PublicKey, error) {
	raw := os.Getenv(keyEnv)
	if strings.TrimSpace(raw) == "" {
		return nil, nil, ErrNoKey
	}
	priv, err := releasemanifest.ParsePrivateKey(raw)
	if err != nil {
		return nil, nil, err
	}
	trusted, err := releasemanifest.TrustedKeys()
	if err != nil {
		return nil, nil, err
	}
	return priv, trusted, nil
}

// checkKey fails unless priv's public key is one this build trusts, so a
// signature made with it would verify on an appliance.
func checkKey(priv ed25519.PrivateKey, trusted map[string]ed25519.PublicKey, stdout io.Writer) error {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("private key has no Ed25519 public key")
	}
	id := releasemanifest.KeyID(pub)
	if want, ok := trusted[id]; !ok || !want.Equal(pub) {
		return fmt.Errorf("%w: %s key id %s is not in internal/releasemanifest/keys.go", releasemanifest.ErrUntrustedKey, keyEnv, id)
	}
	_, err := fmt.Fprintf(stdout, "%s is trusted (key id %s)\n", keyEnv, id)
	return err
}

// metadata is the part of GoReleaser's dist/metadata.json read here.
type metadata struct {
	Tag  string    `json:"tag"`
	Date time.Time `json:"date"`
}

// artifact is the part of one dist/artifacts.json entry read here.
type artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	GOARM  string `json:"goarm"`
	Type   string `json:"type"`
	Extra  struct {
		Checksum string `json:"Checksum"`
	} `json:"extra"`
}

// generate builds the manifest from the GoReleaser output in dist, signs it,
// verifies the signature against the trusted keys as an appliance would, and
// writes manifest.json and manifest.json.sig into dist.
func generate(dist, wantTag string, priv ed25519.PrivateKey, trusted map[string]ed25519.PublicKey) (*releasemanifest.Manifest, error) {
	var meta metadata
	if err := readJSON(filepath.Join(dist, "metadata.json"), &meta); err != nil {
		return nil, err
	}
	if wantTag != "" && meta.Tag != wantTag {
		return nil, fmt.Errorf("%w: GoReleaser built %q, expected %q", ErrTagMismatch, meta.Tag, wantTag)
	}
	var arts []artifact
	if err := readJSON(filepath.Join(dist, "artifacts.json"), &arts); err != nil {
		return nil, err
	}
	sums, err := readChecksums(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		return nil, err
	}

	m := &releasemanifest.Manifest{
		Schema:   releasemanifest.Schema,
		Version:  meta.Tag,
		Date:     meta.Date.UTC().Truncate(time.Second),
		NotesURL: "https://github.com/" + releasemanifest.Repository + "/releases/tag/" + meta.Tag,
		Targets:  map[string]releasemanifest.Target{},
	}
	for i := range arts {
		a := &arts[i]
		if a.Type != "Archive" || a.GOOS != "linux" {
			continue
		}
		key := releasemanifest.TargetKey(a.GOOS, a.GOARCH, a.GOARM)
		if _, dup := m.Targets[key]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateTarget, key)
		}
		t, err := archiveTarget(a, meta.Tag, sums)
		if err != nil {
			return nil, err
		}
		m.Targets[key] = t
	}
	for _, key := range requiredTargets {
		if _, ok := m.Targets[key]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrMissingTarget, key)
		}
	}

	body, err := releasemanifest.Marshal(m)
	if err != nil {
		return nil, err
	}
	sig, err := releasemanifest.Sign(priv, body)
	if err != nil {
		return nil, err
	}
	// Verify what is about to be published exactly as an appliance will, so a
	// key missing from keys.go fails here and not on every appliance.
	if _, err := releasemanifest.Verify(body, sig, trusted); err != nil {
		return nil, fmt.Errorf("signed manifest does not verify: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dist, releasemanifest.FileName), body, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dist, releasemanifest.SignatureFileName), sig, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// archiveTarget hashes one archive and cross-checks the digest against
// checksums.txt and GoReleaser's own record, so the manifest cannot disagree
// with the checksums published beside it.
func archiveTarget(a *artifact, tag string, sums map[string]string) (releasemanifest.Target, error) {
	f, err := os.Open(a.Path)
	if err != nil {
		return releasemanifest.Target{}, err
	}
	defer f.Close() //nolint:errcheck // read-only file; a close error cannot lose data
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return releasemanifest.Target{}, fmt.Errorf("%s: %w", a.Path, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	listed, ok := sums[a.Name]
	if !ok {
		return releasemanifest.Target{}, fmt.Errorf("%w: %s is not in checksums.txt", ErrChecksum, a.Name)
	}
	if listed != sum {
		return releasemanifest.Target{}, fmt.Errorf("%w: %s hashes to %s, checksums.txt lists %s", ErrChecksum, a.Name, sum, listed)
	}
	if rec := a.Extra.Checksum; rec != "" && rec != "sha256:"+sum {
		return releasemanifest.Target{}, fmt.Errorf("%w: %s hashes to %s, GoReleaser recorded %s", ErrChecksum, a.Name, sum, rec)
	}
	return releasemanifest.Target{
		URL:    "https://github.com/" + releasemanifest.Repository + "/releases/download/" + tag + "/" + a.Name,
		Size:   size,
		SHA256: sum,
	}, nil
}

// readChecksums parses a sha256sum-format file: "<hex>  <name>" per line.
func readChecksums(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok {
			return nil, fmt.Errorf("%s: malformed line %q", path, line)
		}
		sums[strings.TrimPrefix(name, "*")] = strings.ToLower(sum)
	}
	return sums, sc.Err()
}

// verifyFiles checks manifest.json and manifest.json.sig in dist.
func verifyFiles(dist string, trusted map[string]ed25519.PublicKey) (*releasemanifest.Manifest, error) {
	body, err := os.ReadFile(filepath.Join(dist, releasemanifest.FileName))
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(dist, releasemanifest.SignatureFileName))
	if err != nil {
		return nil, err
	}
	return releasemanifest.Verify(body, sig, trusted)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
