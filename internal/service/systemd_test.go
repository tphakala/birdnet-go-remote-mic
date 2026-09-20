//go:build linux

package service

import (
	"errors"
	"testing"
)

func TestSystemdCommands(t *testing.T) {
	fr := &fakeRunner{}
	sd := &Systemd{Run: fr.run}

	if err := sd.DaemonReload(); err != nil {
		t.Fatalf("DaemonReload: %v", err)
	}
	if err := sd.Enable("remote-mic.service", false); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := sd.Enable("remote-mic.service", true); err != nil {
		t.Fatalf("Enable now: %v", err)
	}
	if err := sd.Stop("remote-mic.service"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := sd.Disable("remote-mic.service"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	fr.wantSeq(t,
		"systemctl daemon-reload",
		"systemctl enable remote-mic.service",
		"systemctl enable --now remote-mic.service",
		"systemctl stop remote-mic.service",
		"systemctl disable remote-mic.service",
	)
}

func TestSystemdQuery(t *testing.T) {
	// systemctl exits nonzero for a negative state while still printing the
	// word; the query must read the word, not the exit code.
	states := map[string]struct {
		out     string
		err     error
		enabled bool // true = IsEnabled, false = IsActive
		want    bool
		wantErr bool
	}{
		"enabled":  {"enabled\n", errors.New("exit 0 not guaranteed"), true, true, false},
		"disabled": {"disabled\n", errors.New("exit 1"), true, false, false},
		"static":   {"static\n", nil, true, true, false},
		"active":   {"active\n", nil, false, true, false},
		"inactive": {"inactive\n", errors.New("exit 3"), false, false, false},
		"notfound": {"", errors.New("Failed to get unit file state: No such file"), true, false, true},
	}
	for name, c := range states {
		t.Run(name, func(t *testing.T) {
			fr := &fakeRunner{resp: func(_ string, _ []string) ([]byte, error) {
				return []byte(c.out), c.err
			}}
			sd := &Systemd{Run: fr.run}
			var (
				got bool
				err error
			)
			if c.enabled {
				got, err = sd.IsEnabled("remote-mic.service")
			} else {
				got, err = sd.IsActive("remote-mic.service")
			}
			if got != c.want {
				t.Errorf("state = %v, want %v", got, c.want)
			}
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestSystemdPresent(t *testing.T) {
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = func(p string) bool { return p == "/run/systemd/system" }
	if !(&Systemd{}).Present() {
		t.Error("Present() = false, want true when /run/systemd/system exists")
	}
	fileExists = func(string) bool { return false }
	if (&Systemd{}).Present() {
		t.Error("Present() = true, want false when marker absent")
	}
}
