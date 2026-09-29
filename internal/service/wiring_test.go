//go:build linux

package service

import (
	"os"
	"reflect"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// wantWired fails unless every function field of the struct v points at, is
// set to the production function in want (a nil entry means a closure, which
// is only checked to be set), and unless every function field has an entry: a seam that a test replaces is otherwise a nil function in the
// binary the day its wiring is dropped, and a new seam cannot go unwired.
func wantWired(t *testing.T, v any, want map[string]any) {
	t.Helper()
	rv := reflect.ValueOf(v).Elem()
	for i := range rv.NumField() {
		f := rv.Type().Field(i)
		if f.Type.Kind() != reflect.Func {
			continue
		}
		w, ok := want[f.Name]
		if !ok {
			t.Errorf("%s has no entry in the wiring table", f.Name)
			continue
		}
		if w == nil {
			if rv.Field(i).IsNil() {
				t.Errorf("%s is not set", f.Name)
			}
			continue
		}
		if got := rv.Field(i).Pointer(); got != reflect.ValueOf(w).Pointer() {
			t.Errorf("%s is not wired to its production function", f.Name)
		}
	}
	for name := range want {
		if _, ok := rv.Type().FieldByName(name); !ok {
			t.Errorf("the wiring table names %s, which is not a field", name)
		}
	}
}

func TestNewInstallerWiresEverySeam(t *testing.T) {
	t.Parallel()
	in := NewInstaller(ServiceSpec{})
	wantWired(t, in, map[string]any{
		"Run":             execRunner,
		"selfExe":         os.Executable,
		"userExists":      userExists,
		"lookupUser":      lookupUser,
		"ensureDir":       ensureDir,
		"binDirOK":        checkBinDir,
		"makeBinDir":      ensureBinDir,
		"dirsOK":          checkNotOverBinDir,
		"trustedBin":      update.CheckRootOnlyFile,
		"lockBin":         lockBin,
		"binVersion":      installedVersion,
		"isLink":          isSymlink,
		"chownTree":       chownTree,
		"copyFile":        copyFile,
		"writeFile":       atomicfile.Write,
		"stagingDir":      ensureStagingDir,
		"withdrawRequest": withdrawStagedRequest,
		"rootOnly":        update.CheckRootOnlyFile,
		"removeFile":      os.Remove,
		"lexists":         lexists,
		"syncDir":         atomicfile.SyncDir,
		"packageOwns":     PackageOwns,
		"installed":       InstalledSpec,
		"execStartDropIn": nil,
		"removeStaging":   removeStagingDir,
		"isRegular":       isRegularFile,
	})
	if in.Init == nil || in.warn != os.Stderr {
		t.Error("Init or warn is not set")
	}
}

func TestNewUninstallerWiresEverySeam(t *testing.T) {
	t.Parallel()
	un := NewUninstaller(ServiceSpec{})
	wantWired(t, un, map[string]any{
		"Run":         execRunner,
		"removeFile":  os.Remove,
		"removeAll":   os.RemoveAll,
		"userExists":  userExists,
		"dirsOK":      checkPurgeDirs,
		"packageOwns": PackageOwns,
	})
	if un.Init == nil {
		t.Error("Init is not set")
	}
}
