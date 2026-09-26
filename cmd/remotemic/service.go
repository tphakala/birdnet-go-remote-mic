//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	installService = func(spec service.ServiceSpec, start bool) error {
		return service.NewInstaller(spec).Install(start)
	}
	uninstallService = func(spec service.ServiceSpec, purge bool) error {
		return service.NewUninstaller(spec).Uninstall(purge)
	}
	statusService = runServiceStatusDefault
)

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
// without a sudo prompt.
func runServiceInstall(args []string, escalated bool, stderr io.Writer) error {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic service install [flags]\n\n"+
			"Create a system user, install and enable a systemd unit, and start the\n"+
			"appliance. Re-runs itself under sudo when not already root.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	user := fs.String("user", service.DefaultUser, "system user to create and run the service as")
	cfg := fs.String("config", service.DefaultConfigPath, "config path baked into the unit")
	stateDir := fs.String("state-dir", service.DefaultStateDir, "state directory for the management certificate")
	binPath := fs.String("bin-path", service.DefaultBinPath, "path to install the binary to")
	noStart := fs.Bool("no-start", false, "enable at boot but do not start the service now")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if err := ensureRoot(escalated); err != nil {
		return err
	}
	spec := service.ServiceSpec{User: *user, ConfigPath: *cfg, StateDir: *stateDir, BinPath: *binPath}
	if err := installService(spec, !*noStart); err != nil {
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
	user := fs.String("user", service.DefaultUser, "service user to remove with --purge")
	cfg := fs.String("config", service.DefaultConfigPath, "config path whose directory --purge removes")
	stateDir := fs.String("state-dir", service.DefaultStateDir, "state directory --purge removes")
	binPath := fs.String("bin-path", service.DefaultBinPath, "installed binary path --purge removes")
	purge := fs.Bool("purge", false, "also remove the config, state, binary, and service user")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if err := ensureRoot(escalated); err != nil {
		return err
	}
	spec := service.ServiceSpec{User: *user, ConfigPath: *cfg, StateDir: *stateDir, BinPath: *binPath}
	if err := uninstallService(spec, *purge); err != nil {
		return err
	}
	out(stderr, "removed remote-mic.service\n")
	return nil
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
