package update

import (
	"runtime"
	"strings"
	"testing"
)

func TestDetectInstall(t *testing.T) {
	t.Parallel()
	dpkg := func(p string) bool { return p == debBin }
	tests := []struct {
		name     string
		env      InstallEnv
		method   Method
		canApply bool
		hint     string
	}{
		{
			name:     "service with updater",
			env:      InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, UpdaterBinPath: serviceBin, DpkgOwns: dpkg},
			method:   MethodService,
			canApply: true,
		},
		{
			name:   "service without updater",
			env:    InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, DpkgOwns: dpkg},
			method: MethodService,
			hint:   "service install",
		},
		{
			name:   "updater installs another binary",
			env:    InstallEnv{Exe: serviceBin, ServiceBinPath: serviceBin, UpdaterBinPath: "/opt/remote-mic"},
			method: MethodService,
			hint:   "service install",
		},
		{
			name:   "deb, even when a unit runs it",
			env:    InstallEnv{Exe: debBin, ServiceBinPath: debBin, UpdaterBinPath: debBin, DpkgOwns: dpkg},
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
			name:   "unpacked tarball",
			env:    InstallEnv{Exe: "/home/pi/remote-mic", ServiceBinPath: serviceBin},
			method: MethodManual,
			hint:   "/home/pi/remote-mic",
		},
	}
	for _, tt := range tests {
		got := DetectInstall(tt.env)
		if got.Method != tt.method || got.CanApply != tt.canApply || !strings.Contains(got.Hint, tt.hint) {
			t.Errorf("%s: got %+v, want %s canApply=%t hint containing %q", tt.name, got, tt.method, tt.canApply, tt.hint)
		}
		if got.CanApply && got.Hint != "" {
			t.Errorf("%s: hint %q on an installation that can update itself", tt.name, got.Hint)
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
