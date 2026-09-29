//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/tphakala/birdnet-go-remote-mic/internal/service"
)

// escalateGuard is a hidden argument appended when the command re-execs itself
// under sudo. It travels as an argument (not an environment variable) because
// sudo scrubs unknown environment variables by default, which would drop an env
// guard and loop the re-exec. Seeing it means "already escalated": if the
// process is still not root, refuse rather than escalate again.
const escalateGuard = "--internal-escalated"

// Privilege and re-exec seams, package vars so the escalation logic is testable
// without being root and without actually replacing the process. stdinIsTerminal
// is shared with the token commands (token.go).
var (
	geteuid      = os.Geteuid
	lookPath     = exec.LookPath
	osExecutable = os.Executable
	execSelf     = syscall.Exec //nolint:gosec // argv0 is our own binary via os.Executable, run under sudo
)

// Action seams so subcommand routing and flag parsing are testable without
// creating real users, writing units, or driving systemctl.
var (
	installService = func(spec service.ServiceSpec, start, allowDowngrade bool) error {
		return newInstaller(spec, allowDowngrade).Install(start)
	}
	uninstallService = func(spec service.ServiceSpec, purge bool) error {
		return service.NewUninstaller(spec).Uninstall(purge)
	}
	statusService = runServiceStatusDefault
)

// newInstaller builds the production installer for spec, told which version
// this binary is so it can refuse to replace a newer installed one unless
// allowDowngrade is set.
func newInstaller(spec service.ServiceSpec, allowDowngrade bool) *service.Installer {
	in := service.NewInstaller(spec)
	in.Version = version
	in.AllowDowngrade = allowDowngrade
	return in
}

// runService routes the service command group and returns the exit code.
func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		serviceUsage(stderr)
		return 2
	}
	escalated, rest := stripGuard(args)
	if len(rest) == 0 {
		// The input was only the escalation guard flag (e.g. a hand-run
		// `service --internal-escalated`); there is no subcommand to route.
		serviceUsage(stderr)
		return 2
	}
	switch rest[0] {
	case "install":
		return toExit(runServiceInstall(rest[1:], escalated, stderr), stderr)
	case "uninstall":
		return toExit(runServiceUninstall(rest[1:], escalated, stderr), stderr)
	case "status":
		return toExit(runServiceStatus(rest[1:], stdout, stderr), stderr)
	case "apply-update":
		return toExit(runServiceApplyUpdate(rest[1:], stderr), stderr)
	}
	if isHelp(rest[0]) {
		serviceUsage(stdout)
		return 0
	}
	out(stderr, "unknown service command %q\n\n", rest[0])
	serviceUsage(stderr)
	return 2
}

// stripGuard removes the escalation guard flag from args (wherever it sits) and
// reports whether it was present, so it never reaches a subcommand FlagSet.
func stripGuard(args []string) (escalated bool, rest []string) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if a == escalateGuard {
			escalated = true
			continue
		}
		rest = append(rest, a)
	}
	return escalated, rest
}

// ensureRoot makes the current command run with privilege. When already root it
// returns nil. Otherwise it re-execs the whole command under sudo (which prompts
// for the password on the inherited terminal) and never returns on success. It
// refuses, with a clear message, when there is no terminal to prompt on, when
// sudo is unavailable, or when a prior escalation still did not yield root.
func ensureRoot(escalated bool) error {
	if geteuid() == 0 {
		return nil
	}
	if escalated {
		return errors.New("this command must run as root, but re-running under sudo did not grant it; re-run as root")
	}
	if !stdinIsTerminal() {
		return errors.New("this command must run as root; re-run it with sudo, for example sudo remote-mic service install")
	}
	sudo, err := lookPath("sudo")
	if err != nil {
		return errors.New("this command must run as root and sudo was not found; re-run as root")
	}
	self, err := osExecutable()
	if err != nil {
		return fmt.Errorf("locate own binary for sudo re-exec: %w", err)
	}
	argv := append([]string{sudo, self}, os.Args[1:]...)
	argv = append(argv, escalateGuard)
	// On success syscall.Exec replaces this process, so this does not return;
	// only a failed exec falls through, and it carries context.
	if err := execSelf(sudo, argv, os.Environ()); err != nil {
		return fmt.Errorf("re-exec under sudo: %w", err)
	}
	return nil
}

