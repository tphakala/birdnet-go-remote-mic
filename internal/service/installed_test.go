//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExecStartBin(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"[Service]\nExecStartPre=-/x serve --check\nExecStart=/usr/local/bin/remote-mic serve\n": "/usr/local/bin/remote-mic",
		"ExecStart=-/opt/remote-mic service apply-update\n":                                      "/opt/remote-mic",
		"[Service]\nType=oneshot\n":                                                              "",
		"ExecStart=\n":                                                                           "",
	}
	for unit, want := range tests {
		if got := execStartBin(unit); got != want {
			t.Errorf("execStartBin(%q) = %q, want %q", unit, got, want)
		}
	}
}

func TestInstalledBinPaths(t *testing.T) {
	orig := readUnitFile
	t.Cleanup(func() { readUnitFile = orig })
	readUnitFile = func(p string) ([]byte, error) {
		switch p {
		case "/etc/systemd/system/remote-mic.service":
			return Render(ServiceSpec{})
		case "/etc/systemd/system/remote-mic-update.service":
			return nil, os.ErrNotExist
		}
		return nil, errors.New("unexpected path " + p)
	}
	app, upd := InstalledBinPaths()
	if app != DefaultBinPath || upd != "" {
		t.Errorf("got %q, %q; want %q, \"\"", app, upd, DefaultBinPath)
	}
	readUnitFile = func(p string) ([]byte, error) {
		if p == "/etc/systemd/system/remote-mic-update.service" {
			_, svc, err := RenderUpdater(ServiceSpec{})
			return svc, err
		}
		return Render(ServiceSpec{})
	}
	if _, upd := InstalledBinPaths(); upd != DefaultBinPath {
		t.Errorf("updater bin path %q, want %q", upd, DefaultBinPath)
	}
}

// stubUnits serves unit and drop-in text from files, keyed by path, through the
// readUnitFile and globDropIns seams; a path absent from files does not exist.
func stubUnits(t *testing.T, files map[string]string) {
	t.Helper()
	origRead, origGlob := readUnitFile, globDropIns
	t.Cleanup(func() { readUnitFile, globDropIns = origRead, origGlob })
	readUnitFile = func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	globDropIns = func(pattern string) ([]string, error) {
		var out []string
		for p := range files {
			if ok, _ := filepath.Match(pattern, p); ok {
				out = append(out, p)
			}
		}
		return out, nil
	}
}

func TestInstalledConfig(t *testing.T) {
	const (
		userMic = "mic"
		unit    = "/etc/systemd/system/remote-mic.service"
		dropA   = "/etc/systemd/system/remote-mic.service.d/10-a.conf"
		dropB   = "/etc/systemd/system/remote-mic.service.d/20-b.conf"
	)
	rendered, err := Render(ServiceSpec{ConfigPath: "/srv/rm/config.yaml", User: userMic})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		files              map[string]string
		wantPath, wantUser string
	}{
		{"no unit", nil, "", ""},
		{"rendered unit", map[string]string{unit: string(rendered)}, "/srv/rm/config.yaml", userMic},
		{"no environment or user", map[string]string{unit: "[Service]\nExecStart=/x serve\n"}, "", ""},
		{"several assignments on one line", map[string]string{unit: "[Service]\nEnvironment=A=1 REMOTEMIC_CONFIG=/a.yaml B=2\n"}, "/a.yaml", ""},
		{"quoted assignment", map[string]string{unit: "[Service]\nEnvironment=\"REMOTEMIC_CONFIG=/my dir/c.yaml\" A=1\n"}, "/my dir/c.yaml", ""},
		{"quoted value", map[string]string{unit: "[Service]\nEnvironment=REMOTEMIC_CONFIG='/q/c.yaml'\n"}, "/q/c.yaml", ""},
		{"later line wins", map[string]string{unit: "[Service]\nEnvironment=REMOTEMIC_CONFIG=/a.yaml\nEnvironment=REMOTEMIC_CONFIG=/b.yaml\n"}, "/b.yaml", ""},
		{"empty environment resets", map[string]string{unit: "[Service]\nEnvironment=REMOTEMIC_CONFIG=/a.yaml\nEnvironment=\n"}, "", ""},
		{"relative value rejected", map[string]string{unit: "[Service]\nEnvironment=REMOTEMIC_CONFIG=config.yaml\nUser=mic\n"}, "", userMic},
		{"outside the service section", map[string]string{unit: "[Unit]\nUser=nobody\nEnvironment=REMOTEMIC_CONFIG=/u.yaml\n"}, "", ""},
		{"drop-ins override in lexical order", map[string]string{
			unit:  string(rendered),
			dropB: "[Service]\nEnvironment=REMOTEMIC_CONFIG=/b.yaml\n",
			dropA: "[Service]\nEnvironment=REMOTEMIC_CONFIG=/a.yaml\nUser=other\n",
		}, "/b.yaml", "other"},
		{"drop-in without the unit ignored", map[string]string{dropA: "[Service]\nEnvironment=REMOTEMIC_CONFIG=/a.yaml\n"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubUnits(t, tc.files)
			path, user := InstalledConfig()
			if path != tc.wantPath || user != tc.wantUser {
				t.Errorf("got %q, %q; want %q, %q", path, user, tc.wantPath, tc.wantUser)
			}
		})
	}
}
