//go:build linux

// Command remotemic is the remote microphone appliance: it captures
// local audio and serves it over RTSP/RTP for BirdNET-Go to pull.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	capture "github.com/tphakala/go-audio-capture"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"

	"github.com/tphakala/birdnet-go-remote-mic/internal/announce"
	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/monitor"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sse"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// deviceInUse reports whether a capture device is held exclusively by another
// process, via a non-blocking capability query. It is a package var so the open
// retry is testable without hardware.
var deviceInUse = audio.DeviceInUse

// resolveOpenChannels resolves the hardware channel count to open a device at,
// rounding its selection up to a count the card supports. It is a package var
// so the open retry's per-attempt resolution is testable without hardware.
var resolveOpenChannels = audio.ResolveOpenChannels

// startPprof serves net/http/pprof diagnostics on addr until ctx is cancelled. It
// is only reached when the operator passes --pprof: the endpoints expose CPU/heap
// profiles, a live goroutine dump, and the command line with no authentication, so
// on a field appliance addr should be loopback (127.0.0.1:port). The handlers are
// served from a private mux so this listener exposes only pprof; note that
// importing net/http/pprof also registers them on http.DefaultServeMux via its
// init, so the appliance must never serve http.DefaultServeMux. The bind is
// synchronous so a bad address or a port already in use is reported here and the
// appliance keeps capturing and serving audio (non-fatal, like the management
// listener) rather than the failure being swallowed in the serve goroutine.
// WriteTimeout is left unset so a /debug/pprof/profile?seconds=N capture can stream
// its full duration; ReadHeaderTimeout, ReadTimeout, and IdleTimeout still bound a
// slow or idle client (gosec G112), matching the management listener.
func startPprof(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("pprof disabled: cannot listen on %s: %v", addr, err)
		return
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout stays unset so a /debug/pprof/profile?seconds=N capture can
		// stream its full duration; the read and idle timeouts bound a slow or idle
		// client without truncating that response.
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		log.Printf("pprof: serving diagnostics on %s", ln.Addr())
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			log.Printf("pprof: %v", serr)
		}
	}()
}

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// streamRuntime bundles one stream fanned out from a device's shared capture: its
// selecting source (the device's channels it carries), its pipeline stage, its
// frame buffer and its RTSP track. dropped counts audio lost for this stream,
// whether the fan-out dropped a period (its encoder is slow) or the RTSP writer
// dropped a frame (its client is slow); the device sums them for the host
// monitor.
type streamRuntime struct {
	stream  config.Stream
	src     audio.Source
	stage   pipeline.Stage
	frames  *rtspserver.ChanSource
	track   *rtspserver.Track
	dropped atomic.Uint64
	// encoded records that this stream's stage has emitted an encoded frame,
	// which happens only while a client plays it (see pipeline.Stage). An
	// unattended retry after this stream's encode fault waits for it before its
	// settle (see encodeFault). Set once by the stage goroutine, read
	// by the run loop.
	encoded atomic.Bool
}

// noteEncoded records the stream's first encoded frame and wakes the run loop
// when a retry is waiting for one (await is the device's awaitEncode). It costs
// one atomic load per frame after the first.
func (sr *streamRuntime) noteEncoded(await *atomic.Bool, wake func()) {
	if sr.encoded.Load() {
		return
	}
	sr.encoded.Store(true)
	if await.Load() {
		wake()
	}
}

// deviceRuntime bundles one configured device's moving parts: one exclusive
// capture, metered once over every opened channel, fanned out into one or more
// streams. A device that failed to open keeps a record with src nil and no
// streams so the management API can still report it (state skipped). src, fanout,
// and streams are set only when it opened.
type deviceRuntime struct {
	dev      config.Device
	src      audio.Source  // the metered base capture; teardown closes it via fanout.Close (idempotent), which ends the pump
	fanout   *audio.Fanout // reads src on the pump goroutine and feeds every stream
	streams  []*streamRuntime
	rate     int
	channels int // opened hardware channel count (every channel is metered)
	// format is the negotiated hardware capture format token (s16, s24_le,
	// s24_3le, s32); a wider capture is downconverted to S16LE for the stream.
	// Set at open, read without a lock like rate and channels.
	format string
	// friendlyName is the sound card's human label; supportedRates and
	// supportedChannels are the rate and channel-count sets the device accepted at
	// the startup probe. All static per run and read without a lock.
	friendlyName      string
	supportedRates    []int
	supportedChannels []int
	// hwAddr is the current-boot ALSA address ("hw:4,0") the configured id
	// resolved to when this record was built, for display only; empty when the
	// id resolved to no present hardware. Static per record.
	hwAddr string
	// gen is a process-unique identity for this runtime instance, assigned at
	// creation (see runtimeGen). A restart builds a fresh runtime with a fresh gen,
	// so the host monitor rebaselines the dropped-frame counter on the change even
	// when the new runtime's counter has already climbed past the old value. Static
	// per run; read without a lock, like dev.Name.
	gen uint64

	mu    sync.Mutex
	state mgmtserver.DeviceState
	err   string

	// superseded marks a device the reconcile loop deliberately stopped (a
	// hot-reload stop or restart). Its pump still delivers a final pumpResult;
	// the loop reads this to skip the spontaneous-death cleanup, so a device
	// restarted on the same RTSP path is not torn down by the old pump's exit.
	// Set and read only on the run-loop goroutine.
	superseded bool

	// awaitEncode asks the stage goroutines to wake the run loop (through
	// retryDue) when a stream encodes its first frame. Set by the run loop before
	// it checks any stream's encoded flag, and read by a stage after it sets its
	// flag, so one side always sees the other.
	awaitEncode atomic.Bool
}

