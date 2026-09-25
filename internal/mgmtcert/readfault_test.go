package mgmtcert

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestReadFaultClassifies covers every branch of readFault, including an Lstat
// that fails for a reason other than "does not exist" (a permission or I/O
// fault on the directory), which no real filesystem fixture triggers reliably.
func TestReadFaultClassifies(t *testing.T) {
	t.Parallel()
	errPerm := fs.ErrPermission
	errIO := errors.New("input/output error")
	// Lstat of the working directory stands in for "the link itself exists".
	lstatOK := func(string) (fs.FileInfo, error) { return os.Lstat(".") }
	lstatMissing := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	lstatIO := func(string) (fs.FileInfo, error) { return nil, errIO }
	for _, tc := range []struct {
		name    string
		readErr error
		lstat   func(string) (fs.FileInfo, error)
		// wantCause is the error the *PinnedReadError must carry; nil means
		// readFault must return nil.
		wantCause error
	}{
		{"read worked", nil, lstatIO, nil},
		{"read refused", errPerm, lstatOK, errPerm},
		{"file itself missing", fs.ErrNotExist, lstatMissing, nil},
		{"dangling symlink", fs.ErrNotExist, lstatOK, errDanglingLink},
		{"lstat fails otherwise", fs.ErrNotExist, lstatIO, errIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := readFault("/pinned/cert.pem", tc.readErr, tc.lstat)
			if tc.wantCause == nil {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			pe, ok := errors.AsType[*PinnedReadError](err)
			if !ok {
				t.Fatalf("got %v, want a *PinnedReadError", err)
			}
			if !errors.Is(pe.Err, tc.wantCause) {
				t.Errorf("got cause %v, want %v", pe.Err, tc.wantCause)
			}
			if pe.Path != "/pinned/cert.pem" {
				t.Errorf("got path %q, want the read path", pe.Path)
			}
			// A refusal must never look like "missing, safe to regenerate".
			if errors.Is(err, fs.ErrNotExist) {
				t.Error("a *PinnedReadError must not match fs.ErrNotExist")
			}
		})
	}
}
