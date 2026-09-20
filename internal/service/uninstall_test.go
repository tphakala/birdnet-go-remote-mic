//go:build linux

package service

import "testing"

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
	}
}

func TestUninstallKeepsData(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	if err := testUninstaller(&events, init, true).Uninstall(false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	wantSeq(t, events, []string{
		"stop remote-mic.service",
		"disable remote-mic.service",
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
		"stop remote-mic.service",
		"disable remote-mic.service",
		"rm /etc/systemd/system/remote-mic.service",
		evReload,
		"rm /usr/local/bin/remote-mic",
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