// encodedAll reports whether every stream of this runtime whose path is listed
// has encoded a frame. A listed path the runtime no longer serves counts as
// proven: nothing on it can fault again.
func (rt *deviceRuntime) encodedAll(paths []string) bool {
	for _, sr := range rt.streams {
		if slices.Contains(paths, sr.stream.Path) && !sr.encoded.Load() {
			return false
		}
	}
	return true
}

// droppedTotal sums every stream's dropped-audio counter, the device-level figure
// the host monitor watches for a rising-drops condition.
func (rt *deviceRuntime) droppedTotal() uint64 {
	var n uint64
	for _, sr := range rt.streams {
		n += sr.dropped.Load()
	}
	return n
}

// runtimeGen hands out a process-unique generation to each serving deviceRuntime
// so the host monitor can tell one runtime from its restarted successor. Only
// openDevice (the sole builder of a serving runtime) draws from it; skipped and
// disabled records keep gen 0 and never reach the drop monitor.
var runtimeGen atomic.Uint64

// openDevice opens and starts capture for one configured device at the resolved
// hardware channel count openCh, meters every opened channel once, and fans the
// capture out into one pipeline stage, SDP, and RTSP track per configured stream.
// The meter and fan-out run on the capture pump regardless of whether any RTSP
// client is connected, but the fan-out copies a period only for a stream a
// client is playing. Each stream extracts its own channels from the shared
// capture with a selecting source, so the device opens the hardware exactly once.
func openDevice(dev *config.Device, openCh int, hub *levels.Hub) (*deviceRuntime, error) {
	base, capFormat, err := audio.OpenCaptureAt(dev, openCh)
	if err != nil {
		return nil, fmt.Errorf("open capture: %w", err)
	}
	rate, channels := base.Negotiated()

	// Build every stream's stage, SDP and track before starting anything, so a
	// failure here closes the capture and reports the device skipped rather than
	// leaving a half-registered device or a phantom meter behind.
	streams := make([]*streamRuntime, 0, len(dev.Streams))
	for i := range dev.Streams {
		s := dev.Streams[i]
		selCount := len(s.Channels)
		stage, payloadType := buildStage(&s)
		sdpBytes, serr := sdp.WriteSession(pipeline.SDPSpec(&s, dev.Name, rate, selCount))
		if serr != nil {
			_ = base.Close()
			return nil, fmt.Errorf("build sdp for %s: %w", s.Path, serr)
		}
		frames := rtspserver.NewChanSource(64)
		streams = append(streams, &streamRuntime{
			stream: s,
			stage:  stage,
			frames: frames,
			track:  &rtspserver.Track{Path: s.Path, SDP: sdpBytes, PayloadType: payloadType, Frames: frames},
		})
	}

	// Meter every captured channel once on the shared reader, then fan the metered
	// capture out to each stream's selecting source. Registering the meter after the
	// fallible build above keeps a device that fails there out of the levels hub.
	// Each consumer is gated on its stream feed's play session, so a stream with
	// no client playing costs no per-period copy or channel extraction, and each
	// period it gets is tagged with that session.
	metered := audio.NewMeteredSource(base, hub.Meter(dev.Name, channels))
	fanout, consumers := audio.NewFanout(metered, dev.Name, fanoutStreams(streams))
	for i := range streams {
		streams[i].src = audio.NewSelectingSource(consumers[i], channels, streams[i].stream.Channels)
	}
	return &deviceRuntime{
		dev:      *dev,
		gen:      runtimeGen.Add(1),
		src:      metered,
		fanout:   fanout,
		streams:  streams,
		rate:     rate,
		channels: channels,
		format:   capFormat.String(),
	}, nil
}

