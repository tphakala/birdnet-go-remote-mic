package update

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// Check timing. The first check comes a few minutes after startup (or after
// checks are turned on), so a boot loop does not hammer the release host;
// later ones come about daily, jittered so a fleet does not check in step.
// A failed check is retried sooner, backing off to the daily interval.
const (
	firstCheckDelay  = 3 * time.Minute
	firstCheckJitter = 2 * time.Minute
	checkInterval    = 24 * time.Hour
	checkJitter      = time.Hour
	retryFirst       = time.Hour
	// checkTimeout bounds a check, and stays under the management server's
	// 30 s WriteTimeout so a manual check still gets its answer written.
	checkTimeout = 20 * time.Second
	// manualThrottle is how recent a check may be before a manual check
	// returns it instead of asking again.
	manualThrottle = 10 * time.Second
	// stageTimeout bounds the tarball download on a slow link.
	stageTimeout = 15 * time.Minute
	// updaterStartTimeout is how long a request may wait for the root updater
	// to pick it up before the attempt is abandoned.
	updaterStartTimeout = time.Minute
)

// AvailableKey is the notification condition raised while a newer release
// is available.
const AvailableKey = "update:available"

// Sentinel errors for the management API.
var (
	ErrChecksDisabled = errors.New("update checks are turned off")
	ErrNoUpdate       = errors.New("no update is available")
	ErrCannotApply    = errors.New("this installation cannot update itself")
	ErrBusy           = errors.New("an update is already in progress")
)

// errEarlierAttempt is ErrBusy for an attempt found on disk rather than one
// this process started. The status may then read idle or failed (the attempt
// was started before a restart, or the updater was killed mid-run), so the
// message says why the button is refused and how long it can last.
var errEarlierAttempt = fmt.Errorf("%w: an earlier update attempt may still be running (it counts as abandoned at most %d minutes after it started); try again later", ErrBusy, int(claimMaxAge/time.Minute))

// Phase is where a one-button update stands.
type Phase string

const (
	// PhaseIdle means no update is in progress.
	PhaseIdle Phase = "idle"
	// PhaseDownloading means the release is being downloaded and staged.
	PhaseDownloading Phase = "downloading"
	// PhaseInstalling means the staged release was handed to the root
	// updater, which restarts the appliance.
	PhaseInstalling Phase = "installing"
	// PhaseFailed means the last attempt failed; PhaseMessage says why.
	PhaseFailed Phase = "failed"
)

// Status is a snapshot of the update state for the management API.
type Status struct {
	// Current is the running version.
	Current string
	// Supported is false for a build that names no release (such as one
	// built as "dev"), which never checks.
	Supported bool
	// CheckEnabled mirrors the operator's toggle.
	CheckEnabled bool
	// Latest and NotesURL describe the newest release found, empty before a
	// successful check. Available reports whether Latest is an update.
	Latest    string
	NotesURL  string
	Available bool
	// LastCheck is when the last check finished (zero before one); LastError
	// is why it failed, empty after a success.
	LastCheck time.Time
	LastError string
	// Install says how the binary was installed and whether it can update
	// itself.
	Install Install
	// Phase and PhaseMessage report a one-button update in progress.
	Phase        Phase
	PhaseMessage string
}

// Config wires a Manager.
type Config struct {
	// Running is the running version.
	Running string
	// Fetch returns the newest verified release (Fetcher.Latest).
	Fetch func(ctx context.Context) (*Release, error)
	// Stage stages a release for the root updater (Stager.Stage).
	Stage func(ctx context.Context, rel *Release) error
	// Dir is the staging directory, where the updater's result appears.
	Dir string
	// Install is the detected installation.
	Install Install
	// Publisher receives the available condition and update events.
	Publisher notify.Publisher
	// Logf logs; log.Printf when nil.
	Logf func(format string, args ...any)
	// Jitter returns a uniform duration in [0, n); rand when nil.
	Jitter func(n time.Duration) time.Duration
}

// Manager runs the periodic update check as a condition monitor (the
// appliance re-arms it through Apply on every config reload) and drives the
// one-button update. The check is background work without a live consumer,
// which the appliance allows only because its result is a notification; with
// checks turned off it makes no request at all.
type Manager struct {
	cfg       Config
	supported bool
	enabled   atomic.Bool
	wake      chan struct{}
	ctx       context.Context // the appliance's lifetime, for apply goroutines

	checkMu sync.Mutex // serializes checks

	mu        sync.Mutex
	latest    *Release
	available bool
	lastCheck time.Time
	lastErr   string
	lastCause string
	failures  int    // consecutive failed checks
	announced string // the version last logged as available
	phase     Phase
	phaseMsg  string
	// cancelApply stops an in-flight download when checks are turned off.
	cancelApply context.CancelCauseFunc
	// cancelCheck stops an in-flight check when checks are turned off.
	cancelCheck context.CancelCauseFunc
}

