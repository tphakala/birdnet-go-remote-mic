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
// appliance withdraws it) plus a margin, or the updater's claim, which it
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

// Boot is the appliance's startup step for the update path, run once the
// appliance is up and serving. dir is the staging directory; Boot does
// nothing when it does not exist (the root updater is not installed).
//
// It writes the health file the updater waits for, reports the outcome of
// an update addressed to this version, and clears leftovers of an abandoned
// attempt, logging each abandoned request. While an attempt is in flight
// (a request or claim within its age limit, see attempts: this process may
// be the new version the updater is watching), it keeps looking for the result in the background until ctx
// ends or the updater's health wait has passed, and leaves results addressed
// to another version for the process they belong to.
func Boot(ctx context.Context, dir, version string, pub notify.Publisher, logf func(string, ...any)) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return
	}
	if pub == nil {
		pub = (*notify.Center)(nil) // a no-op, where a nil interface would panic
	}
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
	// Nothing is in flight, so a result not addressed to this version will
	// never be reported by anyone: drop it.
	if reportResult(dir, version, pub, logf) == nil {
		if res, err := readResult(dir); err == nil {
			logf("update: dropping a result for %s (this is %s): %s", res.Installed, version, resultMessage(res))
			_ = os.Remove(filepath.Join(dir, StatusFile))
		}
	}
	(&Stager{Dir: dir}).clean()
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
// a regular file, or does not parse, is removed and reported as an error.
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
