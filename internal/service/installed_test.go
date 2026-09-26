//go:build linux

package service

import (
	"errors"
	"os"
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