// Checks run only while the monitors have been applied with UpdateCheck on.
var _ monitor.Monitors = (*Manager)(nil)

// NewManager builds a Manager; Run starts its check loop.
func NewManager(ctx context.Context, c *Config) *Manager {
	cfg := *c
	_, supported := BaseVersion(cfg.Running)
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Publisher == nil {
		// A nil *Center is a no-op Publisher; a nil interface would panic.
		cfg.Publisher = (*notify.Center)(nil)
	}
	if cfg.Jitter == nil {
		cfg.Jitter = func(n time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(n))) }
	}
	return &Manager{cfg: cfg, supported: supported, wake: make(chan struct{}, 1), ctx: ctx, phase: PhaseIdle}
}

// Apply turns the periodic check on or off from the config. Turning it off
// forgets the release found, clears its notification, and stops a check or
// download in progress, so the appliance makes no update request with checks
// off. An update already handed to the root updater is past that point and
// goes on.
func (m *Manager) Apply(s *monitor.Settings) {
	was := m.enabled.Swap(s.UpdateCheck)
	if was == s.UpdateCheck {
		return
	}
	if !s.UpdateCheck {
		m.mu.Lock()
		m.latest, m.available = nil, false
		cancel := m.cancelApply
		// Under m.mu, so a check finishing now either sees checks off or has
		// already raised the condition this resolves.
		m.cfg.Publisher.Resolve(AvailableKey, "Update checks turned off")
		// Also under m.mu, where the check records its result: a fetch that
		// returns on its own just after this, with checks turned back on in
		// between, still finds the cause set and keeps nothing.
		if m.cancelCheck != nil {
			m.cancelCheck(ErrChecksDisabled)
		}
		m.mu.Unlock()
		if cancel != nil {
			cancel(ErrChecksDisabled)
		}
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run is the check loop; it returns when ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if !m.supported {
		m.cfg.Logf("update: %s is not a release build; update checks are off", m.cfg.Running)
		return
	}
	var timer *time.Timer
	stop := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}
	defer stop()
	for {
		on := m.enabled.Load()
		switch {
		case on && timer == nil:
			timer = time.NewTimer(firstCheckDelay + m.cfg.Jitter(firstCheckJitter))
		case !on:
			stop()
		}
		var tick <-chan time.Time
		if timer != nil {
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tick:
			failures := m.check(ctx, false)
			timer = time.NewTimer(nextCheck(failures, m.cfg.Jitter))
		}
	}
}

// nextCheck is the delay after a check: about a day after a success, and
// retryFirst doubling per consecutive failure up to a day after a failure.
func nextCheck(failures int, jitter func(time.Duration) time.Duration) time.Duration {
	if failures == 0 {
		return checkInterval - checkJitter + jitter(2*checkJitter)
	}
	d := retryFirst
	for range failures - 1 {
		d *= 2
		if d >= checkInterval {
			return checkInterval
		}
	}
	return d
}

// CheckNow checks immediately for the management API. A check finished in
// the last few seconds is returned as it is. The check runs on the
// appliance's own lifetime, not the caller's: a client that goes away mid
// check must not turn it into a recorded failure.
func (m *Manager) CheckNow(context.Context) (Status, error) {
	if !m.supported {
		return m.Status(), ErrNotRelease
	}
	if !m.enabled.Load() {
		return m.Status(), ErrChecksDisabled
	}
	m.check(m.ctx, true)
	return m.Status(), nil
}

