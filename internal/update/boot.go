package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/atomicfile"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// An attempt is in flight while its root updater may still be running: a
// request not yet picked up, for updaterStartTimeout (after which the
// process that wrote it withdraws it; a later process withdraws it at
// startup, see WithdrawOrphanedRequest) plus a margin, or the updater's claim, which it
// touches on taking it, for the updater unit's 10 min start timeout plus a
// margin. Anything older is abandoned: the updater units were removed, never
// ran, or were stopped. A file dated in the future (the clock stepped back)
// gets the same allowance the other way, so a small correction does not
// drop a live attempt and a big one does not keep a dead one forever.
const (
	requestMaxAge = updaterStartTimeout + time.Minute
	claimMaxAge   = 15 * time.Minute
)

// notifySource is the source chip on update notifications.
const notifySource = "update"

// resultPoll is how often the appliance looks for the updater's result while
// an update is in flight.
const resultPoll = 2 * time.Second

// WithdrawOrphanedRequest removes an update request an earlier appliance
// process left unclaimed, and reports the attempt as failed. It runs at
// startup, before this process can write a request of its own. Only the
// process that wrote a request waits to withdraw it (Manager.awaitUpdater),
// so one left by a process that stopped during that wait would otherwise stay
// on disk, and the next start of the updater's path unit (a reboot, a
// re-run install) would install an update no one is tracking. The updater
// claims a request by renaming it, so exactly one of the claim and this
// removal wins; a claim is left for Boot to watch. dir is the staging
// directory; nothing happens when it or the request does not exist.
func WithdrawOrphanedRequest(dir, running string, pub notify.Publisher, logf func(string, ...any)) {
	p := filepath.Join(dir, RequestFile)
	if _, err := os.Lstat(p); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logf("update: look for an unclaimed update request: %v", err)
		}
		return
	}
	// The version is only for the report; the request goes either way.
	var req Request
	_ = readSmall(dir, RequestFile, &req)
	if err := os.Remove(p); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logf("update: withdraw an unclaimed update request: %v", err)
		}
		return
	}
	logf("update: withdrew an update request to %q that an earlier run left unclaimed", req.Version)
	orNop(pub).Publish(resultNotification(&Result{
		Outcome: OutcomeFailed,
		From:    running,
		To:      req.Version,
		Reason:  "the appliance restarted before the root updater took the request; start the update again",
	}))
}

// Boot is the appliance's startup step for the update path, run once the
// appliance is up and serving. dir is the staging directory; Boot does
// nothing when it does not exist (the root updater is not installed).
//
// It writes the health file the updater waits for, reports the outcome of
// an update addressed to this version, and clears leftovers of an abandoned
// attempt, logging each abandoned request. While an attempt is in flight
// (a request or claim within its age limit, see attempts: this process may
// be the new version the updater is watching, or have started an attempt of
// its own since WithdrawOrphanedRequest ran), it keeps looking for the result
// in the background until ctx ends or the updater's health wait has passed,
// and leaves results addressed to another version for the process they
// belong to. With nothing in flight it drops a result addressed to another
// version, unless the updater rolled back from this very version and will
// restart the appliance onto the one the result is for (see restoredFrom).
func Boot(ctx context.Context, dir, version string, pub notify.Publisher, logf func(string, ...any)) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return
	}
	pub = orNop(pub)
	h, err := json.Marshal(Health{Version: version, PID: os.Getpid()})
	if err == nil {
		err = atomicfile.Write(filepath.Join(dir, HealthFile), h, 0o644)
	}
	if err != nil {
		logf("update: write the health file: %v", err)
	}
	live, abandoned := attempts(dir)
	if live {
		go watchResult(ctx, dir, version, pub, logf, DefaultHealthTimeout+time.Minute)
		return
	}
	for _, since := range abandoned {
		logf("update: removing an update request abandoned since %s", since.UTC().Format(time.RFC3339))
	}
	// Nothing looks in flight, so a result not addressed to this version
	// will never be reported by anyone: drop it. The exception is a rollback
	// away from this version: the claim can look abandoned while its updater
	// still runs (a reboot mid-install, a clock stepped by more than the
	// claim's age limit), and that updater restarts the appliance onto the
	// restored version, which reports it.
	if reportResult(dir, version, pub, logf) == nil {
		if res, err := readResult(dir); err == nil {
			if restoredFrom(res, version) {
				logf("update: leaving the rollback result for %s, which the updater restored in place of %s", res.Installed, version)
			} else {
				logf("update: dropping a result for %s (this is %s): %s", res.Installed, version, resultMessage(res))
				_ = os.Remove(filepath.Join(dir, StatusFile))
			}
		}
	}
	(&Stager{Dir: dir}).clean()
}

