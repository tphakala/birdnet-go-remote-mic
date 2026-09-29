//go:build linux

package service

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

const (
	handRun  = "/home/pi/remote-mic"
	pkgBin   = "/usr/bin/remote-mic"
	copyBin  = "/usr/local/bin/remote-mic"
	pkgUnit  = "/etc/systemd/system/remote-mic.service"
	evLockCp = "lock " + copyBin
)

// packagedInstaller is a testInstaller whose running binary is the .deb's,
// installing to that same path, with an earlier copy install described by prev
// (nil for none). The unit's contents are captured in *unit.
func packagedInstaller(events *[]string, init *fakeInit, prev *ServiceSpec, unit *string) *Installer {
	userThere := true
	in := testInstaller(events, init, &userThere)
	in.Spec = ServiceSpec{BinPath: pkgBin}
	in.selfExe = func() (string, error) { return pkgBin, nil }
	in.packageOwns = func(p string) bool { return p == pkgBin }
	in.installed = func() (ServiceSpec, error) {
		if prev == nil {
			return ServiceSpec{}, nil
		}
		return *prev, nil
	}
	in.lockBin = func(p string, _ func()) (func(), error) {
		*events = append(*events, "lock "+p)
		return func() {}, nil
	}
	in.writeFile = func(path string, data []byte, _ os.FileMode) error {
		*events = append(*events, "write "+path)
		if path == pkgUnit && unit != nil {
			*unit = string(data)
		}
		return nil
	}
	return in
}

func copyInstall() *ServiceSpec {
	s := ServiceSpec{User: DefaultUser, ConfigPath: DefaultConfigPath, StateDir: DefaultStateDir, BinPath: copyBin}
	return &s
}

// TestInstallPackagedRunsThePackageBinary asserts that a fresh install of the
// .deb's binary points the unit at it and installs neither a copy, nor the
// updater units, nor a staging directory.
func TestInstallPackagedRunsThePackageBinary(t *testing.T) {
	var events []string
	var unit string
	init := &fakeInit{events: &events, present: true}
	in := packagedInstaller(&events, init, nil, &unit)
	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.Contains(unit, "ExecStart="+pkgBin+" serve ") {
		t.Errorf("unit %q, want ExecStart running %s", unit, pkgBin)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "copy ") || strings.HasPrefix(e, "staging ") || strings.HasPrefix(e, "lock ") || strings.Contains(e, "remote-mic-update") && strings.HasPrefix(e, "write ") {
			t.Errorf("event %q, want no copy, staging, lock or updater unit write in a packaged install", e)
		}
	}
	wantOrder(t, events, "write "+pkgUnit, evChownConfig, evStopPath, "rmstaging /var/lib/remote-mic", evReload, evEnableNowApp)
	if slices.Contains(events, "enable --now remote-mic-update.path") || slices.Contains(events, "restart remote-mic.service") {
		t.Errorf("events %v: a fresh packaged install enables no updater and restarts nothing", events)
	}
}

// TestInstallPackagedRefusesAnotherBinary asserts that another binary is not
// installed over a path the package owns, and nothing is written.
func TestInstallPackagedRefusesAnotherBinary(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	in := packagedInstaller(&events, init, nil, nil)
	in.selfExe = func() (string, error) { return handRun, nil }
	err := in.Install(true)
	if err == nil || !strings.Contains(err.Error(), "belongs to the .deb package") {
		t.Fatalf("Install = %v, want a refusal naming the package", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "write ") || strings.HasPrefix(e, "copy ") || strings.HasPrefix(e, "enable") {
			t.Errorf("event %q after a refusal", e)
		}
	}
}

// TestInstallMigratesFromACopy asserts the order of a migration from a copy
// install: the copy is locked, the new unit is enabled and the running
// appliance restarted onto the package binary, and only then are the copy and
// the files an update leaves beside it removed (never the lock file).
func TestInstallMigratesFromACopy(t *testing.T) {
	var events []string
	var unit string
	init := &fakeInit{events: &events, present: true, activeUnits: map[string]bool{DefaultUnitName: true}}
	in := packagedInstaller(&events, init, copyInstall(), &unit)
	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantOrder(t, events,
		evLockCp,
		"write "+pkgUnit,
		evStopPath, "remove /etc/systemd/system/remote-mic-update.path", "remove /etc/systemd/system/remote-mic-update.service",
		evReload, evEnableNowApp, "restart remote-mic.service",
		"remove "+copyBin, "remove "+copyBin+".prev", "remove "+copyBin+".new", "remove "+copyBin+".pending",
		"syncdir /usr/local/bin",
	)
	if slices.Contains(events, "remove "+copyBin+".lock") {
		t.Errorf("events %v: the lock file must stay", events)
	}
	if !strings.Contains(unit, "ExecStart="+pkgBin+" serve ") {
		t.Errorf("unit %q, want it to run the package binary", unit)
	}
}

