//go:build linux

package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWritePreservesOwner guards the regression the ownership fix protects
// against: rewriting an existing file must not change its owner. The
// cross-uid benefit (root rewriting a service-user file keeps the service
// user) needs privilege to exercise and is validated on the appliance; here a
// same-owner rewrite must at least leave uid and gid untouched.
func TestWritePreservesOwner(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := Write(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := ownerOf(t, p)
	if err := Write(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if after := ownerOf(t, p); after != before {
		t.Fatalf("owner changed on rewrite: %v -> %v", before, after)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "two" {
		t.Fatalf("content = %q err %v, want \"two\"", b, err)
	}
}

// TestPreserveOwnerChownsToTarget observes the mechanism a same-uid rewrite
// cannot reveal through the resulting file: preserveOwner stats the target and
// chowns the temp file to its uid/gid. Deleting the preserveOwner call makes
// this fail (chownFile is never invoked).
func TestPreserveOwnerChownsToTarget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := Write(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := ownerOf(t, p)

	orig := chownFile
	t.Cleanup(func() { chownFile = orig })
	var called bool
	var gotUID, gotGID int
	chownFile = func(f *os.File, uid, gid int) error {
		called, gotUID, gotGID = true, uid, gid
		return f.Chown(uid, gid)
	}
	if err := Write(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("preserveOwner did not chown the temp file on a rewrite")
	}
	if uint32(gotUID) != want[0] || uint32(gotGID) != want[1] {
		t.Errorf("chown(%d,%d), want target owner (%d,%d)", gotUID, gotGID, want[0], want[1])
	}
}

func ownerOf(t *testing.T, p string) [2]uint32 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no syscall.Stat_t")
	}
	return [2]uint32{st.Uid, st.Gid}
}

// TestReplaceDoesNotKeepOwner pins that Replace never carries the old file's
// owner over: a file planted by someone else must not stay theirs once root
// writes the binary. Restoring the preserveOwner call makes chownFile run.
func TestReplaceDoesNotKeepOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := chownFile
	t.Cleanup(func() { chownFile = orig })
	chownFile = func(*os.File, int, int) error {
		t.Error("Replace chowned the new file to the old file's owner")
		return nil
	}
	if err := Replace(p, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
}
