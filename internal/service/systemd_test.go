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
		"enabled":          {"enabled\n", errors.New("exit 0 not guaranteed"), true, true, false},
		"disabled":         {"disabled\n", errors.New("exit 1"), true, false, false},
		"static":           {"static\n", nil, true, true, false},
		"active":           {"active\n", nil, false, true, false},
		"reloading":        {"reloading\n", nil, false, true, false},
		"inactive":         {"inactive\n", errors.New("exit 3"), false, false, false},
		"not-found word":   {"not-found\n", errors.New("exit 4"), true, false, false},
		"broken systemctl": {"", errors.New("exec: systemctl not found"), true, false, true},
		"unexpected word":  {"weird\n", nil, true, false, true}, // exit 0 but unknown word -> unknown, not false
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

// TestSystemdRestartAndMainPID pins the two calls the root updater makes: the
// exact systemctl invocations and the MainPID parse, including a stopped unit
// (MainPID 0) and output that is not a number.
func TestSystemdRestartAndMainPID(t *testing.T) {
	out := "4242\n"
	fr := &fakeRunner{resp: func(name string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show" {
			return []byte(out), nil
		}
		return nil, nil
	}}
	sd := &Systemd{Run: fr.run}
	if err := sd.Restart("remote-mic.service"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	pid, err := sd.MainPID("remote-mic.service")
	if err != nil || pid != 4242 {
		t.Errorf("MainPID = %d, %v; want 4242", pid, err)
	}
	fr.wantSeq(t,
		"systemctl restart remote-mic.service",
		"systemctl show --property=MainPID --value remote-mic.service",
	)
	out = "0\n"
	if pid, err := sd.MainPID("remote-mic.service"); err != nil || pid != 0 {
		t.Errorf("stopped unit: MainPID = %d, %v; want 0", pid, err)
	}
	out = "MainPID=4242\n"
	if _, err := sd.MainPID("remote-mic.service"); err == nil {
		t.Error("unparsable output: got nil error")
	}
}
