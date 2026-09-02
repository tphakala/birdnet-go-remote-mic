//go:build linux

// Command birdnet-go-remote-mic is the remote microphone appliance: it captures
// local audio and serves it over RTSP/RTP for BirdNET-Go to pull.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
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

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to the YAML config file")
	listDevices := flag.Bool("list-devices", false, "list capture devices and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("birdnet-go-remote-mic", version)
	case *listDevices:
		if err := printDevices(); err != nil {
			fatal(err)
		}
	default:
		if err := run(*cfgPath); err != nil {
			fatal(err)
		}
	}
}

func printDevices() error {
	devs, err := capture.Devices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		fmt.Printf("%-12s %s\n", d.ID, d.Name)
	}
	return nil
}

// deviceRuntime bundles one configured device's moving parts. A device that
// failed to open keeps a record with src and track nil so the management API can
// still report it (state skipped). src and track are set only when it opened.
type deviceRuntime struct {
	dev      config.Device
	src      audio.Source
	stage    pipeline.Stage
	frames   *rtspserver.ChanSource
	track    *rtspserver.Track
	rate     int
	channels int
	// friendlyName is the sound card's human label; supportedRates and
	// supportedChannels are the rate and channel-count sets the device accepted at
	// the startup probe. All static per run and read without a lock.
	friendlyName      string
	supportedRates    []int
	supportedChannels []int
	dropped           atomic.Uint64

	mu    sync.Mutex
	state mgmtserver.DeviceState
	err   string

	// superseded marks a device the reconcile loop deliberately stopped (a
	// hot-reload stop or restart). Its pump still delivers a final pumpResult;
	// the loop reads this to skip the spontaneous-death cleanup, so a device
	// restarted on the same RTSP path is not torn down by the old pump's exit.
	// Set and read only on the run-loop goroutine.
	superseded bool
}

// openDevice opens and starts capture for one configured device at the
// resolved hardware channel count openCh and builds its pipeline stage, SDP, and
// RTSP track. The capture source is wrapped so every period also feeds the
// device's level meter, which runs on the capture pump regardless of whether an
// RTSP client is connected.
func openDevice(dev *config.Device, openCh int, hub *levels.Hub) (*deviceRuntime, error) {
	src, err := audio.OpenCaptureAt(dev, openCh)
	if err != nil {
		return nil, fmt.Errorf("open capture: %w", err)
	}
	rate, channels := src.Negotiated()
	stage, payloadType := buildStage(dev, channels)
	sdpBytes, err := sdp.WriteSession(pipeline.SDPSpec(dev, rate, channels))
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("build sdp: %w", err)
	}
	// Register the level meter only once the device has fully opened. Doing it
	// after the last fallible step keeps a device that fails here out of the
	// hub, so the levels stream never reports a phantom silent device for it.
	src = audio.NewMeteredSource(src, hub.Meter(dev.Name, channels))
	frames := rtspserver.NewChanSource(64)
	return &deviceRuntime{
		dev:      *dev,
		src:      src,
		stage:    stage,
		frames:   frames,
		track:    &rtspserver.Track{Path: dev.Path, SDP: sdpBytes, PayloadType: payloadType, Frames: frames},
		rate:     rate,
		channels: channels,
	}, nil
}

// openDeviceRetry opens a device, retrying a few times on failure. The retry
// matters most on a hot-reload restart: ALSA is single-client and the kernel may
// not release a hw device the instant Close returns, so an immediate reopen of
// the same card can transiently fail with EBUSY. A handful of short retries rides
// that out; a device that still will not open is reported skipped, not dropped.
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
		openCh := resolveOpenChannels(dev.Device, dev.Channels)
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
		}
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return nil, err
}

