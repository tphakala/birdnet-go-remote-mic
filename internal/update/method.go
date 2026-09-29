package update

import (
	"path/filepath"
	"strings"
)

// Method is how the running binary was installed, which decides whether the
// appliance may replace it.
type Method string

const (
	// MethodService is a `remote-mic service install` install of a copy the
	// installer made (from a tarball, or from a package binary given another
	// bin path): the binary is the one the service unit runs. With the root
	// updater units in place it gets the one-button update.
	MethodService Method = "service"
	// MethodDeb is the .deb package; dpkg owns the binary, also when the
	// service unit runs it in place (`service install` from a package
	// binary), and apt is what updates it.
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
	// ServiceBin is the binary the installed service unit runs, set only for
	// MethodService; advice to re-run `service install` names it.
	ServiceBin string
	// ServiceBinUnsafe says why root must not run ServiceBin (someone other
	// than root could change it), or is "" when it may.
	ServiceBinUnsafe string
}

// RestartService is the advice to restart the appliance after re-running
// install, for the hints that follow RerunInstall with it.
const RestartService = "restart the service (sudo systemctl restart remote-mic)"

// RerunInstall is the advice to re-run `service install`, as a lowercase
// clause to follow a colon or semicolon. It names the service's own binary
// when that is known and only root can change it: a bare command name could
// resolve to a package copy earlier on PATH, and root must not run a file
// others can write. Otherwise it points at the release binary the operator
// installed from.
func (in Install) RerunInstall() string {
	switch {
	case in.ServiceBin == "":
		return "re-run service install from the release binary you installed from"
	case in.ServiceBinUnsafe != "":
		return "make " + in.ServiceBin + " and the directories above it writable only by root (" + in.ServiceBinUnsafe + "), then re-run service install from the release binary you installed from, by its full path"
	default:
		return "re-run sudo " + ShellQuote(in.ServiceBin) + " service install"
	}
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
	// RootOnly refuses a service binary anyone but root could change
	// (CheckRootOnlyFile). Nil refuses every binary.
	RootOnly func(path string) error
}

// DetectInstall classifies the running installation. What decides the button
// is not which artifact the binary came from but whether it is the binary the
// installed service unit runs, with the root updater installing to that same
// path: a copy that `sudo remote-mic service install` made gets it. A package
// manager's own copy is never replaced behind its back (dpkg and Homebrew get
// the upgrade command, also when the service runs that copy in place), and neither is a binary
// run by hand. The root updater refuses to replace a service binary anyone
// but root could change, so such a binary gets no button either. A hint that
// tells the operator to run `service install` names the service binary's own
// path, not a bare command a package copy on PATH could answer, and only when
// root may run it (see Install.RerunInstall).
func DetectInstall(env InstallEnv) Install {
	exe := filepath.Clean(env.Exe)
	switch {
	case env.DpkgOwns != nil && env.DpkgOwns(exe):
		return Install{Method: MethodDeb, Hint: "Download the .deb for this system from the release page and install it with sudo apt install ./birdnet-go-remote-mic_*.deb"}
	case isHomebrew(exe):
		return Install{Method: MethodHomebrew, Hint: "Run brew upgrade birdnet-go-remote-mic"}
	case env.ServiceBinPath != "" && filepath.Clean(env.ServiceBinPath) == exe:
		inst := Install{Method: MethodService, ServiceBin: exe}
		if env.RootOnly == nil {
			inst.ServiceBinUnsafe = "its permissions were not checked"
		} else if err := env.RootOnly(exe); err != nil {
			inst.ServiceBinUnsafe = err.Error()
		}
		updater := env.UpdaterBinPath != "" && filepath.Clean(env.UpdaterBinPath) == exe
		inst.CanApply = updater && inst.ServiceBinUnsafe == ""
		if !inst.CanApply {
			inst.Hint = "To enable one-button updates, " + inst.RerunInstall() + ", and " + RestartService + ", or install the release by hand"
			if inst.ServiceBinUnsafe != "" {
				inst.Hint = "The root updater does not replace a binary others can change. " + inst.Hint
			}
		}
		return inst
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
		return "This binary is run by hand and is never replaced. Run sudo " + ShellQuote(exe) + " service install to install it as a service that updates with one button, or download the release for this system from the release page and replace " + exe
	}
	return "This is not the binary the service runs (" + filepath.Clean(serviceBin) + "), so it is never replaced. Download the release for this system from the release page and replace " + exe
}