// TestInstallMigrationDoesNotRestartWhatIsNotRunning asserts that a stopped
// appliance, and an install with --no-start, are not restarted.
func TestInstallMigrationDoesNotRestartWhatIsNotRunning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
		now    bool
	}{{"stopped", false, true}, {"no-start", true, false}} {
		var events []string
		init := &fakeInit{events: &events, present: true, activeUnits: map[string]bool{DefaultUnitName: tc.active}}
		in := packagedInstaller(&events, init, copyInstall(), nil)
		if err := in.Install(tc.now); err != nil {
			t.Fatalf("%s: Install: %v", tc.name, err)
		}
		if slices.Contains(events, "restart remote-mic.service") {
			t.Errorf("%s: events %v, want no restart", tc.name, events)
		}
		if !slices.Contains(events, "remove "+copyBin) {
			t.Errorf("%s: events %v, want the old copy removed", tc.name, events)
		}
	}
}

// TestInstallMigrationRefusesWhileAnUpdaterRuns asserts nothing is written
// while the root updater is installing the copy.
func TestInstallMigrationRefusesWhileAnUpdaterRuns(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true, activeUnits: map[string]bool{UpdateServiceUnit: true}}
	in := packagedInstaller(&events, init, copyInstall(), nil)
	err := in.Install(true)
	if err == nil || !strings.Contains(err.Error(), "is being installed; try again") {
		t.Fatalf("Install = %v, want a refusal while the updater runs", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "write ") || strings.HasPrefix(e, "remove ") {
			t.Errorf("event %q after a refusal", e)
		}
	}
}

// TestInstallMigrationRefusesANewerCopy asserts the downgrade guard applies to
// the copy being migrated away from, with advice that fits, and that
// --allow-downgrade overrides it.
func TestInstallMigrationRefusesANewerCopy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		var events []string
		init := &fakeInit{events: &events, present: true}
		in := packagedInstaller(&events, init, copyInstall(), nil)
		in.Version, in.AllowDowngrade = "v0.4.0", allow
		in.binVersion = func(p string) (string, bool, error) {
			if p != copyBin {
				t.Errorf("version read from %s, want %s", p, copyBin)
			}
			return "v0.5.0", true, nil
		}
		err := in.Install(true)
		if allow {
			if err != nil {
				t.Errorf("allow-downgrade: Install = %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "install the newer .deb first") {
			t.Fatalf("Install = %v, want the downgrade refusal with .deb advice", err)
		}
		if slices.Contains(events, "write "+pkgUnit) {
			t.Errorf("events %v: the unit was written after a refusal", events)
		}
	}
}

// TestInstallMigrationLeavesWhatIsNotACopy asserts that nothing is removed
// when the installed unit names the same binary or another package-owned one,
// when the unit is not one the installer wrote, and that a link or other
// non-regular file at the old path is left alone.
func TestInstallMigrationLeavesWhatIsNotACopy(t *testing.T) {
	same := copyInstall()
	same.BinPath = pkgBin
	for _, tc := range []struct {
		name string
		prev *ServiceSpec
		err  error
		link bool
	}{
		{"same binary", same, nil, false},
		{"unit not the installer's", nil, errors.New("hand edited"), false},
		{"link at the old path", copyInstall(), nil, true},
	} {
		var events []string
		init := &fakeInit{events: &events, present: true}
		in := packagedInstaller(&events, init, tc.prev, nil)
		in.installed = func() (ServiceSpec, error) {
			if tc.prev == nil {
				return ServiceSpec{}, tc.err
			}
			return *tc.prev, nil
		}
		in.isRegular = func(string) bool { return !tc.link }
		if err := in.Install(true); err != nil {
			t.Fatalf("%s: Install: %v", tc.name, err)
		}
		for _, e := range events {
			if strings.HasPrefix(e, "remove /usr/local/bin") {
				t.Errorf("%s: event %q, want the old path left alone", tc.name, e)
			}
		}
	}
}

// TestInstallCopyOfThePackagedBinaryWarns asserts that an install to another
// path from the packaged binary still copies it, saying apt will not update
// that copy.
func TestInstallCopyOfThePackagedBinaryWarns(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.selfExe = func() (string, error) { return pkgBin, nil }
	in.packageOwns = func(p string) bool { return p == pkgBin }
	var warn bytes.Buffer
	in.warn = &warn
	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !slices.Contains(events, "copy "+pkgBin+" -> "+copyBin) || !strings.Contains(warn.String(), "installing a copy of the packaged "+pkgBin) {
		t.Errorf("events %v warning %q, want the copy made with a warning", events, warn.String())
	}
	warn.Reset()
	in.selfExe = func() (string, error) { return handRun, nil }
	if err := in.Install(true); err != nil || warn.Len() != 0 {
		t.Errorf("other binary: err %v warning %q, want no warning", err, warn.String())
	}
}