// runServiceInstall parses install flags, escalates to root, and installs the
// service. Flags are parsed before escalation so -h and a bad flag report
// without a sudo prompt. The install refuses to replace a newer installed
// binary unless --allow-downgrade is given.
func runServiceInstall(args []string, escalated bool, stderr io.Writer) error {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic service install [flags]\n\n"+
			"Create a system user, install and enable a systemd unit, and start the\n"+
			"appliance. Re-runs itself under sudo when not already root.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	user := fs.String("user", "", "system user to create and run the service as"+installedDefault(service.DefaultUser))
	cfg := fs.String("config", "", "config path baked into the unit"+installedDefault(service.DefaultConfigPath))
	stateDir := fs.String("state-dir", "", "state directory for the management certificate"+installedDefault(service.DefaultStateDir))
	binPath := fs.String("bin-path", "", "path to install the binary to"+installedDefault(service.DefaultBinPath))
	noStart := fs.Bool("no-start", false, "enable at boot but do not start the service now")
	allowDowngrade := fs.Bool("allow-downgrade", false, "replace an installed binary that is newer than this one")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if err := ensureRoot(escalated); err != nil {
		return err
	}
	spec, err := specFromFlags(service.ServiceSpec{User: *user, ConfigPath: *cfg, StateDir: *stateDir, BinPath: *binPath}, specInstall, stderr)
	if err != nil {
		return err
	}
	if err := installService(spec, !*noStart, *allowDowngrade); err != nil {
		return err
	}
	if *noStart {
		out(stderr, "installed and enabled remote-mic.service (not started; start with: systemctl start remote-mic)\n")
	} else {
		out(stderr, "installed, enabled, and started remote-mic.service\n")
	}
	return nil
}

// runServiceUninstall parses uninstall flags, escalates to root, and removes the
// service.
func runServiceUninstall(args []string, escalated bool, stderr io.Writer) error {
	fs := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic service uninstall [flags]\n\n"+
			"Stop, disable, and remove the systemd unit. Re-runs itself under sudo\n"+
			"when not already root. Config and certificates are kept unless --purge.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	user := fs.String("user", "", "service user to remove with --purge"+installedDefault(service.DefaultUser))
	cfg := fs.String("config", "", "config path whose directory --purge removes"+installedDefault(service.DefaultConfigPath))
	stateDir := fs.String("state-dir", "", "state directory --purge removes"+installedDefault(service.DefaultStateDir))
	binPath := fs.String("bin-path", "", "installed binary path --purge removes"+installedDefault(service.DefaultBinPath))
	purge := fs.Bool("purge", false, "also remove the config, state, binary, and service user")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if err := ensureRoot(escalated); err != nil {
		return err
	}
	mode := specDefaults
	if *purge {
		mode = specPurge
	}
	spec, err := specFromFlags(service.ServiceSpec{User: *user, ConfigPath: *cfg, StateDir: *stateDir, BinPath: *binPath}, mode, stderr)
	if err != nil {
		return err
	}
	if *purge {
		out(stderr, "purging config directory %s, state directory %s, binary %s, and user %s\n",
			spec.ConfigDir(), spec.StateDir, spec.BinPath, spec.User)
	}
	if err := uninstallService(spec, *purge); err != nil {
		return err
	}
	out(stderr, "removed remote-mic.service\n")
	return nil
}

// installedSpec reports the spec the installed unit was written from. A
// variable so tests never read the host's real units.
var installedSpec = service.InstalledSpec

// installedDefault is the flag help suffix for a service flag that defaults
// to the installed unit's value.
func installedDefault(def string) string {
	return " (default: the installed unit's, else " + def + ")"
}

// specMode says how specFromFlags fills unset service flags.
type specMode int

const (
	specDefaults specMode = iota // package defaults (plain uninstall)
	specInstall                  // the installed unit's values (install)
	specPurge                    // the installed unit's values only where they are the defaults
)

