package update

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestDetectInstall(t *testing.T) {
	t.Parallel()
	dpkg := func(p string) bool { return p == debBin }
	trusted := func(string) error { return nil }
	loose := func(string) error { return errors.New("/usr/local/bin is writable by its group") }
	tests := []struct {
		name       string
		env        InstallEnv
		method     Method
		canApply   bool
		hint       string
		noHint     string // must not appear in the hint
		serviceBin string
	}{
		{
			name:       "service with updater",
			env:        InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, UpdaterBinPath: serviceBin, DpkgOwns: dpkg, RootOnly: trusted},
			method:     MethodService,
			canApply:   true,
			serviceBin: serviceBin,
		},
		{
			name:       "service with updater, binary others can change",
			env:        InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, UpdaterBinPath: serviceBin, RootOnly: loose},
			method:     MethodService,
			hint:       "writable only by root (/usr/local/bin is writable by its group)",
			noHint:     "sudo " + serviceBin,
			serviceBin: serviceBin,
		},
		{
			name:       "service with updater, permissions not checked",
			env:        InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, UpdaterBinPath: serviceBin},
			method:     MethodService,
			hint:       "writable only by root",
			noHint:     "sudo " + serviceBin,
			serviceBin: serviceBin,
		},
		{
			name:       "service without updater",
			env:        InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, DpkgOwns: dpkg, RootOnly: trusted},
			method:     MethodService,
			hint:       "sudo " + serviceBin + " service install",
			serviceBin: serviceBin,
		},
		{
			name:       "service without updater, binary others can change",
			env:        InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, RootOnly: loose},
			method:     MethodService,
			hint:       "from the release binary you installed from, by its full path",
			noHint:     "sudo " + serviceBin,
			serviceBin: serviceBin,
		},
		{
			name:       "service without updater, path with a space",
			env:        InstallEnv{Exe: spacedBin, ServiceBinPath: spacedBin, RootOnly: trusted},
			method:     MethodService,
			hint:       "sudo '/opt/Remote Mic/remote-mic' service install",
			serviceBin: spacedBin,
		},
		{
			name:       "updater installs another binary",
			env:        InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, UpdaterBinPath: "/opt/remote-mic", RootOnly: trusted},
			method:     MethodService,
			hint:       "sudo " + serviceBin + " service install",
			serviceBin: serviceBin,
		},
		{
			name:   "deb, even when a unit runs it",
			env:    InstallEnv{Exe: debBin, ServiceBinPath: debBin, UpdaterBinPath: debBin, DpkgOwns: dpkg, RootOnly: trusted},
			method: MethodDeb,
			hint:   "apt install",
		},
		{
			name:   "homebrew keg",
			env:    InstallEnv{Exe: "/home/linuxbrew/.linuxbrew/Cellar/remote-mic/0.2.0/bin/remote-mic"},
			method: MethodHomebrew,
			hint:   "brew upgrade",
		},
		{
			name:   "unpacked tarball beside a service that runs another binary",
			env:    InstallEnv{Exe: "/home/pi/remote-mic", ServiceBinPath: serviceBin, RootOnly: trusted},
			method: MethodManual,
			hint:   "not the binary the service runs (" + serviceBin + ")",
		},
		{
			name:   "run by hand with no service",
			env:    InstallEnv{Exe: "/home/pi/remote-mic"},
			method: MethodManual,
			hint:   "sudo /home/pi/remote-mic service install",
		},
	}
	for _, tt := range tests {
		got := DetectInstall(tt.env)
		if got.Method != tt.method || got.CanApply != tt.canApply || !strings.Contains(got.Hint, tt.hint) {
			t.Errorf("%s: got %+v, want %s canApply=%t hint containing %q", tt.name, got, tt.method, tt.canApply, tt.hint)
		}
		if tt.noHint != "" && strings.Contains(got.Hint, tt.noHint) {
			t.Errorf("%s: hint %q, want it without %q", tt.name, got.Hint, tt.noHint)
		}
		if got.ServiceBin != tt.serviceBin {
			t.Errorf("%s: ServiceBin = %q, want %q", tt.name, got.ServiceBin, tt.serviceBin)
		}
		if got.CanApply && got.Hint != "" {
			t.Errorf("%s: hint %q on an installation that can update itself", tt.name, got.Hint)
		}
	}
}

// TestRerunInstall pins the three kinds of advice: the service binary by its
// quoted path when root may run it, the release binary when the service
// binary is unknown, and permissions first, with no command to run, when
// others could change it.
func TestRerunInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     Install
		want   string
		noWant string
	}{
		{"unknown binary", Install{}, "re-run service install from the release binary you installed from", "sudo "},
		{"trusted binary", Install{ServiceBin: spacedBin}, "re-run sudo '" + spacedBin + "' service install", ""},
		{"binary others can change", Install{ServiceBin: serviceBin, ServiceBinUnsafe: "it is writable by its group"}, "make " + serviceBin + " and the directories above it writable only by root (it is writable by its group), then re-run service install from the release binary", "sudo " + serviceBin},
	}
	for _, tt := range tests {
		got := tt.in.RerunInstall()
		if !strings.Contains(got, tt.want) {
			t.Errorf("%s: RerunInstall() = %q, want it to contain %q", tt.name, got, tt.want)
		}
		if tt.noWant != "" && strings.Contains(got, tt.noWant) {
			t.Errorf("%s: RerunInstall() = %q, want it without %q", tt.name, got, tt.noWant)
		}
	}
}

func TestRunningTarget(t *testing.T) {
	t.Parallel()
	got := RunningTarget()
	if !strings.HasPrefix(got, runtime.GOOS+"/") {
		t.Errorf("RunningTarget() = %q", got)
	}
	if runtime.GOARCH != "arm" && got != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("RunningTarget() = %q, want %s/%s", got, runtime.GOOS, runtime.GOARCH)
	}
}

func TestManualHintQuotesThePath(t *testing.T) {
	t.Parallel()
	got := DetectInstall(InstallEnv{Exe: "/home/pi/Remote Mic/remote-mic"}).Hint
	if !strings.Contains(got, "sudo '/home/pi/Remote Mic/remote-mic' service install") {
		t.Errorf("hint %q, want the path shell-quoted", got)
	}
}