// openDeviceRetry opens a device, retrying a few times on failure. The retry
// matters most on a hot-reload restart: ALSA is single-client and the kernel may
// not release a hw device the instant Close returns, so an immediate reopen of
// the same card can transiently fail with EBUSY. A handful of short retries rides
// that out; a device that still will not open is reported skipped, not dropped.
// An error that cannot change between attempts (a malformed id, no such device,
// an ambiguous id, or an invalid config; see permanentOpenError) returns at once
// rather than sleeping out the retry budget.
//
// The hardware open channel count is resolved at the top of EACH attempt, not
// once up front. openDeviceRetry runs right after the old capture source was
// closed, inside the very EBUSY window the retry exists to ride out; a probe
// during that window fails, so ResolveOpenChannels falls back to max(selection).
// Re-resolving per attempt lets a card freed between attempts be opened at the
// count it actually needs (a stereo-only card whose mono fallback would fail to
// open), instead of a wrong value pinned from the first probe. The busy gate and
// the capture open within one attempt share that attempt's resolved count, so
// they never disagree.
func openDeviceRetry(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
	const attempts = 5
	const delay = 50 * time.Millisecond
	var err error
	for i := range attempts {
		openCh := resolveOpenChannels(dev.Device, dev.StreamChannelUnion())
		// Gate the blocking capture open on a non-blocking busy check. A device
		// held exclusively by another process can make the ALSA open block rather
		// than fail promptly, which would park the single reconcile goroutine and
		// stall the whole capture-open phase (no records publish, RTSP never comes
		// up). The O_NONBLOCK capability query returns at once, so a busy device is
		// retried and then skipped instead of blocking. The retry also rides out
		// the transient EBUSY window right after a hot-reload Close, before the
		// kernel releases the card, so a same-card restart is not falsely skipped.
		if deviceInUse(dev.Device, openCh) {
			err = capture.ErrDeviceInUse
		} else {
			var rt *deviceRuntime
			if rt, err = openDevice(dev, openCh, hub); err == nil {
				return rt, nil
			}
			if permanentOpenError(err) {
				return nil, err
			}
		}
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return nil, err
}

// permanentOpenError reports whether an open failure cannot change between
// retry attempts, so openDeviceRetry returns it at once instead of spending the
// retry budget: a malformed id (BadDeviceError), an id that names no device
// (DeviceNotFoundError) or several (AmbiguousDeviceError), and an invalid config
// (ConfigError). A rate, format or channel rejection (BadRateError,
// BadFormatError) is NOT treated as permanent: openDeviceRetry re-resolves the
// open channel count each attempt, and the rates and formats a device accepts
// can depend on that count, so a stereo-only card that failed at the mono
// fallback can open once it frees and resolves to stereo. Everything else (a
// busy or transiently failing device) is retried.
func permanentOpenError(err error) bool {
	_, badDev := errors.AsType[*capture.BadDeviceError](err)
	_, badCfg := errors.AsType[*capture.ConfigError](err)
	_, missing := errors.AsType[*capture.DeviceNotFoundError](err)
	_, amb := errors.AsType[*capture.AmbiguousDeviceError](err)
	return badDev || badCfg || missing || amb
}

// lockState builds the run-lock state for a serving management API. certPath
// is made absolute: it derives from --config, which may be relative to this
// process's working directory, and a token command reading the lock usually
// runs from somewhere else.
func lockState(pid int, mgmtAddr, certPath string) runlock.State {
	if abs, err := filepath.Abs(certPath); err == nil {
		certPath = abs
	}
	return runlock.State{PID: pid, MgmtAddr: mgmtAddr, CertPath: certPath}
}

// runLockState is the run-lock state recording this process, with the
// management API's endpoint when ep is non-nil (nil means no API is serving,
// so no API handler can rewrite the config file).
func runLockState(ep *mgmtEndpoint) runlock.State {
	pid := os.Getpid()
	if ep != nil {
		return lockState(pid, ep.addr, ep.certPath)
	}
	return runlock.State{PID: pid}
}

// runLockRetryDelay is how long runLockPublisher waits before rewriting a run
// lock whose last write failed.
const runLockRetryDelay = 30 * time.Second

// runLockKey keys the unwritable-run-lock condition in the notification center.
const runLockKey = "run-lock-unwritable"

// runLockPublisher writes the run lock for run() and for the management
// supervisor, which publishes from its own goroutine so the lock tracks each
// API transition (up, stopped, a background attempt) as it happens. The mutex
// keeps a write from interleaving with another.
//
// A failed write can leave the lock empty (Publish truncates before it
// writes), which the token commands read as "starting up" for as long as it
// stays that way. So the publisher remembers the state it wants published and,
// after a failure, rewrites the latest one every runLockRetryDelay until a
// write succeeds, logging the first failure and the recovery rather than every
// attempt. A nil *runLockPublisher is a no-op, for tests that drive the retry
// without a lock.
type runLockPublisher struct {
	mu      sync.Mutex
	lock    *runlock.Lock
	cfgPath string
	// write replaces lock.Publish when non-nil, for tests.
	write func(runlock.State) error
	// retryDelay replaces runLockRetryDelay when non-zero, for tests.
	retryDelay time.Duration
	// center, when non-nil, carries the unwritable-lock condition: raised on
	// the first failure of a streak and cleared once a write succeeds.
	center notify.Publisher

	want    runlock.State // the latest state asked for
	failing bool          // the last write failed; a retry is pending
	retry   *time.Timer
	stopped bool
}

// publish records this process in the run lock, with the management API's
// endpoint when ep is non-nil (see runLockState).
func (p *runLockPublisher) publish(ep *mgmtEndpoint) {
	p.set(runLockState(ep))
}

// starting marks the appliance as starting up in the run lock, while a
// background attempt reloads the config file and brings the API up. A token
// command reading the lock then asks the operator to try again rather than
// editing a file the attempt may already have read, which the API it brings up
// would later overwrite.
func (p *runLockPublisher) starting() {
	p.set(runlock.State{})
}

// set makes st the state the lock should hold and writes it.
func (p *runLockPublisher) set(st runlock.State) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.want = st
	p.flush()
}

