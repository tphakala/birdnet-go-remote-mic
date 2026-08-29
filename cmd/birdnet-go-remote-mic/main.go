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
	"runtime"
	"strconv"
	"syscall"

	capture "github.com/tphakala/go-audio-capture"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"

	"github.com/tphakala/birdnet-go-remote-mic/internal/announce"
	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
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

// deviceRuntime bundles one opened device's moving parts.
type deviceRuntime struct {
	dev      config.Device
	src      audio.Source
	stage    pipeline.Stage
	frames   *rtspserver.ChanSource
	track    *rtspserver.Track
	rate     int
	channels int
}

// openDevice opens and starts capture for one configured device and builds its
// pipeline stage, SDP, and RTSP track.
func openDevice(dev *config.Device) (*deviceRuntime, error) {
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// A device that fails to open is skipped, not fatal: the appliance keeps
	// serving whatever hardware is actually present.
	var devices []*deviceRuntime
	for i := range cfg.Devices {
		rt, oerr := openDevice(&cfg.Devices[i])
		if oerr != nil {
			log.Printf("skipping device %q (%s): %v", cfg.Devices[i].Name, cfg.Devices[i].Device, oerr)
			continue
		}
		log.Printf("capture %q: %d Hz, %d ch on %s serving %s", rt.dev.Name, rt.rate, rt.channels, rt.dev.Device, rt.dev.Path)
		devices = append(devices, rt)
	}
	if len(devices) == 0 {
		return errors.New("no configured capture device could be opened")
	}
	defer func() {
		for _, rt := range devices {
			_ = rt.src.Close()
		}
	}()

	tracks := make([]*rtspserver.Track, len(devices))
	for i, rt := range devices {
		tracks[i] = rt.track
	}
	srv := rtspserver.New(rtspserver.Config{Listen: cfg.Listen}, tracks...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One capture/pipeline pump per device: each sound card is its own clock.
	// Lock each pump to its OS thread so the capture loop is not descheduled
	// mid-period.
	type pumpResult struct {
		rt  *deviceRuntime
		err error
	}
	pumpDone := make(chan pumpResult, len(devices))
	for _, rt := range devices {
		go func(rt *deviceRuntime) {
			runtime.LockOSThread()
			var drops uint64
			perr := rt.stage.Run(rt.src, func(f pipeline.Frame) error {
				if !rt.frames.Push(f) {
					drops++
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
	log.Printf("serving %d device(s) on %s", len(devices), cfg.Listen)

	if cfg.DiscoveryEnabled() {
		startAnnounce(ctx, cfg.Listen, devices)
	}

	// A dead device is isolated: its track is retired (404) and everything
	// else keeps serving. The process exits when every pump has stopped.
	alive := len(devices)
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
			if res.err != nil && ctx.Err() == nil {
				lastPumpErr = res.err
				log.Printf("device %q failed: %v; %s returns 404 until restart", res.rt.dev.Name, res.err, res.rt.dev.Path)
			}
			if alive == 0 {
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
		codec := "L16"
		if rt.dev.Mode == config.ModeOpus {
			codec = "opus"
		}
		infos = append(infos, announce.Info{
			Name:     rt.dev.Name,
			Path:     rt.dev.Path,
			Port:     port,
			Codec:    codec,
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
		return pipeline.NewOpus(d.Opus), 97
	}
	return pipeline.NewPCM(channels), 96
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "birdnet-go-remote-mic:", err)
	os.Exit(1)
}
