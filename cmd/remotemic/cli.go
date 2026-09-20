//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// serveFn and listDevicesFn are the serve and devices list entry points behind a
// seam so dispatch routing is testable without starting the appliance or
// touching audio hardware.
var (
	serveFn       = runServe
	listDevicesFn = runListDevices
)

// captureDevices enumerates the host's capture devices and resolveDevice maps
// one configured id to the device it names right now. They are package vars so
// reportCheck (serve --check) and devices list are testable without ALSA
// hardware: a test can inject a known device list or a probe failure.
var (
	captureDevices = audio.Enumerate
	resolveDevice  = audio.Resolve
)

// configEnv names the environment variable that supplies the config path when
// --config is not given, so a service unit can set it once and every command
// run by hand on the appliance finds the same file.
const configEnv = "REMOTEMIC_CONFIG"

// configFlag registers the shared --config flag on fs. Its default is
// $REMOTEMIC_CONFIG when set and non-empty, else config.yaml in the working directory.
func configFlag(fs *flag.FlagSet) *string {
	def := os.Getenv(configEnv)
	if def == "" {
		def = "config.yaml"
	}
	return fs.String("config", def, "path to the YAML config file (env "+configEnv+")")
}

// out writes formatted CLI text to w, discarding the write error: output to
// stdout or stderr is best-effort, and a failed write there is unrecoverable and
// not worth surfacing.
func out(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// dispatch routes CLI arguments (os.Args[1:]) to a command and returns the
// process exit code. Commands are noun-verb groups (token get, devices list),
// plus the top-level serve and version. serve is the implicit default: a bare
// invocation, or one whose first argument is a flag, runs the appliance.
func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return toExit(serveFn(nil, stderr), stderr)
	}
	switch args[0] {
	case "version":
		out(stdout, "remote-mic %s\n", version)
		return 0
	case "devices":
		return runDevices(args[1:], stdout, stderr)
	case "token":
		return runToken(args[1:], stdout, stderr)
	case "service":
		return runService(args[1:], stdout, stderr)
	case "serve":
		return toExit(serveFn(args[1:], stderr), stderr)
	}
	if isHelp(args[0]) {
		usage(stdout)
		return 0
	}
	if strings.HasPrefix(args[0], "-") {
		// The version flag works in any position, so `--config x.yaml -v` prints
		// the version instead of starting the appliance.
		for _, a := range args {
			switch a {
			case "-v", "--version", "-version":
				out(stdout, "remote-mic %s\n", version)
				return 0
			}
		}
		// Bare flags with no subcommand: run the appliance (implicit serve).
		return toExit(serveFn(args, stderr), stderr)
	}
	out(stderr, "unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

// isHelp reports whether arg asks a command group for its usage.
func isHelp(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help"
}

// usageError marks a command-line usage problem (a bad flag or a stray
// argument) so toExit maps it to exit code 2, matching an unknown command,
// rather than the generic 1. printed is true when the flag package has already
// written the message and usage to stderr (a Parse failure), so toExit does not
// print it a second time; a usage error raised after a successful parse
// (printed false) is printed by toExit like any other error.
type usageError struct {
	err     error
	printed bool
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// parseFailed wraps an error from flag.FlagSet.Parse, which has already reported
// it to the command's stderr with usage, so toExit exits 2 without repeating it.
func parseFailed(err error) error { return &usageError{err: err, printed: true} }

// badUsage wraps an argument-validation error raised after a successful parse
// (the flag package has printed nothing), so toExit prints it and exits 2.
func badUsage(err error) error { return &usageError{err: err, printed: false} }

// toExit maps a subcommand's error to an exit code, printing it to stderr. A
// flag.ErrHelp (a -h request) is not an error: the FlagSet already printed its
// usage, so exit 0. A usageError is command-line misuse and exits 2, matching an
// unknown command; the flag package's own message is not repeated.
func toExit(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	}
	var ue *usageError
	if errors.As(err, &ue) {
		if !ue.printed {
			out(stderr, "remote-mic: %v\n", err)
		}
		return 2
	}
	out(stderr, "remote-mic: %v\n", err)
	return 1
}

// usage prints the top-level command summary.
func usage(w io.Writer) {
	out(w, `remote-mic - remote microphone appliance for BirdNET-Go

Usage:
  remote-mic [serve] [flags]     capture and serve (the default)
  remote-mic token <command>     manage the shared access token (get, generate, set, clear)
  remote-mic devices <command>   inspect capture devices (list)
  remote-mic service <command>   install and manage the systemd service (install, uninstall, status)
  remote-mic version             print version and exit

Commands that read the config take --config, which defaults to $`+configEnv+`
or config.yaml. Run a command with -h to see its flags.
`)
}

// runServe parses the serve flags and starts the appliance.
func runServe(args []string, stderr io.Writer) error {
	cfgPath, ov, check, err := parseServeFlags(args, stderr)
	if err != nil {
		return err
	}
	return run(cfgPath, ov, check)
}

// parseServeFlags parses the serve flags into the config path, the set of config
// overrides (only the flags actually passed, so precedence is flag > config >
// default via applyServeOverrides), and the --check switch. It is separated from
// runServe so the flag-name-to-override-key mapping is unit-testable without
// starting the appliance. Stray positional arguments are rejected rather than
// silently ignored.
func parseServeFlags(args []string, stderr io.Writer) (cfgPath string, ov serveOverrides, check bool, err error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic [serve] [flags]\n\n"+
			"Capture local audio and serve it over RTSP. Flags override the config\n"+
			"file for this run only and are never written back to it; use\n"+
			"--flag=false for the boolean toggles.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	path := configFlag(fs)
	listen := fs.String("listen", "", "RTSP listen address host:port (overrides config)")
	mgmtListen := fs.String("mgmt-listen", "", "HTTPS management listen address host:port (overrides config)")
	certDir := fs.String("cert-dir", "", "directory for the self-signed management certificate (overrides config)")
	management := fs.Bool("management", true, "serve the management API and web UI (use --management=false to disable)")
	discovery := fs.Bool("discovery", true, "advertise devices over mDNS (use --discovery=false to disable)")
	checkFlag := fs.Bool("check", false, "validate the config and configured devices, then exit without serving")
	if err := parseNoArgs(fs, args); err != nil {
		return "", serveOverrides{}, false, err
	}
	ov = serveOverrides{
		listen:     *listen,
		mgmtListen: *mgmtListen,
		certDir:    *certDir,
		management: *management,
		discovery:  *discovery,
		set:        make(map[string]bool),
	}
	fs.Visit(func(f *flag.Flag) { ov.set[f.Name] = true })
	return *path, ov, *checkFlag, nil
}

// runDevices routes the devices command group and returns the exit code.
func runDevices(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		devicesUsage(stderr)
		return 2
	}
	if args[0] == "list" {
		fs := flag.NewFlagSet("devices list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.Usage = func() {
			out(stderr, "Usage: remote-mic devices list\n\n"+
				"List the host's capture devices: the id to put in a device's config\n"+
				"entry, its current ALSA address, and its label. The id names the\n"+
				"physical device and survives reboots; the address does not.\n")
		}
		if err := parseNoArgs(fs, args[1:]); err != nil {
			return toExit(err, stderr)
		}
		return toExit(listDevicesFn(stdout), stderr)
	}
	if isHelp(args[0]) {
		devicesUsage(stdout)
		return 0
	}
	out(stderr, "unknown devices command %q\n\n", args[0])
	devicesUsage(stderr)
	return 2
}

