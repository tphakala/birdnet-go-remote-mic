package update

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// The staging directory is DirName inside the service's state directory. The
// appliance owns it and writes the staged release there; the root updater
// only reads what it verifies itself, and writes StatusFile back.
const (
	DirName = "update"
	// RequestFile asks the root updater to install the staged release. It is
	// written last, once everything else is in place, and the updater removes
	// it whatever the outcome, so the systemd path unit watching for it does
	// not start the updater again.
	RequestFile = "request.json"
	// BinaryFile is the extracted release binary.
	BinaryFile = "remote-mic.new"
	// ManifestFile and SignatureFile are the exact manifest bytes and
	// signature the appliance verified, for the updater to verify again.
	ManifestFile  = releasemanifest.FileName
	SignatureFile = releasemanifest.SignatureFileName
	// StatusFile is the updater's outcome, read (and removed) by the
	// appliance.
	StatusFile = "status.json"
	// HealthFile is written by the appliance once it is up; the updater waits
	// for one naming the new version before it keeps an update.
	HealthFile = "health.json"
	// downloadFile holds a tarball while it downloads.
	downloadFile = "download.part"
)

// maxSmallFile caps the request, status and health files, which are a few
// dozen bytes.
const maxSmallFile = 4 << 10

// Request is the content of RequestFile. It names the version for the
// updater's log; the updater decides what to install from the manifest.
type Request struct {
	Version string `json:"version"`
}

// Outcome is how an update attempt ended.
type Outcome string

const (
	// OutcomeUpdated means the new version was installed and came up.
	OutcomeUpdated Outcome = "updated"
	// OutcomeRolledBack means the new version was installed but did not come
	// up, so the previous binary was restored.
	OutcomeRolledBack Outcome = "rolled_back"
	// OutcomeFailed means the update did not happen as intended: it was
	// refused before the installed binary was touched, or, when Installed is
	// the new version, the previous binary could not be put back.
	OutcomeFailed Outcome = "failed"
)

// Result is the content of StatusFile.
type Result struct {
	Outcome Outcome `json:"outcome"`
	From    string  `json:"from"`
	To      string  `json:"to"`
	// Installed is the version at the binary path when the result was
	// written: the process running that version is the one to report it.
	Installed string    `json:"installed"`
	Reason    string    `json:"reason,omitempty"`
	Time      time.Time `json:"time"`

	// written records that the updater already wrote this result, so it is
	// written exactly once whatever the outcome.
	written bool
}

// Health is the content of HealthFile.
type Health struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// RunningTarget is the release target key of the running build, as
// releasemanifest.TargetKey names it.
func RunningTarget() string {
	goarm := ""
	if runtime.GOARCH == "arm" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "GOARM" {
					goarm = s.Value
				}
			}
		}
	}
	return releasemanifest.TargetKey(runtime.GOOS, runtime.GOARCH, goarm)
}

// decodeSmall unmarshals a small JSON file's content into v, refusing an
// oversize one.
func decodeSmall(name string, b []byte, v any) error {
	if len(b) > maxSmallFile {
		return fmt.Errorf("%s: %d bytes, limit %d", name, len(b), maxSmallFile)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// regularFile reports an error unless fi describes a regular file: a symlink,
// directory or device where a staged file should be is refused.
func regularFile(name string, fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (%s)", name, fi.Mode().Type())
	}
	return nil
}
