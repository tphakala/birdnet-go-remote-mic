//go:build unix

package update

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileOwner pins that fileOwner reads the real owner, the one fact the
// root-only checks rest on (the other tests stand in a fake owner).
func TestFileOwner(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := fileOwner(fi)
	if !ok || int(uid) != os.Getuid() || int(gid) != os.Getegid() {
		t.Errorf("fileOwner = %d, %d, %t, want %d, %d, true", uid, gid, ok, os.Getuid(), os.Getegid())
	}
}