// devicesUsage prints the devices command summary.
func devicesUsage(w io.Writer) {
	out(w, `Inspect the host's capture devices.

Usage:
  remote-mic devices list   list capture devices (id, address and label)
`)
}

// runListDevices prints the id, current address and label of every capture
// device on the host.
func runListDevices(w io.Writer) error {
	devs, err := captureDevices()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	out(tw, "ID\tALSA\tLABEL\n")
	for _, d := range devs {
		label := d.Label
		if !d.IDStable {
			label += " (no stable id; card index (can change after a reboot))"
		}
		out(tw, "%s\t%s\t%s\n", d.ID, d.HWAddr, label)
	}
	return tw.Flush()
}

// reportCheck validates cfg and reports which hardware each configured device
// id resolves to on the host, writing a summary to w. It returns the validation
// error for an invalid config (so `serve --check` exits nonzero, like
// `nginx -t`), but a device that is not currently present is only noted, not
// fatal, because the appliance tolerates a missing device by skipping it.
func reportCheck(cfg *config.Config, w io.Writer) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	out(w, "config OK: %d device(s), RTSP %s\n", len(cfg.Devices), cfg.Listen)
	if _, derr := captureDevices(); derr != nil {
		// The host enumeration failed wholesale (no readable device listing), so
		// per-device resolution would fail too and print "Cannot resolve" for
		// every entry, which tells the operator nothing about the hardware. Report
		// the probe failure once and mark every device unknown instead.
		out(w, "  (device probe unavailable: %v)\n", derr)
		for i := range cfg.Devices {
			out(w, "  %-20s %s  unknown\n", cfg.Devices[i].Name, cfg.Devices[i].Device)
		}
		return nil
	}
	// owner maps a resolved hardware address to the first ENABLED entry that
	// claims it, matching how the appliance opens enabled entries in config order
	// and refuses a later enabled entry naming the same device through another id.
	// A disabled entry is never opened, so it does not claim hardware and is not
	// reported as a duplicate (see checkStatus).
	owner := make(map[string]string, len(cfg.Devices))
	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		out(w, "  %-20s %s  %s\n", d.Name, d.Device, checkStatus(d, owner))
	}
	return nil
}

