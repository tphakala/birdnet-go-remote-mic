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
