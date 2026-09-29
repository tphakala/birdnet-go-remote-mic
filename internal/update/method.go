package update

import (
	"path/filepath"
	"strings"
)

// Method is how the running binary was installed, which decides whether the
// appliance may replace it.
type Method string

const (
	// MethodService is a `remote-mic service install` install, from a
	// tarball, a .deb or Homebrew alike: the binary is the one the service
	// unit runs. With the root updater units in place it gets the one-button
	// update.
	MethodService Method = "service"
	// MethodDeb is the .deb package; dpkg owns the binary.
	MethodDeb Method = "deb"
	// MethodHomebrew is the Homebrew formula; brew owns the binary.
	MethodHomebrew Method = "homebrew"
	// MethodManual is anything else: a binary run by hand, or a copy that is
	// not the one the service unit runs. It is never replaced.
	MethodManual Method = "manual"
)

// Install describes the running installation.
type Install struct {
	Method Method
	// CanApply reports whether the one-button update is available: a service
	// install whose root updater is installed for this very binary.
	CanApply bool
	// Hint tells the operator how to update when CanApply is false.
	Hint string
}

// InstallEnv is what DetectInstall needs to know about the host.
type InstallEnv struct {
	// Exe is the running binary's resolved path.
	Exe string
	// ServiceBinPath is the binary the appliance's systemd unit runs, or ""
	// when no unit is installed.
	ServiceBinPath string
	// UpdaterBinPath is the binary the root updater unit installs to, or ""
	// when the updater units are not installed.
	UpdaterBinPath string
	// DpkgOwns reports whether dpkg lists path as installed by a package.
	DpkgOwns func(path string) bool
}

// DetectInstall classifies the running installation. What decides the button
// is not which artifact the binary came from but whether it is the binary the
// installed service unit runs, with the root updater installing to that same
// path: a tarball, .deb or Homebrew install that ran `sudo remote-mic service
// install` gets it. A package manager's own copy is never replaced behind its
// back (dpkg and Homebrew get the upgrade command), and neither is a binary
// run by hand.
func DetectInstall(env InstallEnv) Install {
	exe := filepath.Clean(env.Exe)
	switch {
	case env.DpkgOwns != nil && env.DpkgOwns(exe):
		return Install{Method: MethodDeb, Hint: "Download the .deb for this system from the release page and install it with sudo apt install ./birdnet-go-remote-mic_*.deb"}
	case isHomebrew(exe):
		return Install{Method: MethodHomebrew, Hint: "Run brew upgrade birdnet-go-remote-mic"}
	case env.ServiceBinPath != "" && filepath.Clean(env.ServiceBinPath) == exe:
		if env.UpdaterBinPath != "" && filepath.Clean(env.UpdaterBinPath) == exe {
			return Install{Method: MethodService, CanApply: true}
		}
		return Install{Method: MethodService, Hint: "Re-run sudo remote-mic service install and restart the service (sudo systemctl restart remote-mic) to enable one-button updates (if install warns that others can write the binary's directory, fix that first), or install the release by hand"}
	default:
		return Install{Method: MethodManual, Hint: manualHint(exe, env.ServiceBinPath)}
	}
}

// isHomebrew reports whether path lies in a Homebrew prefix: a keg under
// Cellar, or the standard Linux prefix.
func isHomebrew(path string) bool {
	return strings.Contains(path, "/Cellar/") || strings.HasPrefix(path, "/home/linuxbrew/.linuxbrew/")
}

// manualHint tells the operator how to update a binary that is not the
// service's: with no service path, `service install` is what turns on
// one-button updates; with a unit that runs another binary, this one is a
// stray copy. The caller passes "" only when it found no readable unit, and
// says so itself when the cause was an unreadable one (see detectInstall).
func manualHint(exe, serviceBin string) string {
	if serviceBin == "" {
		return "This binary is run by hand and is never replaced. Run sudo " + exe + " service install to install it as a service that updates with one button, or download the release for this system from the release page and replace " + exe
	}
	return "This is not the binary the service runs (" + filepath.Clean(serviceBin) + "), so it is never replaced. Download the release for this system from the release page and replace " + exe
}