// specFromFlags completes s, the service flags as given, with defaults.
// Install takes each unset field from the installed unit, so a reinstall
// keeps a custom install's paths and account; each adopted value is
// reported. Purge adopts only values equal to the package defaults: a custom
// user may be an existing login account the install merely reused, and a
// custom directory may hold more than the install put there, so deleting
// them takes the flag given explicitly. The installed unit counts only when
// it is exactly what the installer wrote (service.InstalledSpec): a drop-in
// or a hand edit never chooses what purge deletes or install chowns, and a
// hand-edited unit makes the command ask for every flag. Without an
// installed unit, unset fields take the package defaults, and purge says so.
func specFromFlags(s service.ServiceSpec, mode specMode, stderr io.Writer) (service.ServiceSpec, error) {
	adopt := mode != specDefaults
	if adopt && (s.User == "" || s.ConfigPath == "" || s.StateDir == "" || s.BinPath == "") {
		inst, err := installedSpec()
		if err != nil {
			return s, fmt.Errorf("cannot take defaults from the installed %s (%w); pass --user, --config, --state-dir and --bin-path", service.DefaultUnitName, err)
		}
		if inst == (service.ServiceSpec{}) && mode == specPurge {
			out(stderr, "no installed %s; unset flags take the package defaults\n", service.DefaultUnitName)
		}
		if inst != (service.ServiceSpec{}) {
			var custom []string
			for _, f := range []struct {
				name, installed, def string
				dst                  *string
			}{
				{"user", inst.User, service.DefaultUser, &s.User},
				{"config", inst.ConfigPath, service.DefaultConfigPath, &s.ConfigPath},
				{"state-dir", inst.StateDir, service.DefaultStateDir, &s.StateDir},
				{"bin-path", inst.BinPath, service.DefaultBinPath, &s.BinPath},
			} {
				switch {
				case *f.dst != "":
				case mode == specPurge && f.installed != f.def:
					custom = append(custom, "--"+f.name+"="+f.installed)
				default:
					*f.dst = f.installed
					out(stderr, "using --%s=%s from the installed %s\n", f.name, f.installed, service.DefaultUnitName)
				}
			}
			if len(custom) > 0 {
				return s, fmt.Errorf("the installed %s uses %s; --purge deletes those only when they are given explicitly",
					service.DefaultUnitName, strings.Join(custom, " "))
			}
		}
	}
	for _, f := range []struct {
		dst *string
		def string
	}{
		{&s.User, service.DefaultUser},
		{&s.ConfigPath, service.DefaultConfigPath},
		{&s.StateDir, service.DefaultStateDir},
		{&s.BinPath, service.DefaultBinPath},
	} {
		if *f.dst == "" {
			*f.dst = f.def
		}
	}
	return s, nil
}

// runServiceStatus reports the unit's boot enablement and running state. It
// needs no privilege, so it never escalates.
func runServiceStatus(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("service status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic service status\n\n"+
			"Report whether the remote-mic systemd unit is enabled at boot and\n"+
			"currently running.\n")
	}
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	return statusService(stdout)
}

// runServiceStatusDefault is the production status action: it queries systemd
// for the default unit and prints a compact summary.
func runServiceStatusDefault(w io.Writer) error {
	sd := service.NewSystemd()
	out(w, "unit:    %s\n", service.DefaultUnitName)
	// Surface a genuine systemctl failure as "unknown" rather than folding it
	// into a definitive "false", which would be indistinguishable from an
	// installed-but-disabled or stopped unit.
	if enabled, err := sd.IsEnabled(service.DefaultUnitName); err != nil {
		out(w, "enabled: unknown (%v)\n", err)
	} else {
		out(w, "enabled: %t\n", enabled)
	}
	if active, err := sd.IsActive(service.DefaultUnitName); err != nil {
		out(w, "active:  unknown (%v)\n", err)
	} else {
		out(w, "active:  %t\n", active)
	}
	return nil
}

// serviceUsage prints the service command summary.
func serviceUsage(w io.Writer) {
	out(w, `Install and manage the systemd service.

Usage:
  remote-mic service install     install and enable the unit, run at boot
  remote-mic service uninstall   stop, disable, and remove the unit
  remote-mic service status      show whether the unit is enabled and active

install and uninstall re-run themselves under sudo when not already root.
`)
}