// restoredFrom reports whether res is the updater's rollback away from
// version: the binary on disk is now res.Installed, and the appliance the
// updater restarts runs that version and reports res.
func restoredFrom(res *Result, version string) bool {
	return res.Outcome == OutcomeRolledBack && res.To == version && res.Installed != "" && res.Installed != version
}

// inFlight reports whether an update attempt may still be running in dir.
func inFlight(dir string) bool {
	live, _ := attempts(dir)
	return live
}

// attempts reports whether an update attempt may still be running in dir (a
// request or the updater's claim on one, within its age limit) and, when
// none is, the times of the files abandoned attempts left.
func attempts(dir string) (live bool, abandoned []time.Time) {
	for _, f := range []struct {
		name   string
		maxAge time.Duration
	}{{RequestFile, requestMaxAge}, {TakenFile, claimMaxAge}} {
		fi, err := os.Stat(filepath.Join(dir, f.name))
		if err != nil {
			continue
		}
		if age := time.Since(fi.ModTime()); age > -f.maxAge && age < f.maxAge {
			return true, nil
		}
		abandoned = append(abandoned, fi.ModTime())
	}
	return false, abandoned
}

// watchResult polls for the updater's result addressed to version until one
// is reported, ctx ends, or limit passes.
func watchResult(ctx context.Context, dir, version string, pub notify.Publisher, logf func(string, ...any), limit time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	t := time.NewTicker(resultPoll)
	defer t.Stop()
	for {
		if reportResult(dir, version, pub, logf) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reportResult publishes and removes the updater's result when it is
// addressed to version, and returns it; a result for another version is left
// in place.
func reportResult(dir, version string, pub notify.Publisher, logf func(string, ...any)) *Result {
	res, err := takeResult(dir, version)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errNotAddressed) {
			logf("update: %v", err)
		}
		return nil
	}
	logf("update: %s", resultMessage(res))
	pub.Publish(resultNotification(res))
	return res
}

// errNotAddressed reports a result written for a process running another
// version: the one that will be running once the updater is done.
var errNotAddressed = errors.New("the update result is addressed to another version")

// takeResult reads the status file and removes it when it is addressed to
// version (Installed names the version the result belongs to). A malformed
// one is removed too, so it is reported once.
func takeResult(dir, version string) (*Result, error) {
	res, err := readResult(dir)
	if err != nil {
		return nil, err
	}
	if res.Installed != version {
		return nil, errNotAddressed
	}
	_ = os.Remove(filepath.Join(dir, StatusFile))
	return res, nil
}

// readResult reads the status file without consuming it. A file that is not
// a regular file, or does not parse, is removed and reported as an error; one
// that cannot be read right now is left for the next look.
func readResult(dir string) (*Result, error) {
	p := filepath.Join(dir, StatusFile)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if err := regularFile(StatusFile, fi); err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // p is the fixed status file in the staging directory
	if err != nil {
		return nil, err
	}
	var res Result
	if err := decodeSmall(StatusFile, b, &res); err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	return &res, nil
}

// readSmall decodes the small JSON file name in dir into v, refusing one that
// is not a regular file. It removes nothing.
func readSmall(dir, name string, v any) error {
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if err := regularFile(name, fi); err != nil {
		return err
	}
	b, err := os.ReadFile(p) //nolint:gosec // p is a fixed file name in the staging directory
	if err != nil {
		return err
	}
	return decodeSmall(name, b, v)
}

// orNop returns pub, or a no-op publisher when it is nil, where a nil
// interface would panic.
func orNop(pub notify.Publisher) notify.Publisher {
	if pub == nil {
		return (*notify.Center)(nil)
	}
	return pub
}

func resultMessage(res *Result) string {
	switch res.Outcome {
	case OutcomeUpdated:
		return fmt.Sprintf("Updated from %s to %s", res.From, res.To)
	case OutcomeRolledBack:
		return fmt.Sprintf("%s did not come up, so %s was restored: %s", res.To, res.From, res.Reason)
	default:
		if res.Installed != "" && res.Installed == res.To {
			return fmt.Sprintf("%s did not come up and %s could not be restored: %s", res.To, res.From, res.Reason)
		}
		if res.To == "" {
			return "The update was not installed: " + res.Reason
		}
		return fmt.Sprintf("The update to %s was not installed: %s", res.To, res.Reason)
	}
}

func resultNotification(res *Result) notify.Notification {
	n := notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategorySystem,
		Kind:     notify.KindEvent,
		Source:   notifySource,
		Message:  resultMessage(res),
	}
	switch res.Outcome {
	case OutcomeUpdated:
		n.Severity, n.Title = notify.SeverityInfo, "Updated to "+res.To
	case OutcomeRolledBack:
		n.Title = "Update rolled back"
	default:
		n.Title = "Update failed"
	}
	return n
}