// TestUninstallPurgeKeepsAPackageBinary asserts that purge leaves a binary
// the package owns to dpkg, and still removes the rest.
func TestUninstallPurgeKeepsAPackageBinary(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	un := testUninstaller(&events, init, true)
	un.Spec = ServiceSpec{BinPath: pkgBin}
	un.packageOwns = func(p string) bool { return p == pkgBin }
	if err := un.Uninstall(true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "rm /usr/bin") {
			t.Errorf("event %q, want the packaged binary left to dpkg", e)
		}
	}
	if !slices.Contains(events, "rmall /etc/remote-mic") || !slices.Contains(events, "rmall /var/lib/remote-mic") {
		t.Errorf("events %v, want the config and state removed", events)
	}
}

// TestInstallPackagedFailures asserts every failing step of a packaged
// migration stops the install with its own error, and that a step that only
// warns does not.
func TestInstallPackagedFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		break_  func(in *Installer, init *fakeInit)
		wantErr string
	}{
		{"locate self", func(in *Installer, _ *fakeInit) { in.selfExe = func() (string, error) { return "", boom } }, "locate the running binary"},
		{"bin dir", func(in *Installer, _ *fakeInit) { in.binDirOK = func(string) error { return boom } }, "refusing to install"},
		{"config dir over bin dir", func(in *Installer, _ *fakeInit) { in.dirsOK = func(ServiceSpec) error { return boom } }, "refusing to install"},
		{"lock busy", func(in *Installer, _ *fakeInit) {
			in.lockBin = func(string, func()) (func(), error) { return nil, update.ErrBinBusy }
		}, "try again in a few minutes"},
		{"lock", func(in *Installer, _ *fakeInit) {
			in.lockBin = func(string, func()) (func(), error) { return nil, boom }
		}, "lock " + copyBin},
		{"user lookup", func(in *Installer, _ *fakeInit) { in.lookupUser = func(string) (int, int, error) { return 0, 0, boom } }, "resolve user"},
		{"write unit", func(in *Installer, _ *fakeInit) {
			in.writeFile = func(string, []byte, os.FileMode) error { return boom }
		}, "write unit"},
		{"create dir", func(in *Installer, _ *fakeInit) { in.ensureDir = func(string, os.FileMode) error { return boom } }, "create /etc/remote-mic"},
		{"chown", func(in *Installer, _ *fakeInit) { in.chownTree = func(string, int, int) error { return boom } }, "chown /etc/remote-mic"},
		{"remove updater units", func(in *Installer, _ *fakeInit) { in.removeFile = func(string) error { return boom } }, "remove unit"},
		{"reload", func(_ *Installer, init *fakeInit) { init.reloadErr = boom }, "daemon-reload"},
		{"enable", func(_ *Installer, init *fakeInit) { init.enableErr = boom }, "enable remote-mic.service"},
		{"restart", func(_ *Installer, init *fakeInit) { init.restartErr = boom }, "restart remote-mic.service onto " + pkgBin},
	} {
		var events []string
		init := &fakeInit{events: &events, present: true, activeUnits: map[string]bool{DefaultUnitName: true}}
		in := packagedInstaller(&events, init, copyInstall(), nil)
		tc.break_(in, init)
		err := in.Install(true)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: Install = %v, want an error containing %q", tc.name, err, tc.wantErr)
		}
		if tc.name == "restart" && slices.Contains(events, "remove "+copyBin) {
			t.Errorf("restart: events %v, want the old copy kept when the restart failed", events)
		}
	}
}

// TestInstallPackagedRefusesWhatItCannotSee asserts that an unreadable
// systemd state, and a drop-in that overrides ExecStart=, stop the install
// before anything is written, instead of being read as "not running".
func TestInstallPackagedRefusesWhatItCannotSee(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		break_  func(in *Installer, init *fakeInit)
		wantErr string
	}{
		{"updater state", func(_ *Installer, init *fakeInit) { init.activeErr = map[string]error{UpdateServiceUnit: boom} }, "whether an update is running"},
		{"appliance state", func(_ *Installer, init *fakeInit) { init.activeErr = map[string]error{DefaultUnitName: boom} }, "whether remote-mic.service is running"},
		{"drop-in overrides ExecStart", func(in *Installer, _ *fakeInit) {
			in.execStartDropIn = func() (string, error) { return "/etc/systemd/system/remote-mic.service.d/x.conf", nil }
		}, "x.conf sets ExecStart="},
		{"unit cannot be read", func(in *Installer, _ *fakeInit) { in.execStartDropIn = func() (string, error) { return "", boom } }, "cannot tell what remote-mic.service runs"},
	} {
		var events []string
		init := &fakeInit{events: &events, present: true}
		in := packagedInstaller(&events, init, copyInstall(), nil)
		tc.break_(in, init)
		err := in.Install(true)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: Install = %v, want an error containing %q", tc.name, err, tc.wantErr)
		}
		for _, e := range events {
			if strings.HasPrefix(e, "write ") || strings.HasPrefix(e, "remove ") || strings.HasPrefix(e, "enable") {
				t.Errorf("%s: event %q after a refusal", tc.name, e)
			}
		}
	}
}