// check fetches the newest release and records the outcome, unless checks
// were turned off meanwhile; turning them off also stops a fetch in progress,
// which is then not recorded even if checks are back on when it returns.
// A manual check skips the fetch when another finished within
// manualThrottle, re-read after waiting for any check in flight, so a burst
// of requests makes one fetch. It returns the number of consecutive failures,
// zero after a success.
func (m *Manager) check(ctx context.Context, manual bool) int {
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	cctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	m.mu.Lock()
	// Checks may have been turned off while this call waited for another.
	// Read under m.mu, where the cancel is registered: Apply(off) sets
	// enabled before it takes m.mu, so it either is seen here or finds the
	// cancel and stops the fetch.
	skip := !m.enabled.Load() ||
		manual && !m.lastCheck.IsZero() && time.Since(m.lastCheck) < manualThrottle
	failures := m.failures
	if !skip {
		m.cancelCheck = stop
	}
	m.mu.Unlock()
	if skip {
		return failures
	}
	fctx, cancel := context.WithTimeout(cctx, checkTimeout)
	rel, err := m.cfg.Fetch(fctx)
	cancel()
	var newer bool
	if err == nil {
		newer, err = Newer(rel.Manifest.Version, m.cfg.Running)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelCheck = nil
	// Turned off while fetching: keep nothing, even when checks were turned
	// back on before the stopped fetch returned, since its error is not a
	// failed check.
	if !m.enabled.Load() || errors.Is(context.Cause(cctx), ErrChecksDisabled) {
		return m.failures
	}
	m.lastCheck = time.Now()
	if err != nil {
		m.lastErr = err.Error()
		// Log once per cause, not once per attempt: an appliance on an
		// isolated network fails every check the same way.
		if c := failureCause(err); c != m.lastCause {
			m.lastCause = c
			m.cfg.Logf("update: check failed: %v", err)
		}
		return m.failuresLocked()
	}
	if m.lastCause != "" {
		m.cfg.Logf("update: checks succeed again")
	}
	m.lastErr, m.lastCause, m.failures = "", "", 0
	m.latest, m.available = rel, newer
	if !newer {
		m.cfg.Publisher.Resolve(AvailableKey, "No update available")
		return 0
	}
	v := rel.Manifest.Version
	if m.announced != v {
		m.announced = v
		m.cfg.Logf("update: %s is available (running %s)", v, m.cfg.Running)
	}
	title := "Update available: " + v
	msg := fmt.Sprintf("remote-mic %s is available; this appliance runs %s. Release notes: %s", v, m.cfg.Running, rel.Manifest.NotesURL)
	if !m.cfg.Publisher.Onset(notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategorySystem,
		Key:      AvailableKey,
		Source:   notifySource,
		Title:    title,
		Message:  msg,
	}) {
		if u, ok := m.cfg.Publisher.(notify.Updater); ok {
			u.Update(AvailableKey, title, msg)
		}
	}
	return 0
}

func (m *Manager) failuresLocked() int {
	m.failures++
	return m.failures
}

func isNetError(err error) bool {
	_, ok := errors.AsType[net.Error](err)
	return ok
}

// failureCause names the kind of a check failure, for logging each kind once.
func failureCause(err error) string {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return fmt.Sprintf("http %d", se.Code)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case isNetError(err):
		return "network"
	case errors.Is(err, ErrNoRelease):
		return "no release"
	case errors.Is(err, releasemanifest.ErrBadSignature), errors.Is(err, releasemanifest.ErrUntrustedKey):
		return "signature"
	default:
		return err.Error()
	}
}

// Status returns the current state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		Current:      m.cfg.Running,
		Supported:    m.supported,
		CheckEnabled: m.enabled.Load(),
		Available:    m.available,
		LastCheck:    m.lastCheck,
		LastError:    m.lastErr,
		Install:      m.cfg.Install,
		Phase:        m.phase,
		PhaseMessage: m.phaseMsg,
	}
	if m.latest != nil {
		s.Latest, s.NotesURL = m.latest.Manifest.Version, m.latest.Manifest.NotesURL
	}
	return s
}

// StartApply begins a one-button update to the newest release found: it
// stages the release in the background and hands it to the root updater,
// which restarts the appliance. It returns at once with the new phase. It
// refuses while checks are off, since staging downloads the release, and
// while an attempt is in flight, here or on disk.
func (m *Manager) StartApply() (Status, error) {
	if !m.cfg.Install.CanApply {
		return m.Status(), ErrCannotApply
	}
	if !m.enabled.Load() {
		return m.Status(), ErrChecksDisabled
	}
	// An attempt on disk counts too: one started before this process (which
	// may be the new version the updater is still watching) has not ended.
	if inFlight(m.cfg.Dir) {
		return m.Status(), errEarlierAttempt
	}
	m.mu.Lock()
	switch {
	case m.phase == PhaseDownloading || m.phase == PhaseInstalling:
		m.mu.Unlock()
		return m.Status(), ErrBusy
	case !m.available || m.latest == nil:
		m.mu.Unlock()
		return m.Status(), ErrNoUpdate
	}
	rel := m.latest
	m.phase, m.phaseMsg = PhaseDownloading, "Downloading "+rel.Manifest.Version
	// The cancel is in place before m.mu is released: Apply(off) sets
	// enabled before it takes m.mu, so it either made latest nil above or
	// finds this cancel.
	ctx, stop := context.WithCancelCause(m.ctx)
	m.cancelApply = stop
	m.mu.Unlock()
	go m.apply(ctx, stop, rel)
	return m.Status(), nil
}