// checkStatus describes what one configured device id resolves to for
// reportCheck, recording a present device's address in owner.
func checkStatus(d *config.Device, owner map[string]string) string {
	hw, err := resolveDevice(d.Device)
	if err != nil {
		_, msg := resolveError(d, err)
		return msg
	}
	status := "present"
	if hw.HWAddr != "" {
		status += " at " + hw.HWAddr
	}
	if hw.Label != "" {
		status += " (" + hw.Label + ")"
	}
	// Only an enabled entry is opened, and the appliance refuses a later enabled
	// entry that resolves to the same hardware (see hardwareOwner). A disabled
	// entry is never opened, so it neither claims the hardware nor is reported as
	// a duplicate: print its resolution and leave ownership to the enabled entry.
	// An empty address names no card, so it cannot be compared for ownership.
	if d.IsEnabled() && hw.HWAddr != "" {
		if first, dup := owner[hw.HWAddr]; dup {
			return status + "; same hardware as " + strconv.Quote(first) + ", so it will not be opened"
		}
		owner[hw.HWAddr] = d.Name
	}
	if config.IsCardIndexID(d.Device) {
		status += "; card index (can change after a reboot)"
		// Suggest the resolved id only when it is a stable one: a host that offers
		// no stable form resolves the index back to the same card index.
		if hw.IDStable && hw.ID != d.Device {
			status += " (use id " + hw.ID + ")"
		} else if !hw.IDStable {
			status += " (this device reports no stable id)"
		}
	}
	return status
}

// serveOverrides carries the serve subcommand's config-overriding flag values
// plus the set of flags the operator actually passed. Only passed flags are
// applied, so an unset flag never clobbers a config value: precedence is
// flag > config file > built-in default. Overrides are ephemeral for the run:
// they shape the running pipeline and listeners but are never written back to
// config.yaml, and the reloader re-applies them on every hot reload so a later
// PATCH /config cannot persist them (issue #29).
type serveOverrides struct {
	listen     string
	mgmtListen string
	certDir    string
	management bool
	discovery  bool
	set        map[string]bool
}

// applyServeOverrides mutates cfg in place, applying only the flags present in
// ov.set. The paired boolean toggles (--management, --discovery) write through a
// fresh *bool so the config's tri-state (nil = default-on) becomes an explicit
// value only when the flag was given.
func applyServeOverrides(cfg *config.Config, ov serveOverrides) {
	if ov.set["listen"] {
		cfg.Listen = ov.listen
	}
	if ov.set["mgmt-listen"] {
		cfg.Management.Listen = ov.mgmtListen
	}
	if ov.set["cert-dir"] {
		cfg.Management.CertDir = ov.certDir
	}
	if ov.set["management"] {
		b := ov.management
		cfg.Management.Enabled = &b
	}
	if ov.set["discovery"] {
		b := ov.discovery
		cfg.Discovery.Enabled = &b
	}
}