// flush writes p.want, arming a retry when the write fails. p.mu is held.
func (p *runLockPublisher) flush() {
	if p.stopped {
		return
	}
	if p.retry != nil {
		p.retry.Stop()
		p.retry = nil
	}
	write := p.write
	if write == nil {
		write = p.lock.Publish
	}
	err := write(p.want)
	if err == nil {
		if p.failing {
			log.Printf("run lock %s written again", runlock.PathFor(p.cfgPath))
			p.failing = false
			if p.center != nil {
				p.center.Clear(runLockKey, notify.Notification{
					Severity: notify.SeverityInfo,
					Title:    "Run lock writable again",
					Message:  "The token commands read this appliance's state correctly again",
				})
			}
		}
		return
	}
	delay := cmp.Or(p.retryDelay, runLockRetryDelay)
	if !p.failing {
		log.Printf("WARNING: cannot write run lock %s: %v (token commands may misread this appliance's state; retrying every %s)", runlock.PathFor(p.cfgPath), err, delay)
		p.failing = true
		if p.center != nil {
			p.center.Onset(notify.Notification{
				Severity: notify.SeverityWarning,
				Category: notify.CategorySystem,
				Key:      runLockKey,
				Source:   "runlock",
				Title:    "Run lock unwritable",
				Message:  fmt.Sprintf("Cannot write %s: %v. remote-mic token commands may misread this appliance's state until it can be written again (retrying every %s)", runlock.PathFor(p.cfgPath), err, delay),
			})
		}
	}
	p.retry = time.AfterFunc(delay, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.flush()
	})
}

// stop ends the publisher before run() releases the lock: no further write,
// and no pending retry.
func (p *runLockPublisher) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.retry != nil {
		p.retry.Stop()
		p.retry = nil
	}
}

// acquireRunLock takes the process-lifetime run lock beside cfgPath. Holding it
// keeps a second appliance off the same config and tells the token commands this
// config is being served (and, once Publish runs, where the management API
// listens). A token command holds the lock only for the moment of a file edit,
// which the short wait absorbs. Only a lock already held by another appliance is
// fatal; any other lock failure is logged and returns a nil lock, so the
// appliance still serves (the token commands then cannot detect it and would
// edit the config file directly).
func acquireRunLock(cfgPath string) (*runlock.Lock, error) {
	lockPath := runlock.PathFor(cfgPath)
	lock, err := runlock.Acquire(lockPath, 2*time.Second)
	switch {
	case errors.Is(err, runlock.ErrHeld):
		return nil, fmt.Errorf("another appliance is already running with %s (lock %s held)", cfgPath, lockPath)
	case err != nil:
		log.Printf("WARNING: cannot create run lock %s: %v (token commands will not detect this running appliance)", lockPath, err)
		return nil, nil //nolint:nilnil // a nil lock with no error is the deliberate "serve without a lock" signal
	}
	return lock, nil
}

