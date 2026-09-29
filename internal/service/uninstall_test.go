//go:build linux

package service

import (
	"errors"
	"strings"
	"testing"
)

func testUninstaller(events *[]string, init *fakeInit, userThere bool) *Uninstaller {
	return &Uninstaller{
		Spec: ServiceSpec{},
		Init: init,
		Run: func(name string, args ...string) ([]byte, error) {
			*events = append(*events, "run "+call{name: name, args: args}.line())
			return nil, nil
		},
		removeFile: func(p string) error { *events = append(*events, "rm "+p); return nil },
		removeAll:  func(p string) error { *events = append(*events, "rmall "+p); return nil },
		userExists: func(string) bool { return userThere },
		dirsOK:     func(ServiceSpec) error { *events = append(*events, "dirs"); return nil },
	}
}

func TestUninstallKeepsData(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	if err := testUninstaller(&events, init, true).Uninstall(false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	wantSeq(t, events, []string{
		evStopPath,
		evDisablePath,
		evStopUpdater,
		evResetPath,
		evResetUpdater,
		"stop remote-mic.service",
		"disable remote-mic.service",
		"rm /etc/systemd/system/remote-mic-update.path",
		"rm /etc/systemd/system/remote-mic-update.service",
		"rm /etc/systemd/system/remote-mic.service",
		evReload,
	})
}

func TestUninstallPurge(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	if err := testUninstaller(&events, init, true).Uninstall(true); err != nil {
		t.Fatalf("Uninstall purge: %v", err)
	}
	wantSeq(t, events, []string{
		"dirs",
		evStopPath,
		evDisablePath,
		evStopUpdater,
		evResetPath,
		evResetUpdater,
		"stop remote-mic.service",
		"disable remote-mic.service",
		"rm /etc/systemd/system/remote-mic-update.path",
		"rm /etc/systemd/system/remote-mic-update.service",
		"rm /etc/systemd/system/remote-mic.service",
		evReload,
		"rm /usr/local/bin/remote-mic",
		"rm /usr/local/bin/remote-mic.prev",
		"rm /usr/local/bin/remote-mic.new",
		"rm /usr/local/bin/remote-mic.pending",
		"rmall /etc/remote-mic",
		"rmall /var/lib/remote-mic",
		"run userdel remote-mic",
	})
}

func TestUninstallPurgeSkipsUserdelWhenAbsent(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	if err := testUninstaller(&events, init, false).Uninstall(true); err != nil {
		t.Fatalf("Uninstall purge: %v", err)
	}
	for _, e := range events {
		if e == "run userdel remote-mic" {
			t.Error("userdel should be skipped when the user does not exist")
		}
	}
}

// TestUninstallPurgeRefusesADirectoryOverTheBinDir pins that a purge whose
// config or state directory reaches the bin directory stops before any
// teardown, while a plain uninstall does not look.
func TestUninstallPurgeRefusesADirectoryOverTheBinDir(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	un := testUninstaller(&events, init, true)
	un.dirsOK = func(ServiceSpec) error { return errors.New("the state directory /var/lib/remote-mic is /usr/local") }
	err := un.Uninstall(true)
	if err == nil || !strings.Contains(err.Error(), "refusing to purge") {
		t.Fatalf("Uninstall(true) = %v, want a refusal", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want none before the refusal", events)
	}
	if err := un.Uninstall(false); err != nil {
		t.Errorf("Uninstall(false) = %v, want nil", err)
	}
}