func run(cfgPath string) error {
	startTime := time.Now()
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		// First run with no config file: boot with defaults and no devices so the
		// web UI comes up and the operator can enumerate the host's capture
		// hardware and enable devices from there. The first provisioning writes the
		// config file at cfgPath.
		log.Printf("no config file at %s; starting with defaults (enable capture devices from the web UI)", cfgPath)
		cfg, err = config.Default(), nil
	}
	if err != nil {
		return err
	}

	mgmtEnabled := cfg.ManagementEnabled()

	// The level hub taps every device's capture pump and streams per-device
	// audio levels over SSE. It is created before devices open so a meter can be
	// registered as each device is wrapped.
	hub := levels.NewHub()

	prov := &provider{
		version:     version,
		start:       startTime,
		rtspListen:  cfg.Listen,
		dataPath:    filepath.Dir(cfgPath),
		enumTrigger: make(chan struct{}, 1),
	}
	prov.setDiscovery(cfg.DiscoveryEnabled())
	prov.setAuthRequired(cfg.AuthRequired())

	// One shared access token gates the RTSP stream (Digest) and the management
	// API and web UI (Bearer). The guard is consulted per request and swapped by
	// reconcile, so a token set or rotated through PATCH /config applies live.
	guard := auth.NewGuard(cfg.Auth.Token)
	if cfg.AuthRequired() {
		log.Print("access token required for the RTSP stream and the management API")
	} else {
		log.Print("WARNING: the RTSP stream and the management API are OPEN to the network; set auth.token (or use the web UI's Access Control card) to require a token")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app := newAppliance(ctx, hub, rtspserver.New(rtspserver.Config{Listen: cfg.Listen, Auth: guard}), prov, guard)

	// reconcileCh carries a runtime config reload from an API handler goroutine to
	// the run loop, which owns the pipeline. The reloader closure handed to the
	// management API blocks the PATCH handler until the loop applies the change,
	// but bails out if the appliance is shutting down or the client disconnects,
	// so a config patch never hangs.
	reconcileCh := make(chan reconcileReq)
	reloader := func(reqCtx context.Context, newCfg config.Config) error {
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

	// Start the management API before the device-open phase so status and
	// diagnostics are reachable even if every device fails to open. It reports
	// zero devices until setDevices publishes the records below. mgmtServing
	// tracks whether the API actually came up (a cert or listener failure leaves
	// it false), so a configured-but-dead API is not mistaken for a live
	// diagnostic surface. The combined shutdown defer cancels ctx first (so the
	// API's shutdown goroutine fires even when run() returns on an error, not a
	// signal) and then drains in-flight API connections before the process exits.
	var management *mgmt
	mgmtServing := false
	if mgmtEnabled {
		// Sample host CPU utilization for GET /system only while the API serves.
		prov.sampler = sysinfo.NewSampler(ctx, 2*time.Second)
		management, mgmtServing = startManagement(ctx, cfgPath, &cfg, prov, hub.EventsHandler(), stop, reloader, guard)
	}
	defer func() {
		stop()
		management.Wait()
	}()

	// Drive the level sampler for the lifetime of the process.
	go hub.Run(ctx)

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
	// up later. When the API is not serving (management disabled or it failed to
	// start) there is nothing to keep alive, so a total open failure is fatal and
	// lets a supervisor restart the process. A deliberate all-disabled config is
	// reported distinctly, since a restart cannot clear it.
	if app.serving() == 0 && !mgmtServing {
		if app.allDisabled() {
			return errors.New("all configured capture devices are disabled; enable at least one device, or enable the management API to keep the appliance up as a diagnostic surface")
		}
		return errors.New("no configured capture device could be opened")
	}

	srvErr := make(chan error, 1)
	go func() { srvErr <- app.srv.ListenAndServe(ctx) }()
	log.Printf("serving %d device(s) on %s", app.serving(), cfg.Listen)

	// The run loop owns the pipeline. It applies config reloads from the API,
	// retires devices that die (their track 404s while the rest keep serving), and
	// exits on shutdown or a fatal server error. While the API is serving the
	// process stays up even after the last pump stops, so a degraded state stays
	// inspectable until a signal arrives.
	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return nil
		case req := <-reconcileCh:
			app.reconcile(&req.cfg)
			req.reply <- nil
		case res := <-app.pumpDone:
			app.onPumpDone(res)
			if app.alive == 0 && !mgmtServing {
				if app.lastPumpErr != nil {
					return fmt.Errorf("all capture devices stopped, last error: %w", app.lastPumpErr)
				}
				return nil
			}
		case serr := <-srvErr:
			if serr != nil {
				return fmt.Errorf("rtsp server: %w", serr)
			}
			return nil
		}
	}
}

// startAnnounce advertises every serving device over mDNS in the background.
// Failure is logged, not fatal: the appliance still serves on a
// multicast-blocked network, where the manual host:port entry is the fallback.
// A device that dies later keeps its advertisement until the process exits
// (dnssd cannot retire a single service); clients get 404. It is a package var
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

// announceInfos builds the per-device advertisement records for the serving
// set on the RTSP listen port, carrying the auth hint (auth=token or auth=none)
// so BirdNET-Go's adopt flow knows whether to ask for the token.
func announceInfos(listen string, devices []*deviceRuntime, authRequired bool) ([]announce.Info, int, error) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot parse listen address %q: %w", listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, 0, fmt.Errorf("bad port in %q: %w", listen, err)
	}
	infos := make([]announce.Info, 0, len(devices))
	for _, rt := range devices {
		infos = append(infos, announce.Info{
			Name:         rt.dev.Name,
			Path:         rt.dev.Path,
			Port:         port,
			Codec:        pipeline.CodecName(rt.dev.Mode),
			Rate:         rt.rate,
			Channels:     rt.channels,
			Version:      version,
			AuthRequired: authRequired,
		})
	}
	return infos, port, nil
}

func buildStage(d *config.Device, channels int) (stage pipeline.Stage, payloadType int) {
	if d.Mode == config.ModeOpus {
		return pipeline.NewOpus(d.Opus), pipeline.PayloadType(d.Mode)
	}
	return pipeline.NewPCM(channels), pipeline.PayloadType(d.Mode)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "birdnet-go-remote-mic:", err)
	os.Exit(1)
}
