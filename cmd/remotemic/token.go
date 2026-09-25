//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
)

// Terminal seams, so the interactive paths of token set and token clear are
// testable without a TTY.
var (
	stdin           io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }              //nolint:gosec // an fd fits in int
	readSecret                = func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) } //nolint:gosec // an fd fits in int
)

// liveTimeout bounds a token change sent to a running appliance. The API
// persists and enforces the token before it answers, then waits for the live
// reload, so allow for a slow reconcile rather than failing a change that landed.
const liveTimeout = 30 * time.Second

// maxTokenInput caps what token set reads from a non-terminal stdin. A valid
// token is at most 128 characters; the slack tolerates a trailing newline.
const maxTokenInput = 1024

// runToken routes the token command group and returns the exit code.
func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		tokenUsage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "get":
		err = runTokenGet(args[1:], stdout, stderr)
	case "generate":
		err = runTokenGenerate(args[1:], stdout, stderr)
	case "set":
		err = runTokenSet(args[1:], stderr)
	case "clear":
		err = runTokenClear(args[1:], stderr)
	default:
		if isHelp(args[0]) {
			tokenUsage(stdout)
			return 0
		}
		out(stderr, "unknown token command %q\n\n", args[0])
		tokenUsage(stderr)
		return 2
	}
	return toExit(err, stderr)
}

// tokenUsage prints the token command summary.
func tokenUsage(w io.Writer) {
	out(w, `Manage the shared access token that gates the web UI, the management API
(Bearer), and the RTSP stream (Digest password, any username).

Usage:
  remote-mic token get                 print the current token
  remote-mic token generate [--force]  create a random token (--force replaces one)
  remote-mic token set                 set a token read from stdin
  remote-mic token clear [--yes]       remove the token (open access)

When the appliance is running with its management API, generate, set, and clear
apply the change through it, so it takes effect immediately. Running without that
API, or with no appliance running, they edit the config file and it applies when
the appliance next starts. A command run while the appliance is still starting up
asks you to retry. Run a command with -h to see its flags.
`)
}

// newTokenFlags returns a FlagSet for a token command with the shared --config
// flag registered and a usage function printing synopsis and summary.
func newTokenFlags(name, synopsis, summary string, stderr io.Writer) (fs *flag.FlagSet, cfgPath *string) {
	fs = flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic %s\n\n%s\n\nFlags:\n", synopsis, summary)
		fs.PrintDefaults()
	}
	return fs, configFlag(fs)
}

// parseNoArgs parses args into fs and rejects stray positional arguments.
func parseNoArgs(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return parseFailed(err)
	}
	if fs.NArg() > 0 {
		return badUsage(fmt.Errorf("unexpected argument(s): %s", strings.Join(fs.Args(), " ")))
	}
	return nil
}

// runTokenGet prints the access token stored in the config file. The bare
// token goes to stdout so `TOKEN=$(remote-mic token get)` works. A missing
// config or an unset token is an error with no stdout, so a script never reads
// empty output as a token. The file is authoritative even while the appliance
// runs: the management API persists a token change before it enforces it.
func runTokenGet(args []string, stdout, stderr io.Writer) error {
	fs, cfgPath := newTokenFlags("token get", "token get [flags]",
		"Print the current access token.", stderr)
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no config file at %s; pass --config or set %s", *cfgPath, configEnv)
	}
	if err != nil {
		return withPermHint(err)
	}
	if cfg.Auth.Token == "" {
		return fmt.Errorf("no access token is set in %s (the appliance is open); create one with `remote-mic token generate`", *cfgPath)
	}
	out(stdout, "%s\n", cfg.Auth.Token)
	return nil
}