func run(cfgPath string, ov serveOverrides, check bool, pprofAddr string) error {
	startTime := time.Now()

	// Take the run lock before loading the config (serve only, never --check).
	// Loading first would let a token command edit the file in the gap between
	// the load and the moment this appliance announces itself through the lock,
	// so the appliance would run the pre-edit token and a later web UI save would
	// revert the operator's change. --check does no work under the lock: it is a
	// systemd ExecStartPre and must not fence a starting appliance.
	var lock *runlock.Lock
	if !check {
		l, err := acquireRunLock(cfgPath)
		if err != nil {
			return err
		}
		lock = l
		defer func() { _ = lock.Release() }()
	}

	// First run with no config file: LoadOrDefault boots with defaults and no
	// devices so the web UI comes up and the operator can enumerate the host's
	// capture hardware and enable devices from there; the first provisioning
	// writes the config file at cfgPath. The stat only surfaces that operator
	// hint: the load-or-default decision itself lives in config.LoadOrDefault, so
	// serve and the token commands share one first-run path.
	if _, statErr := os.Stat(cfgPath); errors.Is(statErr, os.ErrNotExist) {
		log.Printf("no config file at %s; starting with defaults (enable capture devices from the web UI)", cfgPath)
	}
	cfg, err := config.LoadOrDefault(cfgPath)
	if err != nil {
		return err
	}

	// cfg drives this run's pipeline and listeners with the serve overrides
	// applied; storeCfg is the override-free config that seeds the persistence
	// store, so a later PATCH cannot bake an ephemeral override into config.yaml
	// (issue #29). See splitServeConfig.
	cfg, storeCfg := splitServeConfig(&cfg, ov)

	// --check validates the config and reports device presence, then exits without
	// binding ports or opening capture (for systemd ExecStartPre).
	if check {
		return reportCheck(&cfg, os.Stdout)
	}

	// Re-validate after applying overrides so an invalid override (a bad
	// --listen or --mgmt-listen) fails fast with a clear config error rather than
	// a late listener bind failure. config.Load already validated the file, but
	// the override mutation happens after that.
	if err := cfg.Validate(); err != nil {
		return err
	}

	mgmtEnabled := cfg.ManagementEnabled()

	// The level hub taps every device's capture pump and streams per-device
	// audio levels over SSE. It is created before devices open so a meter can be
	// registered as each device is wrapped.
	hub := levels.NewHub()

	// The notification center holds the appliance's in-memory event history and
	// active conditions and streams them over SSE beside levels. The startup
	// entry is published before the management API comes up, so it is already in
	// the first snapshot a client fetches. The device, stream, and config
	// emitters and the signal and host condition monitors publish to it.
	center := notify.NewCenter()
	center.Publish(notify.Started(version))

	prov := &provider{
		version:     version,
		start:       startTime,
		rtspListen:  cfg.Listen,
		dataPath:    filepath.Dir(cfgPath),
		enumTrigger: make(chan struct{}, 1),
		hwChanged:   make(chan struct{}, 1),
	}
	prov.setDiscovery(cfg.DiscoveryEnabled())
	prov.setAuthRequired(cfg.AuthRequired())
	// Snapshot which config fields a serve CLI flag overrode for this run, so the
	// web UI can explain why the config view (persisted values) diverges from the
	// running listeners and discovery state (effective values). cfg is the running
	// config and storeCfg the override-free persisted config (see splitServeConfig).
	prov.overrides = buildOverrides(&cfg, &storeCfg, ov)

	// One shared access token gates the RTSP stream (Digest) and the management
	// API and web UI (Bearer). The guard is consulted per request and swapped by
	// reconcile, so a token set or rotated through PATCH /config applies live.
	guard := auth.NewGuard(cfg.Auth.Token)
	switch {
	case cfg.AuthRequired():
		log.Print("access token required for the RTSP stream and the management API")
	case cfg.ManagementEnabled():
		log.Print("WARNING: the RTSP stream and the management API are OPEN to the network; set auth.token (or use the web UI's Access Control card) to require a token")
	default:
		log.Print("WARNING: the RTSP stream is OPEN to the network; set auth.token in the config to require a token")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Optional profiling endpoint, off unless the operator passes --pprof. Started
	// after the signal context exists so it shuts down with the appliance, and only
	// once the config validated. It exposes net/http/pprof to collect a CPU profile
	// under real load (to refresh the committed PGO profile) or debug a live
	// appliance; a bad address is non-fatal and only disables pprof.
	if pprofAddr != "" {
		startPprof(ctx, pprofAddr)
	}

	// The stream-events adapter turns RTSP client connect and disconnect
	// callbacks into notification-center entries, collapsing the churn of a
	// repeatedly reconnecting client into a single flapping warning.
	streamListener := newStreamEvents(center, nil)
	app := newAppliance(ctx, hub, rtspserver.New(rtspserver.Config{Listen: cfg.Listen, Auth: guard, Listener: streamListener}), prov, guard, center)

	// reconcileCh carries a runtime config reload from an API handler goroutine to
	// the run loop, which owns the pipeline. The reloader closure handed to the
	// management API blocks the PATCH handler until the loop applies the change,
	// but bails out if the appliance is shutting down or the client disconnects,
	// so a config patch never hangs.
	reconcileCh := make(chan reconcileReq)
	reloader := newServeReloader(ctx, reconcileCh, ov)

	// Start the management API before the device-open phase so status and
	// diagnostics are reachable even if every device fails to open. It reports
	// zero devices until setDevices publishes the records below. The handle's
	// serving method reports whether an API serves right now (a cert or listener
	// failure, at start or at runtime, leaves none until its supervisor brings
	// one back), so a configured-but-dead API is not mistaken for a live
	// diagnostic surface. The combined shutdown defer cancels ctx first (so the
	// API's shutdown goroutine fires even when run() returns on an error, not a
	// signal) and then drains in-flight API connections before the process exits.
	var management *mgmt
	runLock := &runLockPublisher{lock: lock, cfgPath: cfgPath, center: center}
	defer runLock.stop()
	// With management disabled, publish this process with no API: the token
	// commands edit the file directly, and no API handler can rewrite it. With
	// management enabled, startManagement publishes the first state itself
	// (where its API listens, or that none serves) before its supervisor can
	// publish, so the order is structural; until then the lock reads as
	// starting, so a token command asks the operator to retry instead of
	// editing a file the API is about to be seeded from.
	if !mgmtEnabled {
		runLock.publish(nil)
	}
	// GET /system reports host CPU utilization from a gauge that reads /proc/stat
	// only when a request asks, so an appliance with no browser open does no
	// sampling work at all. It exists only while the management API is enabled (its
	// sole consumer; the host monitor diffs /proc/stat over its own poll window in
	// hostReader.cpu). Collect tolerates a nil gauge and omits CPUPercent.
	if mgmtEnabled {
		prov.cpu = sysinfo.NewCPUGauge()
		management, _ = startManagement(ctx, &mgmtParams{
			cfgPath:   cfgPath,
			cfg:       &cfg,
			storeCfg:  &storeCfg,
			overrides: ov,
			prov:      prov,
			events:    sse.Handler(hub, center),
			center:    center,
			restartFn: stop,
			reloader:  reloader,
			guard:     guard,
			runLock:   runLock,
		})
	}
	defer func() {
		stop()
		management.Wait()
	}()

	// Drive the level sampler for the lifetime of the process.
	go hub.Run(ctx)

	// Start the condition monitors. The signal monitor taps the level hub (so it
	// must come up after the hub is running) and raises stuck-at-zero, very-quiet,
	// and clipping conditions per device; the host monitor polls CPU, memory,
	// temperature, disk, undervoltage, and each device's dropped-frame rate.
	// Handing both to the appliance as its Monitors makes every reconcile re-arm
	// them with the current thresholds and per-device quiet opt-outs, without
	// restarting any device.
	monSettings := monitor.SettingsFrom(&cfg)
	// Log once at startup, only when the monitors are enabled, if this host has no
	// readable rpi_volt hwmon, so an operator knows undervoltage alerts are
	// unavailable here. Skipping the probe when notifications are off avoids
	// warning about a sensor nothing will read.
	if monSettings.Enabled {
		logUndervoltageSupport()
	}
	app.monitors = buildMonitors(ctx, hub, prov.dataPath, prov.dropCounters, center, &monSettings)

	// Sweep stale client-flap warnings: the connect-driven detector only clears on
	// the next connect after the quiet window, which a client that settles into a
	// steady connection or gives up never sends, so a periodic sweep ages the
	// warning out (and drops the path when its device is removed).
	go streamListener.Run(ctx)

	// Build the initial pipeline by reconciling from an empty state to the loaded
	// config: this opens every enabled device, records disabled and skipped ones,
	// and starts the mDNS advertisement, using the very same path a later hot
	// reload takes. A device that fails to open is skipped, not fatal.
	app.reconcile(&cfg)
	defer app.closeAll()

	// Enumerate the host's unconfigured capture hardware for GET /devices/available
	// on a background goroutine: probing opens devices and can be slow, so it must
	// not run on the capture run loop. It starts after the initial reconcile so the
	// first probe already knows which devices the config owns and skips them.
	go prov.runEnumeration(ctx)

	// While the management API is serving, the appliance stays up as a diagnostic
	// surface even when nothing is serving (issue #10): GET /devices still reports
	// every skipped device and its open error, and a hot reload can bring devices
	// up later. When the API is not serving (management disabled, or it failed to
	// start or stopped and its supervisor has not brought it back) there is
	// nothing to keep alive, so a total open failure is fatal and lets systemd
	// restart the process (see startupExit). At runtime an API that dies is
	// retried in process, and while that retry runs it keeps the appliance up
	// as a serving API does (mgmt.keepsUp): exiting would throw away the
	// RAM-only event history and every device's retry state for what the retry
	// recovers. The run loop retakes its exit decision whenever it can turn
	// against staying up: when a pump ends, and when the API retry gives up
	// (see runExit); only then, with no pump alive, does it exit.
	if err := startupExit(app.serving(), management.serving() != nil, app.allDisabled(), mgmtEnabled); err != nil {
		return err
	}

	srvErr := make(chan error, 1)
	go func() { srvErr <- app.srv.ListenAndServe(ctx) }()
	log.Printf("serving %d device(s) on %s", app.serving(), cfg.Listen)

	// shutdown logs and publishes the best-effort shutdown entry, so a client that
	// stays connected to the notification stream through the drain sees why the
	// stream ends. The ring is cleared on the next boot, so this is the last entry
	// of the run. It runs on either exit path a signal can take: the ctx.Done case,
	// and the srvErr case, because a cancelled ctx makes ListenAndServe close its
	// listener and return nil, so both cases become ready at once and select may
	// pick srvErr; without publishing here that race would drop the entry.
	shutdown := func() {
		log.Print("shutting down")
		center.Publish(notify.Notification{
			Severity: notify.SeverityInfo,
			Category: notify.CategorySystem,
			Kind:     notify.KindEvent,
			Title:    "Shutting down",
			Message:  "Appliance shutting down",
		})
	}

	// The run loop owns the pipeline. It applies config reloads from the API,
	// retires devices that die (their track 404s while the rest keep serving), and
	// exits on shutdown or a fatal server error. While the API is serving the
	// process stays up even after the last pump stops, so a degraded state stays
	// inspectable until a signal arrives.
	for {
		select {
		case <-ctx.Done():
			shutdown()
			return nil
		case req := <-reconcileCh:
			app.reconcile(&req.cfg)
			req.reply <- nil
		case <-prov.hwChanged:
			app.retryDown()
		case <-app.retryDue:
			app.onRetryDue()
		case res := <-app.pumpDone:
			app.onPumpDone(res)
			if exit, err := runExit(app.alive, management.keepsUp(), app.lastPumpErr, false); exit {
				return err
			}
		case <-management.lostC():
			switch exit, stopping, err := lostExit(ctx.Err() != nil, app.alive, management.keepsUp(), app.lastPumpErr); {
			case stopping:
				shutdown()
				return nil
			case exit:
				return err
			}
		case serr := <-srvErr:
			if serr != nil {
				return fmt.Errorf("rtsp server: %w", serr)
			}
			// A nil error means ListenAndServe returned only because ctx was
			// cancelled (its Accept loop returns nil solely on a cancelled listener),
			// so this is a shutdown that raced ahead of the ctx.Done case above.
			shutdown()
			return nil
		}
	}
}

// startupExit is run()'s exit decision after the initial reconcile: nil keeps
// the appliance up, an error ends it. With a device serving, or with an API
// serving as a diagnostic surface, it stays up. Otherwise a deliberate
// all-disabled config is reported distinctly, since a restart cannot clear it,
// and a total open failure names the management failure too when management is
// enabled: the API's retry was just logged as running in the background, but
// exiting ends it, so the log must not read as if only the devices were at
// fault.
func startupExit(serving int, apiServing, allDisabled, mgmtEnabled bool) error {
	switch {
	case serving > 0 || apiServing:
		return nil
	case allDisabled:
		return errors.New("all configured capture devices are disabled; enable at least one device, or enable the management API to keep the appliance up as a diagnostic surface")
	case mgmtEnabled:
		return errors.New("no configured capture device could be opened, and the management API that would keep the appliance up could not start (see its error above)")
	default:
		return errors.New("no configured capture device could be opened")
	}
}

// runExit is the run loop's exit decision, taken when a pump ends and, with
// apiLost, when the management API retry gave up. The appliance stays up
// while a capture pump is alive or the API keeps it up (it serves or is being
// retried, see mgmt.keepsUp); otherwise nothing keeps it up, and exiting lets
// systemd restart it. An appliance whose last pump ended exits cleanly unless
// a pump failed; one that lost its API names that, since the API was what
// kept it up.
func runExit(alive int, apiUp bool, lastPumpErr error, apiLost bool) (exit bool, err error) {
	switch {
	case alive > 0 || apiUp:
		return false, nil
	case apiLost && lastPumpErr != nil:
		return true, fmt.Errorf("no capture device is serving and the management API that kept the appliance up is no longer retried, last device error: %w", lastPumpErr)
	case apiLost:
		return true, errors.New("no capture device is serving and the management API that kept the appliance up is no longer retried")
	case lastPumpErr != nil:
		return true, fmt.Errorf("all capture devices stopped, last error: %w", lastPumpErr)
	default:
		return true, nil
	}
}

// lostExit is the run loop's decision when the management API retry gave up.
// A shutdown can make that wake and ctx.Done ready together, and select may
// pick the wake; with cancelled set it is a shutdown (stopping), not a lost
// API, so the process ends cleanly. Otherwise it is runExit's decision.
func lostExit(cancelled bool, alive int, apiUp bool, lastPumpErr error) (exit, stopping bool, err error) {
	if cancelled {
		return true, true, nil
	}
	exit, err = runExit(alive, apiUp, lastPumpErr, true)
	return exit, false, err
}

// splitServeConfig derives the two configs a serve run needs from the loaded
// config: running drives this run's pipeline and listeners with the serve
// overrides applied, and store is an override-free deep copy that seeds the
// persistence store. The serve overrides (--listen, --mgmt-listen, --cert-dir,
// --discovery, --management) are ephemeral for the run: seeding the store from
// the pre-override snapshot keeps them out of config.yaml, so a later PATCH
// /config that saves the whole config cannot bake them in (issue #29).
// newServeReloader re-applies the overrides to the live pipeline on every hot
// reload, so the running RTSP port and discovery state stay overridden without
// ever being persisted. Snapshotting before applyServeOverrides is the crux:
// taken after, store would carry the overrides and the fix would not hold.
func splitServeConfig(loaded *config.Config, ov serveOverrides) (running, store config.Config) {
	store = loaded.Clone()
	running = loaded.Clone()
	applyServeOverrides(&running, ov)
	return running, store
}

// buildOverrides reports the config fields a serve CLI flag overrode for this
// run, comparing the running config against the override-free persisted config.
// Only a flag that was actually given AND changed the value yields an entry, so
// a flag set to the same value the config file already holds lights nothing. The
// booleans are rendered with strconv.FormatBool so effective and persisted share
// one string type the UI renders directly.
func buildOverrides(running, store *config.Config, ov serveOverrides) []mgmtserver.ConfigOverride {
	var out []mgmtserver.ConfigOverride
	add := func(set bool, field, effective, persisted string) {
		if set && effective != persisted {
			out = append(out, mgmtserver.ConfigOverride{Field: field, Effective: effective, Persisted: persisted})
		}
	}
	add(ov.set["listen"], "listen", running.Listen, store.Listen)
	add(ov.set["mgmt-listen"], "management.listen", running.Management.Listen, store.Management.Listen)
	add(ov.set["cert-dir"], "management.certDir", running.Management.CertDir, store.Management.CertDir)
	add(ov.set["management"], "management.enabled",
		strconv.FormatBool(running.ManagementEnabled()), strconv.FormatBool(store.ManagementEnabled()))
	add(ov.set["discovery"], "discovery.enabled",
		strconv.FormatBool(running.DiscoveryEnabled()), strconv.FormatBool(store.DiscoveryEnabled()))
	return out
}

// newServeReloader builds the Reloader the management API calls to hot-apply a
// persisted PATCH /config to the running pipeline. The persisted config the
// store hands back is free of the serve override flags (they are ephemeral for
// the run, issue #29), so this re-applies them to the copy that drives the LIVE
// pipeline: a hot reload then keeps advertising the overridden RTSP port over
// mDNS and honors a --discovery override, while the on-disk config stays clean.
// The store already saved that clean config before this runs, so the re-applied
// overrides never reach disk. It blocks until the run loop applies the change
// but bails out on appliance shutdown (ctx) or client disconnect (reqCtx), so a
// patch never hangs.
func newServeReloader(ctx context.Context, reconcileCh chan<- reconcileReq, ov serveOverrides) mgmtserver.Reloader {
	return func(reqCtx context.Context, newCfg config.Config) error {
		applyServeOverrides(&newCfg, ov)
		reply := make(chan error, 1)
		select {
		case reconcileCh <- reconcileReq{cfg: newCfg, reply: reply}:
		case <-ctx.Done():
			return errors.New("appliance is shutting down")
		case <-reqCtx.Done():
			return reqCtx.Err()
		}
		select {
		case err := <-reply:
			return err
		case <-ctx.Done():
			return errors.New("appliance is shutting down")
		case <-reqCtx.Done():
			return reqCtx.Err()
		}
	}
}

// startAnnounce advertises every serving device over mDNS in the background.
// Failure is logged, not fatal: the appliance still serves on a
// multicast-blocked network, where the manual host:port entry is the fallback.
// A device that dies later keeps its advertisement until the advertisement is
// next rebuilt (see appliance.restartAnnounce; dnssd cannot retire a single
// service); clients get 404 meanwhile. It is a package var
// so reconcile tests can swap in a stub and assert announceGen without a real
// responder multicasting on the test host's LAN.
var startAnnounce = func(ctx context.Context, listen string, devices []*deviceRuntime, authRequired bool) {
	infos, port, err := announceInfos(listen, devices, authRequired)
	if err != nil {
		log.Printf("mDNS disabled: %v", err)
		return
	}
	go func() {
		if aerr := announce.Run(ctx, infos); aerr != nil {
			log.Printf("mDNS advertisement stopped: %v (serving continues without discovery)", aerr)
		}
	}()
	log.Printf("advertising %d service(s) over mDNS (_rtsp._tcp) on port %d", len(infos), port)
}

// announceInfos builds the per-stream advertisement records for the serving set
// on the RTSP listen port, carrying the auth hint (auth=token or auth=none) so
// BirdNET-Go's adopt flow knows whether to ask for the token. A multi-stream
// device advertises one service per stream, each at its own path; the DNS-SD
// instance name stays the device name for a lone stream (unchanged) and is
// qualified with the stream path when a device fans out. Each name is fitted
// to one DNS label with room for the responder's rename on a conflict with
// another host (see instanceLabel). Names here can still collide (a different
// device literally named "<name> <path>", or two names that agree up to the
// cut); announce.Run keeps local duplicates apart with a " #N" suffix.
func announceInfos(listen string, devices []*deviceRuntime, authRequired bool) ([]announce.Info, int, error) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot parse listen address %q: %w", listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, 0, fmt.Errorf("bad port in %q: %w", listen, err)
	}
	var infos []announce.Info
	for _, rt := range devices {
		multi := len(rt.streams) > 1
		for _, sr := range rt.streams {
			var suffix string
			if multi {
				suffix = " " + strings.TrimPrefix(sr.stream.Path, "/")
			}
			infos = append(infos, announce.Info{
				Name:         instanceLabel(rt.dev.Name, suffix),
				Path:         sr.stream.Path,
				Port:         port,
				Codec:        pipeline.CodecName(sr.stream.Mode),
				Rate:         rt.rate,
				Channels:     len(sr.stream.Channels),
				Version:      version,
				AuthRequired: authRequired,
			})
		}
	}
	return infos, port, nil
}

