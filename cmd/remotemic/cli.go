//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	capture "github.com/tphakala/go-audio-capture"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// serveFn and listDevicesFn are the serve and devices list entry points behind a
// seam so dispatch routing is testable without starting the appliance or
// touching audio hardware.
var (
	serveFn       = runServe
	listDevicesFn = runListDevices
)

// captureDevices enumerates the host's capture devices. It is a package var so
// reportCheck (serve --check) and devices list are testable without ALSA
// hardware: a test can inject a known device list or a probe failure.
var captureDevices = capture.Devices

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
		out(stdout, "remotemic %s\n", version)
		return 0
	case "help":
		usage(stdout)
		return 0
	case "devices":
		return runDevices(args[1:], stdout, stderr)
	case "token":
		return runToken(args[1:], stdout, stderr)
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
				out(stdout, "remotemic %s\n", version)
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

// toExit maps a subcommand's error to an exit code, printing it to stderr. A
// flag.ErrHelp (a -h request) is not an error: the FlagSet already printed its
// usage, so exit 0.
func toExit(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		out(stderr, "remotemic: %v\n", err)
		return 1
	}
}

// usage prints the top-level command summary.
func usage(w io.Writer) {
	out(w, `remotemic - remote microphone appliance for BirdNET-Go

Usage:
  remotemic [serve] [flags]     capture and serve (the default)
  remotemic token <command>     manage the shared access token (get, generate, set, clear)
  remotemic devices <command>   inspect capture devices (list)
  remotemic version             print version and exit

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
		out(stderr, "Usage: remotemic [serve] [flags]\n\n"+
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
	if perr := fs.Parse(args); perr != nil {
		return "", serveOverrides{}, false, perr
	}
	if fs.NArg() > 0 {
		return "", serveOverrides{}, false, fmt.Errorf("unexpected argument(s): %s", strings.Join(fs.Args(), " "))
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
			out(stderr, "Usage: remotemic devices list\n\n"+
				"List the host's capture devices: the id to put in a device's config\n"+
				"entry, and its label.\n")
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
  remotemic devices list   list capture devices (id and label)
`)
}

// runListDevices prints the id and label of every capture device on the host.
func runListDevices(w io.Writer) error {
	devs, err := captureDevices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		out(w, "%-12s %s\n", d.ID, d.Name)
	}
	return nil
}

// reportCheck validates cfg and reports each configured device's presence on the
// host, writing a summary to w. It returns the validation error for an invalid
// config (so `serve --check` exits nonzero, like `nginx -t`), but a device that
// is not currently present is only noted, not fatal, because the appliance
// tolerates a missing device by skipping it.
func reportCheck(cfg *config.Config, w io.Writer) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	out(w, "config OK: %d device(s), RTSP %s\n", len(cfg.Devices), cfg.Listen)
	present := make(map[string]bool)
	probed := true
	if devs, derr := captureDevices(); derr == nil {
		for _, d := range devs {
			present[d.ID] = true
		}
	} else {
		// Enumeration failed wholesale; without it every device would print
		// "not found", which would misrepresent present hardware. Say so instead.
		probed = false
		out(w, "  (device probe unavailable: %v)\n", derr)
	}
	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		status := "unknown"
		if probed {
			status = "present"
			if !present[d.Device] {
				status = "not found"
			}
		}
		out(w, "  %-20s %-10s %s\n", d.Name, d.Device, status)
	}
	return nil
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
