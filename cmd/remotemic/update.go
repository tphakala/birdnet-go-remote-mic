//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

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
// root updater has nothing to watch. An installed unit that cannot be read
// means this install cannot update itself; the reason is logged, and the hints
// that send the operator to `service install` name this binary's path when
// only root can change it (update.CheckRootOnlyFile). A binary
// that would otherwise be called run by hand gets the neutral by-hand hint
// (see unreadableUnitHint).
func detectInstall(dir string) update.Install {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Printf("update: locate the running binary: %v", err)
		return update.Install{Method: update.MethodManual, Hint: "Download the release for this system from the release page and replace this binary"}
	}
	app, upd, err := service.InstalledBinPaths()
	if err != nil {
		log.Printf("update: cannot read the installed units, so this install cannot update itself: %v", err)
	}
	inst := update.DetectInstall(update.InstallEnv{Exe: exe, ServiceBinPath: app, UpdaterBinPath: upd, DpkgOwns: dpkgOwns, RootOnly: update.CheckRootOnlyFile})
	inst = unreadableUnitHint(inst, exe, app, err)
	if inst.CanApply {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			inst.CanApply = false
			inst.Hint = stagingDirHint(inst, dir)
		}
	}
	return inst
}

// stagingDirHint tells the operator to re-run install (see
// update.Install.RerunInstall) to create the staging directory dir.
func stagingDirHint(inst update.Install, dir string) string {
	return "To create the update staging directory " + dir + ", " + inst.RerunInstall() + ", and " + update.RestartService
}

// unreadableUnitHint keeps a manual install from being told it is run by hand,
// or to install a service, when the reason it has no service path is that the
// installed unit could not be read: the service may well be there. An error
// from the updater unit alone leaves the service path known (serviceBin), and
// the hint that names it stays.
func unreadableUnitHint(inst update.Install, exe, serviceBin string, unitErr error) update.Install {
	if unitErr != nil && serviceBin == "" && inst.Method == update.MethodManual {
		inst.Hint = "Download the release for this system from the release page and replace " + exe
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
	// systemd stops the updater with SIGTERM (a shutdown, service uninstall,
	// or the unit's start timeout). Cancelling ctx then makes a pending health
	// wait roll back rather than leave an unverified binary installed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return applyUpdate(ctx, *binPath, *stateDir)
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
		Restart:  sd.Restart,
		MainPID:  sd.MainPID,
		Version: func(ctx context.Context, bin string) (string, error) {
			b, err := exec.CommandContext(ctx, bin, "version").Output() //nolint:gosec // bin is the staged binary, verified against the signed manifest
			if ee, ok := errors.AsType[*exec.ExitError](err); ok && len(ee.Stderr) > 0 {
				err = fmt.Errorf("%w: %q", err, truncate(strings.TrimSpace(string(ee.Stderr)), 200))
			}
			return string(b), err
		},
	}
	return a.Apply(ctx)
}

// truncate shortens s to at most n bytes for a log or status message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