// apply stages rel on ctx, which turning checks off or the appliance shutting
// down cancels (the attempt then goes back to idle), and watches for the updater to take it. stop
// releases ctx once staging ends. A panic here would end the appliance, so
// it is recovered and reported as a failure.
func (m *Manager) apply(ctx context.Context, stop context.CancelCauseFunc, rel *Release) {
	v := rel.Manifest.Version
	defer func() {
		if r := recover(); r != nil {
			m.fail(v, fmt.Errorf("internal error: %v", r))
		}
	}()
	m.cfg.Logf("update: downloading %s", v)
	sctx, cancel := context.WithTimeout(ctx, stageTimeout)
	err := m.cfg.Stage(sctx, rel)
	cancel()
	m.mu.Lock()
	m.cancelApply = nil
	m.mu.Unlock()
	stop(nil)
	if err != nil {
		// Turning checks off is the operator's choice, and shutting down
		// the appliance's; neither is a failed update.
		if errors.Is(context.Cause(ctx), ErrChecksDisabled) {
			m.cfg.Logf("update: stopped downloading %s: update checks were turned off", v)
			m.setPhase(PhaseIdle, "")
			return
		}
		if m.ctx.Err() != nil {
			m.cfg.Logf("update: stopped downloading %s: shutting down", v)
			m.setPhase(PhaseIdle, "")
			return
		}
		m.fail(v, err)
		return
	}
	m.setPhase(PhaseInstalling, "Installing "+v+"; the appliance restarts when it is done")
	m.cfg.Logf("update: %s staged; waiting for the root updater", v)
	m.cfg.Publisher.Publish(notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategorySystem,
		Kind:     notify.KindEvent,
		Source:   notifySource,
		Title:    "Installing " + v,
		Message:  "The update to " + v + " is being installed; the appliance restarts when it is done, which drops connected streams briefly",
	})
	m.awaitUpdater(v)
}

// awaitUpdater polls for the root updater's result. The updater restarts this
// process when it installs, so a result addressed to this version here means
// it refused the update or could not complete it. A request still unclaimed
// after updaterStartTimeout is withdrawn by removing it: the updater claims
// one by renaming it, so a failed remove means the updater has it and is
// working, however slowly.
func (m *Manager) awaitUpdater(v string) {
	start := time.Now()
	t := time.NewTicker(resultPoll)
	defer t.Stop()
	withdrawTried := false
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		if res, err := takeResult(m.cfg.Dir, m.cfg.Running); err == nil {
			if res.Outcome == OutcomeFailed {
				// An updater that failed to claim the request leaves it behind.
				_ = os.Remove(filepath.Join(m.cfg.Dir, RequestFile))
				m.fail(v, errors.New(res.Reason))
				return
			}
			m.cfg.Publisher.Publish(resultNotification(res))
			m.setPhase(PhaseIdle, "")
			return
		}
		switch {
		case !withdrawTried && time.Since(start) > updaterStartTimeout:
			withdrawTried = true
			if os.Remove(filepath.Join(m.cfg.Dir, RequestFile)) == nil {
				m.fail(v, errors.New("the root updater did not start; re-run sudo remote-mic service install"))
				return
			}
		case time.Since(start) > updaterStartTimeout+DefaultHealthTimeout+time.Minute:
			m.fail(v, errors.New("the root updater reported no result"))
			return
		}
	}
}

func (m *Manager) setPhase(p Phase, msg string) {
	m.mu.Lock()
	m.phase, m.phaseMsg = p, msg
	m.mu.Unlock()
}

// fail records a failed attempt and reports it.
func (m *Manager) fail(v string, err error) {
	m.cfg.Logf("update: the update to %s failed: %v", v, err)
	m.setPhase(PhaseFailed, err.Error())
	m.cfg.Publisher.Publish(resultNotification(&Result{Outcome: OutcomeFailed, From: m.cfg.Running, To: v, Reason: err.Error()}))
}
