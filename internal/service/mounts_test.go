//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseMountPointsDecodesEscapes(t *testing.T) {
	t.Parallel()
	in := "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
		`40 22 0:35 / /mnt/my\040disk rw - tmpfs tmpfs rw` + "\n" +
		`41 22 0:36 / /mnt/back\134slash rw - tmpfs tmpfs rw` + "\n"
	got, err := parseMountPoints(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/mnt/my disk", `/mnt/back\slash`}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseMountPointsRefusesAShortLine(t *testing.T) {
	t.Parallel()
	if _, err := parseMountPoints(strings.NewReader("22 1 8:1\n")); err == nil {
		t.Error("a line with fewer than five fields was accepted")
	}
}

func TestMountPointsListsTheRoot(t *testing.T) {
	t.Parallel()
	got, err := mountPoints()
	if err != nil {
		t.Skipf("no mount list on this host: %v", err)
	}
	if !slices.Contains(got, "/") {
		t.Errorf("mount list %q does not contain /", got)
	}
}

func TestPurgeDirsOK(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf, state := filepath.Join(root, "etc"), filepath.Join(root, "state")
	for _, d := range []string{conf, state, filepath.Join(root, "state-other")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	spec := func(cfgDir, stateDir string) ServiceSpec {
		return ServiceSpec{ConfigPath: filepath.Join(cfgDir, "config.yaml"), StateDir: stateDir}
	}
	list := func(m ...string) func() ([]string, error) {
		return func() ([]string, error) { return m, nil }
	}
	tests := []struct {
		name    string
		spec    ServiceSpec
		mounts  func() ([]string, error)
		wantErr string
	}{
		{"mount inside the state directory", spec(conf, state), list("/", filepath.Join(state, "bin")), "state directory"},
		{"mount inside the config directory", spec(conf, state), list(filepath.Join(conf, "deep", "er")), "config directory"},
		{"unreadable mount list", spec(conf, state), func() ([]string, error) { return nil, errors.New("boom") }, "mount list"},
		{"mount at the directory itself", spec(conf, state), list(state, conf), ""},
		{"sibling sharing a prefix", spec(conf, state), list(filepath.Join(root, "state-other")), ""},
		{"symlinked directory", spec(conf, link), list(filepath.Join(state, "bin")), ""},
		{"missing directories", spec(filepath.Join(root, "none"), filepath.Join(root, "gone")), func() ([]string, error) { return nil, errors.New("not read") }, ""},
	}
	for _, tt := range tests {
		err := purgeDirsOK(tt.spec, tt.mounts)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: got %v, want an error containing %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestCheckPurgeDirsStillRefusesADirectoryOverTheBinDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.Symlink(bin, state); err != nil {
		t.Fatal(err)
	}
	s := ServiceSpec{BinPath: filepath.Join(bin, "remote-mic"), ConfigPath: filepath.Join(root, "etc", "config.yaml"), StateDir: state}
	if err := checkPurgeDirs(s); err == nil || !strings.Contains(err.Error(), "bin directory") {
		t.Errorf("got %v, want the bin directory refusal", err)
	}
}