// runTokenGenerate creates a strong random token and stores it, refusing to
// replace an existing token without --force (a silent rotation locks out every
// connected client). The bare token goes to stdout; guidance goes to stderr and
// --quiet suppresses it.
func runTokenGenerate(args []string, stdout, stderr io.Writer) error {
	fs, cfgPath := newTokenFlags("token generate", "token generate [flags]",
		"Generate a random access token, store it, and print it.", stderr)
	force := fs.Bool("force", false, "replace an existing token")
	quiet := fs.Bool("quiet", false, "print only the token (no guidance on stderr)")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	token, err := auth.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	res, err := changeToken(*cfgPath, token, func(cur string) error {
		if cur != "" && !*force {
			return fmt.Errorf("an access token is already set in %s; show it with `remote-mic token get`, or re-run with --force to replace it", *cfgPath)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !*quiet {
		reportChange(stderr, res, "Access token saved to "+absPath(*cfgPath)+".")
		out(stderr, "Clients authenticate with it as a Bearer token (web UI, management API)\n"+
			"and as the Digest password with any username (RTSP stream):\n\n")
	}
	out(stdout, "%s\n", token)
	return nil
}

// runTokenSet stores a caller-chosen token read from stdin: a hidden,
// confirmed prompt on a terminal, otherwise the first line of piped input. It
// takes no positional token so the secret stays out of shell history and the
// process list.
func runTokenSet(args []string, stderr io.Writer) error {
	fs, cfgPath := newTokenFlags("token set", "token set [flags] < token-file",
		"Set the access token to a value read from stdin (prompted, hidden, on a\n"+
			"terminal). The token is 12-128 characters of letters, digits, and . _ ~ -", stderr)
	quiet := fs.Bool("quiet", false, "print nothing on success")
	if err := fs.Parse(args); err != nil {
		return parseFailed(err)
	}
	if fs.NArg() > 0 {
		return badUsage(errors.New("token set reads the token from stdin, not the command line (keeping it out of shell history); for example: remote-mic token set < token.txt"))
	}
	token, err := readNewToken(stderr)
	if err != nil {
		return err
	}
	if msg := auth.ValidToken(token); msg != "" {
		return fmt.Errorf("invalid token: %s", msg)
	}
	res, err := changeToken(*cfgPath, token, nil)
	if err != nil {
		return err
	}
	if !*quiet {
		reportChange(stderr, res, "Access token saved to "+absPath(*cfgPath)+".")
	}
	return nil
}

// readNewToken reads the token for token set.
func readNewToken(stderr io.Writer) (string, error) {
	if !stdinIsTerminal() {
		// Read only the first line so a token piped interactively
		// (ssh host remote-mic token set) returns as soon as Enter is pressed,
		// rather than blocking until the sender closes stdin (EOF). ReadString
		// stops at the newline, or at EOF for input with no trailing newline;
		// LimitReader caps a stream that carries neither.
		line, err := bufio.NewReader(io.LimitReader(stdin, maxTokenInput)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		token := strings.TrimSpace(line)
		if token == "" {
			return "", errors.New("no token on stdin")
		}
		return token, nil
	}
	out(stderr, "New access token: ")
	first, err := readSecret()
	out(stderr, "\n")
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	out(stderr, "Repeat access token: ")
	second, err := readSecret()
	out(stderr, "\n")
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	if !bytes.Equal(first, second) {
		return "", errors.New("the tokens do not match")
	}
	token := strings.TrimSpace(string(first))
	if token == "" {
		return "", errors.New("no token entered; to remove the token use `remote-mic token clear`")
	}
	return token, nil
}

// runTokenClear removes the token, opening the appliance to the network. It
// asks for confirmation on a terminal and requires --yes otherwise. Clearing
// when no token is set succeeds, so a provisioning script can run it blindly.
func runTokenClear(args []string, stderr io.Writer) error {
	fs, cfgPath := newTokenFlags("token clear", "token clear [flags]",
		"Remove the access token. The RTSP stream, management API, and web UI\n"+
			"become open to anyone on the network.", stderr)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	// Load and confirm before touching the run lock. The y/N prompt must not run
	// while this command holds the lock, or an appliance starting during the
	// prompt would wait out the lock and fail with a false "already running".
	cfg, err := config.LoadOrDefault(*cfgPath)
	if err != nil {
		return withPermHint(err)
	}
	if cfg.Auth.Token == "" {
		if !*quiet {
			out(stderr, "No access token is set in %s; the appliance is already open.\n", absPath(*cfgPath))
		}
		return nil
	}
	if err := confirmClear(*yes, stderr); err != nil {
		return err
	}

	var alreadyOpen bool
	res, err := changeToken(*cfgPath, "", func(cur string) error {
		if cur == "" {
			// Cleared between the load above and this change (the file edit runs
			// under the lock; a live change goes through the management API).
			alreadyOpen = true
			return errNoChange
		}
		return nil
	})
	switch {
	case alreadyOpen:
		if !*quiet {
			out(stderr, "No access token is set in %s; the appliance is already open.\n", absPath(*cfgPath))
		}
		return nil
	case err != nil:
		return err
	}
	if !*quiet {
		reportChange(stderr, res, "Access token removed from "+absPath(*cfgPath)+".")
		switch res.outcome {
		case changedLive, changedLiveRestart:
			out(stderr, "The RTSP stream, management API, and web UI are now open to anyone on the network.\n")
		default:
			out(stderr, "The RTSP stream, management API, and web UI will be open once the appliance (re)starts.\n")
		}
	}
	return nil
}

// confirmClear asks before opening the appliance, unless yes is set.
func confirmClear(yes bool, stderr io.Writer) error {
	if yes {
		return nil
	}
	if !stdinIsTerminal() {
		return errors.New("refusing to remove the access token without confirmation; re-run with --yes")
	}
	out(stderr, "Remove the access token and open the appliance to anyone on the network? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("aborted; the access token is unchanged")
}

// errNoChange lets a changeToken check stop without writing and without an
// error of its own; the caller records why.
var errNoChange = errors.New("no change")

// changeOutcome says where a token change landed.
type changeOutcome int

const (
	// changedFile: no appliance runs on this config; the file was edited.
	changedFile changeOutcome = iota
	// changedFileRestart: an appliance runs without its management API, so the
	// file was edited and the running process keeps its token until restart.
	changedFileRestart
	// changedLive: the running appliance persisted and applied the change.
	changedLive
	// changedLiveRestart: the running appliance persisted the change and
	// enforces the token, but reported that a restart is needed to finish.
	changedLiveRestart
)

// changeResult describes a completed token change for reportChange.
type changeResult struct {
	outcome changeOutcome
	cfgPath string
	pid     int
}

// changeToken sets the token in the config at cfgPath to token ("" clears it).
// check, when non-nil, sees the current token first and can veto the change.
//
// A running appliance holds the config in memory and rewrites the whole file on
// every web UI save, so editing the file under it would be ignored and later
// reverted. When the run lock shows an appliance with a live management API,
// the change goes through that API instead. With no appliance running, the lock
// is held across the load and save so one cannot start mid-edit.
func changeToken(cfgPath, token string, check func(cur string) error) (changeResult, error) {
	res := changeResult{cfgPath: cfgPath}
	lockPath := runlock.PathFor(cfgPath)
	lock, err := runlock.TryAcquire(lockPath)
	if err != nil && !errors.Is(err, runlock.ErrHeld) {
		return res, withPermHint(err)
	}
	if lock != nil {
		defer func() { _ = lock.Release() }()
		return res, saveToken(cfgPath, token, check)
	}

	st, ok, err := runlock.ReadState(lockPath)
	if err != nil {
		return res, withPermHint(err)
	}
	if !ok {
		return res, errors.New("the appliance is starting up; try again in a few seconds")
	}
	res.pid = st.PID
	if st.MgmtAddr == "" {
		// Without its management API the appliance has no config writer, so the
		// file edit is safe; it takes effect at the next start, or when the API
		// comes back through its background retry (see recoverManagement).
		res.outcome = changedFileRestart
		return res, saveToken(cfgPath, token, check)
	}

	cfg, err := loadConfigForChange(cfgPath, check)
	if err != nil {
		return res, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	restart, err := patchLiveToken(ctx, st, cfg.Auth.Token, token)
	if err != nil {
		return res, fmt.Errorf("the appliance (pid %d) is running but the change could not be confirmed through its management API: %w; check the current token with `remote-mic token get`", st.PID, err)
	}
	res.outcome = changedLive
	if restart {
		res.outcome = changedLiveRestart
	}
	return res, nil
}

// loadConfigForChange loads the config (or defaults, for a first run with no
// file yet) and, when check is non-nil, lets it inspect the current token and
// veto the change. Both the file-edit path (saveToken) and the live-API path
// (changeToken) share this prologue.
func loadConfigForChange(cfgPath string, check func(cur string) error) (config.Config, error) {
	cfg, err := config.LoadOrDefault(cfgPath)
	if err != nil {
		return cfg, withPermHint(err)
	}
	if check != nil {
		if err := check(cfg.Auth.Token); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// saveToken edits the config file directly: load (or default, for a first run
// with no file yet), check, set, and save atomically at 0600.
func saveToken(cfgPath, token string, check func(cur string) error) error {
	cfg, err := loadConfigForChange(cfgPath, check)
	if err != nil {
		return err
	}
	cfg.Auth.Token = token
	return withPermHint(config.Save(cfgPath, &cfg))
}

// reportChange prints headline (what changed in the config file) followed by
// where the change took effect.
func reportChange(w io.Writer, res changeResult, headline string) {
	out(w, "%s\n", headline)
	switch res.outcome {
	case changedFile:
		out(w, "The appliance picks up the change when it next starts.\n")
	case changedFileRestart:
		out(w, "The running appliance (pid %d) has no management API to apply it through,\n"+
			"so it keeps its previous setting until it restarts or its API comes back.\n", res.pid)
	case changedLive:
		out(w, "The running appliance (pid %d) applied it immediately.\n", res.pid)
	case changedLiveRestart:
		out(w, "The running appliance (pid %d) saved it but reports that a restart is\n"+
			"needed to finish applying it.\n", res.pid)
	}
}

// absPath renders cfgPath as an absolute path for a headline, falling back to
// cfgPath itself if the working directory cannot be resolved, so an operator run
// from another directory can see exactly which file the command edited.
func absPath(cfgPath string) string {
	if abs, err := filepath.Abs(cfgPath); err == nil {
		return abs
	}
	return cfgPath
}

// withPermHint adds a pointer to the likely fix when err is a permission
// failure: the config is 0600 and owned by the account the appliance runs as.
func withPermHint(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w (the config is normally readable only by the account the appliance runs as; run this command as that account, for example with sudo -u)", err)
	}
	return err
}
