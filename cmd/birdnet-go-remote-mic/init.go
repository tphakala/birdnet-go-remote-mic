//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// runInit seeds the shared access token into the config file and prints it once.
// The token gates the web UI and management API (Bearer) and the RTSP stream
// (Digest password, any username). It generates a strong random token unless
// --token supplies one, refuses to overwrite an existing token without --force,
// and writes the config atomically at 0600.
//
// The bare token is always written to stdout so `TOKEN=$(... init --quiet)`
// works; the human guidance goes to stderr and --quiet suppresses it.
func runInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: birdnet-go-remote-mic init [flags]\n\n"+
			"Seed and print the shared access token that gates the web UI, the\n"+
			"management API, and the RTSP stream.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	cfgPath := fs.String("config", "config.yaml", "path to the YAML config file")
	force := fs.Bool("force", false, "replace an existing token")
	tokenFlag := fs.String("token", "", "use this token instead of generating one")
	quiet := fs.Bool("quiet", false, "print only the token (no guidance on stderr)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument(s): %s", strings.Join(fs.Args(), " "))
	}

	cfg, err := loadOrDefault(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Auth.Token != "" && !*force {
		return fmt.Errorf("a token is already set in %s; re-run with --force to replace it", *cfgPath)
	}

	token := *tokenFlag
	if token != "" {
		if msg := auth.ValidToken(token); msg != "" {
			return fmt.Errorf("invalid --token: %s", msg)
		}
	} else {
		token, err = auth.GenerateToken()
		if err != nil {
			return fmt.Errorf("generate token: %w", err)
		}
	}
	cfg.Auth.Token = token

	// config.Save writes atomically (temp file + rename, 0600), so a partial or
	// failed write never replaces the existing config. The token was validated
	// just above and the rest of cfg came from a validated Load or from Default,
	// so the saved config is guaranteed valid; no reload-and-revert dance is
	// needed on top of the atomic write.
	if err := config.Save(*cfgPath, &cfg); err != nil {
		return err
	}

	if !*quiet {
		out(stderr, "Access token written to %s (store it now, it is shown once).\n"+
			"The web UI, management API (Bearer), and RTSP stream (Digest\n"+
			"password, any username) now require this token:\n\n", *cfgPath)
	}
	out(stdout, "%s\n", token)
	return nil
}

// loadOrDefault loads the config at path, or returns a fresh Default() when the
// file does not exist yet.
func loadOrDefault(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.Default(), nil
	}
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}
