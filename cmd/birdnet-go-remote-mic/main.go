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
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	capture "github.com/tphakala/go-audio-capture"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"

	"github.com/tphakala/birdnet-go-remote-mic/internal/announce"
	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/sysinfo"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

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
	// friendlyName is the sound card's human label; supportedRates is the set of
	// rates the device accepted at the startup probe. Both are static per run and
	// read without a lock.
	friendlyName   string
	supportedRates []int
	dropped        atomic.Uint64

	mu    sync.Mutex
	state mgmtserver.DeviceState
	err   string
}

// openDevice opens and starts capture for one configured device and builds its
// pipeline stage, SDP, and RTSP track. The capture source is wrapped so every
// period also feeds the device's level meter, which runs on the capture pump
// regardless of whether an RTSP client is connected.
func openDevice(dev *config.Device, hub *levels.Hub) (*deviceRuntime, error) {
	src, err := audio.OpenCapture(dev)
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
	src = audio.NewMeteredSource(src, hub.Meter(dev.Name))
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

func run(cfgPath string) error {
	startTime := time.Now()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	mgmtEnabled := cfg.ManagementEnabled()

	// The level hub taps every device's capture pump and streams per-device
	// audio levels over SSE. It is created before devices open so a meter can be
	// registered as each device is wrapped.
	hub := levels.NewHub()

	prov := &provider{
		version:    version,
		start:      startTime,
		rtspListen: cfg.Listen,
		discovery:  cfg.DiscoveryEnabled(),
		dataPath:   filepath.Dir(cfgPath),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		management, mgmtServing = startManagement(ctx, cfgPath, &cfg, prov, hub.EventsHandler(), stop)
	}
	defer func() {
		stop()
		management.Wait()
	}()

	// A device that fails to open is skipped, not fatal: the appliance keeps
	// serving whatever hardware is actually present. Every configured device
	// keeps a record (records) so the management API can report skipped ones;
	// serving holds only the ones that opened and need pumping.
	// Enumerate the host's capture hardware once so each device can carry a
	// friendly label (defaulting a blank name in the UI). A failure here is not
	// fatal: devices simply carry no label.
	hwNames, nerr := audio.HardwareNames()
	if nerr != nil {
		log.Printf("enumerate capture hardware: %v", nerr)
	}

	records := make([]*deviceRuntime, 0, len(cfg.Devices))
	serving := make([]*deviceRuntime, 0, len(cfg.Devices))
	for i := range cfg.Devices {
		dev := &cfg.Devices[i]
		// Probe supported rates while the device is still free (hw: devices are
		// exclusive, so this must happen before the real open below).
		rates := audio.ProbeRates(dev.Device, dev.Channels, audio.CandidateRates())
		friendly := hwNames[dev.Device]

		rt, oerr := openDevice(dev, hub)
		if oerr != nil {
			log.Printf("skipping device %q (%s): %v", dev.Name, dev.Device, oerr)
			records = append(records, &deviceRuntime{
				dev:            *dev,
				state:          mgmtserver.StateSkipped,
				err:            oerr.Error(),
				friendlyName:   friendly,
				supportedRates: rates,
			})
			continue
		}
		rt.state = mgmtserver.StateServing
		rt.friendlyName = friendly
		rt.supportedRates = rates
		log.Printf("capture %q: %d Hz, %d ch on %s serving %s", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, rt.dev.Path)
		records = append(records, rt)
		serving = append(serving, rt)
	}
	prov.setDevices(records)

	// While the management API is serving, the appliance stays up as a diagnostic
	// surface even when nothing is serving (issue #10): GET /devices still reports
	// every skipped device and its open error. When the API is not serving (either
	// management is disabled or it failed to start) there is nothing to keep alive,
	// so a total open failure is fatal and lets a supervisor restart the process
	// (the current auto-recovery path; in-process capture restart is a later phase).
	if len(serving) == 0 && !mgmtServing {
		return errors.New("no configured capture device could be opened")
	}

	defer func() {
		for _, rt := range serving {
			_ = rt.src.Close()
		}
	}()

	// Drive the level sampler for the lifetime of the process.
	go hub.Run(ctx)

	tracks := make([]*rtspserver.Track, len(serving))
	for i, rt := range serving {
		tracks[i] = rt.track
	}
	srv := rtspserver.New(rtspserver.Config{Listen: cfg.Listen}, tracks...)

	// One capture/pipeline pump per device: each sound card is its own clock.
	// Lock each pump to its OS thread so the capture loop is not descheduled
	// mid-period.
	type pumpResult struct {
		rt  *deviceRuntime
		err error
	}
	pumpDone := make(chan pumpResult, len(serving))
	for _, rt := range serving {
		go func(rt *deviceRuntime) {
			runtime.LockOSThread()
			perr := rt.stage.Run(rt.src, func(f pipeline.Frame) error {
				if !rt.frames.Push(f) {
					drops := rt.dropped.Add(1)
					if drops%50 == 1 {
						log.Printf("%s: dropping frames: the client is not keeping up (total drops: %d)", rt.dev.Name, drops)
					}
				}
				return ctx.Err()
			})
			pumpDone <- pumpResult{rt: rt, err: perr}
		}(rt)
	}

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe(ctx) }()
	log.Printf("serving %d device(s) on %s", len(serving), cfg.Listen)

	// mDNS advertises only actual streams; with nothing serving there is nothing
	// to announce (announce.Run rejects an empty set).
	if prov.discovery && len(serving) > 0 {
		startAnnounce(ctx, cfg.Listen, serving)
	}

	// A dead device is isolated: its track is retired (404) and everything else
	// keeps serving. While the management API is serving the process stays up even
	// after the last pump stops, so the degraded state remains inspectable until a
	// signal arrives; otherwise it exits once every pump has stopped.
	alive := len(serving)
	var lastPumpErr error
	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return nil
		case res := <-pumpDone:
			alive--
			srv.RemoveTrack(res.rt.track.Path)
			res.rt.frames.Close()
			_ = res.rt.src.Close() // release the ALSA stream now, not at process exit

			if res.err != nil && ctx.Err() == nil {
				lastPumpErr = res.err
				res.rt.markFailed(res.err)
				log.Printf("device %q failed: %v; %s returns 404 until restart", res.rt.dev.Name, res.err, res.rt.dev.Path)
			}
			if alive == 0 && !mgmtServing {
				if lastPumpErr != nil {
					return fmt.Errorf("all capture devices stopped, last error: %w", lastPumpErr)
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
// (dnssd cannot retire a single service); clients get 404.
func startAnnounce(ctx context.Context, listen string, devices []*deviceRuntime) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		log.Printf("mDNS disabled: cannot parse listen address %q: %v", listen, err)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Printf("mDNS disabled: bad port in %q: %v", listen, err)
		return
	}
	infos := make([]announce.Info, 0, len(devices))
	for _, rt := range devices {
		infos = append(infos, announce.Info{
			Name:     rt.dev.Name,
			Path:     rt.dev.Path,
			Port:     port,
			Codec:    pipeline.CodecName(rt.dev.Mode),
			Rate:     rt.rate,
			Channels: rt.channels,
			Version:  version,
		})
	}
	go func() {
		if aerr := announce.Run(ctx, infos); aerr != nil {
			log.Printf("mDNS advertisement stopped: %v (serving continues without discovery)", aerr)
		}
	}()
	log.Printf("advertising %d service(s) over mDNS (_rtsp._tcp) on port %d", len(infos), port)
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
