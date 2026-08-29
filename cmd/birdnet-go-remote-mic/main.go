//go:build linux

// Command birdnet-go-remote-mic is the remote microphone appliance: it captures
// local audio and serves it over RTSP/RTP for BirdNET-Go to pull.
package main

import (
	"context"
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

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	src, err := audio.OpenCapture(cfg.Audio)
	if err != nil {
		return fmt.Errorf("open capture: %w", err)
	}
	defer func() { _ = src.Close() }()
	rate, channels := src.Negotiated()
	log.Printf("capture: %d Hz, %d ch on %s", rate, channels, cfg.Audio.Device)

	stage, payloadType := buildStage(&cfg, channels)
	sdpBytes, err := sdp.WriteSession(pipeline.SDPSpec(&cfg, rate, channels))
	if err != nil {
		return fmt.Errorf("build sdp: %w", err)
	}

	frames := rtspserver.NewChanSource(64)
	track := &rtspserver.Track{Path: "/stream", SDP: sdpBytes, PayloadType: payloadType, Frames: frames}
	srv := rtspserver.New(rtspserver.Config{Listen: cfg.Listen}, track)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Capture/pipeline pump: the sound card is the clock. Lock the goroutine to
	// its OS thread so the capture loop is not descheduled mid-period.
	pumpErr := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		var drops uint64
		pumpErr <- stage.Run(src, func(f pipeline.Frame) error {
			if !frames.Push(f) {
				drops++
				if drops%50 == 1 {
					log.Printf("dropping frames: the client is not keeping up (total drops: %d)", drops)
				}
			}
			return ctx.Err()
		})
	}()

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe(ctx) }()
	log.Printf("serving %q on %s (mode %s)", cfg.Name, cfg.Listen, cfg.Mode)

	if cfg.DiscoveryEnabled() {
		startAnnounce(ctx, &cfg, rate, channels)
	}

	select {
	case <-ctx.Done():
		log.Print("shutting down")
		return nil
	case err := <-pumpErr:
		if err != nil {
			return fmt.Errorf("capture pump: %w", err)
		}
		return nil
	case err := <-srvErr:
		if err != nil {
			return fmt.Errorf("rtsp server: %w", err)
		}
		return nil
	}
}

// startAnnounce advertises the appliance over mDNS in the background. Failure is
// logged, not fatal: the appliance still serves on a multicast-blocked network,
// where the manual host:port entry is the fallback.
func startAnnounce(ctx context.Context, cfg *config.Config, rate, channels int) {
	_, portStr, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		log.Printf("mDNS disabled: cannot parse listen address %q: %v", cfg.Listen, err)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Printf("mDNS disabled: bad port in %q: %v", cfg.Listen, err)
		return
	}
	codec := "L16"
	if cfg.Mode == config.ModeOpus {
		codec = "opus"
	}
	info := announce.Info{Name: cfg.Name, Path: "/stream", Port: port, Codec: codec, Rate: rate, Channels: channels, Version: version}
	go func() {
		if err := announce.Run(ctx, []announce.Info{info}); err != nil {
			log.Printf("mDNS advertisement stopped: %v (serving continues without discovery)", err)
		}
	}()
	log.Printf("advertising %q over mDNS (_rtsp._tcp) on port %d", cfg.Name, port)
}

func buildStage(cfg *config.Config, channels int) (stage pipeline.Stage, payloadType int) {
	if cfg.Mode == config.ModeOpus {
		return pipeline.NewOpus(cfg.Opus), 97
	}
	return pipeline.NewPCM(channels), 96
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "birdnet-go-remote-mic:", err)
	os.Exit(1)
}
