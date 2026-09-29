//go:build linux

package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// prepareStaging creates the update staging directory and withdraws an update
// request left in it. The updater's path unit starts on that file, so a
// request nobody tracks (kept by an uninstall without --purge, or written
// while the appliance was stopped) would otherwise run an update the moment
// the unit is enabled; a running appliance that had asked for it reports the
// update as failed. Install warns when it removes one.
func (in *Installer) prepareStaging(s ServiceSpec, uid, gid int) error {
	if err := in.stagingDir(s.StateDir, uid, gid); err != nil {
		return fmt.Errorf("service: update staging directory %s: %w", s.UpdateDir(), err)
	}
	withdrawn, err := in.withdrawRequest(s.StateDir)
	if err != nil {
		return fmt.Errorf("service: withdraw the leftover update request in %s: %w", s.UpdateDir(), err)
	}
	if withdrawn {
		_, _ = fmt.Fprintf(in.warn, "warning: removed an update request left in %s, so the updater does not start on it\n", s.UpdateDir())
	}
	return nil
}

// withdrawStagedRequest removes request.json from the staging directory
// inside stateDir. It goes through an os.Root on the state directory, which
// the service user owns, so a link planted at the staging directory's name
// cannot make root remove a file elsewhere.
func withdrawStagedRequest(stateDir string) (bool, error) {
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	err = root.Remove(filepath.Join(UpdateDirName, update.RequestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
