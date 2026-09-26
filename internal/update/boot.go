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

// staleRequestAge is how old a request found at boot may be before it is
// treated as abandoned (the updater units were removed, or never ran).
const staleRequestAge = time.Hour

// notifySource is the source chip on update notifications.
const notifySource = "update"

// resultPoll is how often the appliance looks for the updater's result while
// an update is in flight.
const resultPoll = 2 * time.Second

// Boot is the appliance's startup step for the update path, run once the
// appliance is up and serving. dir is the staging directory; Boot does
// nothing when it does not exist (the root updater is not installed).
//
// It reports the outcome of an update that restarted this process, writes
// the health file the updater waits for, and clears leftovers of an
// abandoned attempt. When a request is still pending (this process is the
// new version the updater is watching), it keeps looking for the result in
// the background until ctx ends or the updater's health wait has passed.
func Boot(ctx context.Context, dir, version string, pub notify.Publisher, logf func(string, ...any)) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return
	}
	if pub == nil {
		pub = (*notify.Center)(nil) // a no-op, where a nil interface would panic
	}
	reportResult(dir, pub, logf)
	h, err := json.Marshal(Health{Version: version, PID: os.Getpid()})
	if err == nil {
		err = atomicfile.Write(filepath.Join(dir, HealthFile), h, 0o644)
	}
	if err != nil {
		logf("update: write the health file: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, RequestFile))
	switch {
	case err == nil && time.Since(fi.ModTime()) < staleRequestAge:
		go watchResult(ctx, dir, pub, logf, DefaultHealthTimeout+time.Minute)
	case err == nil:
		logf("update: removing an update request abandoned since %s", fi.ModTime().UTC().Format(time.RFC3339))
		(&Stager{Dir: dir}).clean()
	default:
		(&Stager{Dir: dir}).clean()
	}
}

// watchResult polls for the updater's result until one is reported, ctx
// ends, or limit passes.
func watchResult(ctx context.Context, dir string, pub notify.Publisher, logf func(string, ...any), limit time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	t := time.NewTicker(resultPoll)
	defer t.Stop()
	for {
		if reportResult(dir, pub, logf) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reportResult publishes and removes the updater's result, if there is one,
// and returns it.
func reportResult(dir string, pub notify.Publisher, logf func(string, ...any)) *Result {
	res, err := takeResult(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logf("update: %v", err)
		}
		return nil
	}
	logf("update: %s", resultMessage(res))
	pub.Publish(resultNotification(res))
	return res
}

// takeResult reads and removes the status file. A malformed one is removed
// too, so it is reported once.
func takeResult(dir string) (*Result, error) {
	p := filepath.Join(dir, StatusFile)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(p) }()
	if err := regularFile(StatusFile, fi); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // p is the fixed status file in the staging directory
	if err != nil {
		return nil, err
	}
	var res Result
	if err := decodeSmall(StatusFile, b, &res); err != nil {
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