// TestInstallPackagedWarnsAndGoesOn asserts that a staging directory or an old
// copy that cannot be removed only warns.
func TestInstallPackagedWarnsAndGoesOn(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	in := packagedInstaller(&events, init, copyInstall(), nil)
	in.removeStaging = func(string) error { return errors.New("busy") }
	removes := 0
	in.removeFile = func(p string) error {
		if strings.HasPrefix(p, copyBin) {
			removes++
			return errors.New("read-only")
		}
		return nil
	}
	var warn bytes.Buffer
	in.warn = &warn
	if err := in.Install(true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if removes != 4 || !strings.Contains(warn.String(), "cannot remove the update staging directory") || !strings.Contains(warn.String(), "cannot remove "+copyBin) {
		t.Errorf("removes %d warning %q, want every removal tried with a warning each", removes, warn.String())
	}
}

// TestInstallPackagedWaitsForABusyLock asserts the wait for an update on the
// copy is announced.
func TestInstallPackagedWaitsForABusyLock(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	in := packagedInstaller(&events, init, copyInstall(), nil)
	in.lockBin = func(_ string, waiting func()) (func(), error) { waiting(); return func() {}, nil }
	var warn bytes.Buffer
	in.warn = &warn
	if err := in.Install(true); err != nil || !strings.Contains(warn.String(), "waiting for an update of "+copyBin) {
		t.Errorf("Install = %v, warning %q, want the wait announced", err, warn.String())
	}
}

// TestInstallCopyFailsWhenTheDirsCannotBeHandedOver covers the copy path's use
// of the shared handover.
func TestInstallCopyFailsWhenTheDirsCannotBeHandedOver(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	userThere := true
	in := testInstaller(&events, init, &userThere)
	in.ensureDir = func(string, os.FileMode) error { return errors.New("boom") }
	if err := in.Install(true); err == nil || !strings.Contains(err.Error(), "create /etc/remote-mic") {
		t.Errorf("Install = %v, want the handover error", err)
	}
}

func TestIsRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	link := filepath.Join(dir, "l")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(file) || isRegularFile(link) || isRegularFile(dir) || isRegularFile(filepath.Join(dir, "none")) {
		t.Error("isRegularFile must accept only a regular file, not a link, a directory or a missing path")
	}
}

// TestRemoveStagingDir asserts the staging directory is removed with what is
// in it, that a link at its name is removed itself and not followed, and that
// a missing state directory is not an error.
func TestRemoveStagingDir(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	staging := filepath.Join(state, UpdateDirName)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "request.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStagingDir(state); err != nil {
		t.Fatalf("removeStagingDir: %v", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staging directory still there: %v", err)
	}

	elsewhere := t.TempDir()
	keep := filepath.Join(elsewhere, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, staging); err != nil {
		t.Fatal(err)
	}
	if err := removeStagingDir(state); err != nil {
		t.Fatalf("removeStagingDir over a link: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the link was followed: %v", err)
	}
	if err := removeStagingDir(filepath.Join(state, "missing")); err != nil {
		t.Errorf("missing state directory: %v", err)
	}
}

func TestNewInstallerAndUninstallerWirePackageOwnership(t *testing.T) {
	t.Parallel()
	in := NewInstaller(ServiceSpec{})
	if in.packageOwns == nil || in.installed == nil || in.removeStaging == nil || in.isRegular == nil {
		t.Error("NewInstaller left a packaged-install seam unset; Install would panic")
	}
	if NewUninstaller(ServiceSpec{}).packageOwns == nil {
		t.Error("NewUninstaller left packageOwns unset; purge would panic")
	}
}

// TestUninstallPurgeFailsWhenTheBinaryCannotBeRemoved covers the wrapped error
// of the binary removal that is now conditional on package ownership.
func TestUninstallPurgeFailsWhenTheBinaryCannotBeRemoved(t *testing.T) {
	var events []string
	init := &fakeInit{events: &events, present: true}
	un := testUninstaller(&events, init, true)
	un.removeFile = func(p string) error {
		if p == DefaultBinPath {
			return errors.New("busy")
		}
		return nil
	}
	if err := un.Uninstall(true); err == nil || !strings.Contains(err.Error(), "remove binary "+DefaultBinPath) {
		t.Errorf("Uninstall = %v, want the binary removal error", err)
	}
}
