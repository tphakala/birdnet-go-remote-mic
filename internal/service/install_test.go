//go:build linux

package service

import (
	"fmt"
	"os"
	"testing"
)

// testInstaller wires an Installer whose every side effect logs into events, so
// a test asserts the exact ordering across user, filesystem, and systemd steps.
// userThere toggles the user-already-exists path.
func testInstaller(events *[]string, init *fakeInit, userThere *bool) *Installer {
	return &Installer{
		Spec: ServiceSpec{},
		Init: init,
		Run: func(name string, args ...string) ([]byte, error) {
			*events = append(*events, "run "+call{name: name, args: args}.line())
			return nil, nil
		},
		Plat:       Platform{Family: FamilyDebian},
		selfExe:    func() (string, error) { return "/home/pi/remote-mic", nil },
		userExists: func(string) bool { return *userThere },
		lookupUser: func(string) (int, int, error) { return 990, 990, nil },
		ensureDir:  func(p string, _ os.FileMode) error { *events = append(*events, "mkdir "+p); return nil },
		chownTree: func(root string, uid, gid int) error {
			*events = append(*events, fmt.Sprintf("chown %s %d:%d", root, uid, gid))
			return nil
		},
		copyFile: func(src, dst string, _ os.FileMode) error {
			*events = append(*events, "copy "+src+" -> "+dst)
			return nil
		},
		writeFile: func(path string, _ []byte, _ os.FileMode) error { *events = append(*events, "write "+path); return nil },
	}
}

func TestInstallSequence(t *testing.T) {
	// NologinShell probes the filesystem; pin it so the useradd shell is stable.
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = func(p string) bool { return p == "/usr/sbin/nologin" }

	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := false
	in := testInstaller(&events, init, &userThere)

	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantSeq(t, events, []string{
		"run groupadd --system --force remote-mic",
		"run useradd --system --no-create-home --shell /usr/sbin/nologin --gid remote-mic remote-mic",
		"run usermod --append --groups audio remote-mic",
		"mkdir /usr/local/bin",
		"copy /home/pi/remote-mic -> /usr/local/bin/remote-mic",
		"write /etc/systemd/system/remote-mic.service",
		"mkdir /etc/remote-mic",
		"chown /etc/remote-mic 990:990",
		"mkdir /var/lib/remote-mic",
		"chown /var/lib/remote-mic 990:990",
		"reload",
		"enable --now remote-mic.service",
	})
}

// TestInstallChownBeforeStart is the regression guard for the review's ordering
// finding: the config directory must be owned by the service user before the
// unit starts, or first-provision and the run lock fail on a root-owned dir.
func TestInstallChownBeforeStart(t *testing.T) {
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = func(p string) bool { return p == "/usr/sbin/nologin" }

	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := false
	if err := testInstaller(&events, init, &userThere).Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	chownIdx, enableIdx := -1, -1
	for i, e := range events {
		if e == "chown /etc/remote-mic 990:990" {
			chownIdx = i
		}
		if e == "enable --now remote-mic.service" {
			enableIdx = i
		}
	}
	if chownIdx < 0 || enableIdx < 0 {
		t.Fatalf("missing events: chown=%d enable=%d in %v", chownIdx, enableIdx, events)
	}
	if chownIdx > enableIdx {
		t.Errorf("chown (%d) must precede enable/start (%d)", chownIdx, enableIdx)
	}
}

func TestInstallUserExistsSkipsCreation(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	if err := testInstaller(&events, init, &userThere).Install(false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, e := range events {
		if e == "run groupadd --system --force remote-mic" ||
			e == "run usermod --append --groups audio remote-mic" {
			t.Errorf("existing user should not trigger creation: saw %q", e)
		}
	}
	// now=false enables without starting.
	last := events[len(events)-1]
	if last != "enable remote-mic.service" {
		t.Errorf("last event = %q, want %q", last, "enable remote-mic.service")
	}
}

func TestInstallRefusesWithoutSystemd(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: false}
	userThere := false
	err := testInstaller(&events, init, &userThere).Install(true)
	if err == nil {
		t.Fatal("Install without systemd = nil error, want refusal")
	}
	if len(events) != 0 {
		t.Errorf("no side effects expected before the systemd check, got %v", events)
	}
}
