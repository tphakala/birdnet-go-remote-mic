//go:build unix

package update

import (
	"os"
	"syscall"
	"testing"
)

// TestMain pins the umask, so the directories tests create are 0755 however
// the caller's shell is set up: the root-only checks refuse a group-writable
// directory, and a 002 umask would make every fixture one.
func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	os.Exit(m.Run())
}
