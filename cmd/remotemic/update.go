//go:build linux

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
	"github.com/tphakala/birdnet-go-remote-mic/internal/service"
	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// dpkgListGlob matches the file list dpkg keeps for the .deb package (the
// package name is set in .goreleaser.yaml).
const dpkgListGlob = "/var/lib/dpkg/info/birdnet-go-remote-mic*.list"

// updateDirFor is the update staging directory: DirName inside the directory
// holding the management certificate, which a service install points at its
// state directory (--cert-dir), the directory the root updater watches.
func updateDirFor(cfg *config.Config, cfgPath string) string {
	dir := cfg.Management.CertDir
	if dir == "" {
		dir = filepath.Dir(cfgPath)
	}
	return filepath.Join(dir, update.DirName)
}

// newUpdateManager wires the release update subsystem: the verifying fetcher,
// the stager, and the install detection that decides whether the one-button
// update is offered. It returns nil, with the reason logged, only when the
// compiled-in release keys do not parse, which a release build's CI prevents.
func newUpdateManager(ctx context.Context, dir string, center notify.Publisher) *update.Manager {
	trusted, err := releasemanifest.TrustedKeys()
	if err != nil {
		log.Printf("update: release keys: %v; update checks are off", err)
		return nil
	}
	ua := "remote-mic/" + version
	fetcher := &update.Fetcher{Client: http.DefaultClient, Base: update.DefaultBase, Trusted: trusted, UserAgent: ua}
	stager := &update.Stager{Dir: dir, Client: http.DefaultClient, UserAgent: ua, Target: update.RunningTarget()}
	return update.NewManager(ctx, &update.Config{
		Running:   version,
		Fetch:     fetcher.Latest,
		Stage:     stager.Stage,
		Dir:       dir,
		Install:   detectInstall(dir),
		Publisher: center,
	})
}

// detectInstall classifies this installation from the running binary's path,
// the installed systemd units, and dpkg's file lists. The one-button update
// also needs the staging directory the installer creates; without it the
// root updater has nothing to watch.
func detectInstall(dir string) update.Install {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Printf("update: locate the running binary: %v", err)
		return update.Install{Method: update.MethodManual, Hint: "Download the release for this system from the release page and replace this binary"}
	}
	app, upd := service.InstalledBinPaths()
	inst := update.DetectInstall(update.InstallEnv{Exe: exe, ServiceBinPath: app, UpdaterBinPath: upd, DpkgOwns: dpkgOwns})
	if inst.CanApply {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			inst.CanApply = false
			inst.Hint = "Re-run sudo remote-mic service install to create the update staging directory " + dir
		}
	}
	return inst
}

// dpkgOwns reports whether a dpkg file list of the package names path.
func dpkgOwns(path string) bool {
	lists, _ := filepath.Glob(dpkgListGlob)
	for _, l := range lists {
		if fileListHas(l, path) {
			return true
		}
	}
	return false
}

func fileListHas(list, path string) bool {
	f, err := os.Open(list) //nolint:gosec // a dpkg file list matched by dpkgListGlob
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() == path {
			return true
		}
	}
	return false
}

// runServiceApplyUpdate is `remote-mic service apply-update`, the root
// updater the remote-mic-update.service unit runs when the appliance stages
// a release. It is not meant to be run by hand, so it never escalates.
func runServiceApplyUpdate(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("service apply-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		out(stderr, "Usage: remote-mic service apply-update [flags]\n\n"+
			"Install an update the appliance staged. Run by the remote-mic-update\n"+
			"systemd unit as root; not meant to be run by hand.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	binPath := fs.String("bin-path", service.DefaultBinPath, "installed binary to replace")
	stateDir := fs.String("state-dir", service.DefaultStateDir, "state directory holding the update staging directory")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if geteuid() != 0 {
		return fmt.Errorf("apply-update must run as root (it is started by %s)", service.UpdateServiceUnit)
	}
	return applyUpdate(context.Background(), *binPath, *stateDir)
}

// applyUpdate is a seam so the command's flag handling is testable without
// installing anything.
var applyUpdate = func(ctx context.Context, binPath, stateDir string) error {
	trusted, err := releasemanifest.TrustedKeys()
	if err != nil {
		return err
	}
	sd := service.NewSystemd()
	a := &update.Applier{
		StateDir: stateDir,
		BinPath:  binPath,
		Unit:     service.DefaultUnitName,
		Running:  version,
		Target:   update.RunningTarget(),
		Trusted:  trusted,
		Restart: func(unit string) error {
			_, err := sd.Run("systemctl", "restart", unit)
			return err
		},
		Active: sd.IsActive,
		Version: func(ctx context.Context, bin string) (string, error) {
			b, err := exec.CommandContext(ctx, bin, "version").Output() //nolint:gosec // bin is the staged binary, verified against the signed manifest
			return string(b), err
		},
	}
	return a.Apply(ctx)
}