// instanceLabel builds an advertised instance name from a device name and a
// suffix (empty for a lone stream, " <path>" for a fanned-out device) that
// fits announce.NameBudget. The config accepts device names longer than a label
// holds, so it cuts the device name, not the suffix: the suffix is what keeps
// a device's streams apart. A suffix that fills the budget leaves no room for
// the device name, and one that exceeds it is cut too; a device name cut to
// nothing (including one whose first rune does not fit) also drops the
// suffix's leading space. Cuts fall on a rune boundary, and a space left at a
// cut is trimmed. A name that fits is returned unchanged.
func instanceLabel(name, suffix string) string {
	head := announce.CutName(name, announce.NameBudget-len(suffix))
	if head == "" {
		suffix = strings.TrimLeft(suffix, " ")
	}
	return announce.CutName(head+suffix, announce.NameBudget)
}

// fanoutStreams describes each stream's fan-out consumer: its drop counter,
// shared with the stream's downstream frame drops, and its feed's play session,
// so the fan-out sends an idle stream nothing instead of copied audio and tags
// each period it sends with the session it was sent for.
func fanoutStreams(streams []*streamRuntime) []audio.FanoutStream {
	out := make([]audio.FanoutStream, len(streams))
	for i, sr := range streams {
		out[i] = audio.FanoutStream{Dropped: &sr.dropped, Gate: sr.frames.Session}
	}
	return out
}

// buildStage builds one stream's pipeline stage and its RTP payload type. The
// stage takes the stream's channel count from its selecting source.
func buildStage(s *config.Stream) (stage pipeline.Stage, payloadType int) {
	if s.Mode == config.ModeOpus {
		return pipeline.NewOpus(s.Opus), pipeline.PayloadType(s.Mode)
	}
	return pipeline.NewPCM(), pipeline.PayloadType(s.Mode)
}
