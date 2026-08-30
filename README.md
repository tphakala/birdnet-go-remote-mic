# birdnet-go-remote-mic

[![CI](https://github.com/tphakala/birdnet-go-remote-mic/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/birdnet-go-remote-mic/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/tphakala/birdnet-go-remote-mic/branch/main/graph/badge.svg)](https://codecov.io/gh/tphakala/birdnet-go-remote-mic)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/birdnet-go-remote-mic)](go.mod)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tphakala/birdnet-go-remote-mic/badge)](https://scorecard.dev/viewer/?uri=github.com/tphakala/birdnet-go-remote-mic)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

A small, pure-Go remote microphone appliance for [BirdNET-Go](https://github.com/tphakala/birdnet-go).

Runs as a single static binary on a Raspberry Pi Zero 2 W (arm64) or similar,
captures local audio, and streams it over the LAN as standard RTSP/RTP.
BirdNET-Go discovers the mic automatically over mDNS and pulls the stream with
its native ingest client. No ffmpeg, no third-party media server, no external
processes: just one binary you control.

## Status

Phases 0, 2, and 3 are implemented: capture (via
[go-audio-capture](https://github.com/tphakala/go-audio-capture)), the L16 and
Opus pipeline, a TCP-interleaved RTSP server that ffmpeg, VLC, and BirdNET-Go's
own ingest client can play, and mDNS/DNS-SD advertisement so BirdNET-Go can
discover the mic automatically. Packaging (phase 6) is still to come. The
normative design and roadmap live as issues in the private tracker.

## Discovery

Each configured device is advertised as its own mDNS/DNS-SD `_rtsp._tcp`
instance (so `avahi-browse -r _rtsp._tcp` and `dns-sd -B _rtsp._tcp` see them
all), with TXT records BirdNET-Go reads to adopt it: `codec`, `rate`, `ch`,
`path`, and a `txtvers`. It sends goodbye packets on shutdown so stale entries
clear promptly. Set `discovery.enabled: false` to turn it off; on a network
where multicast does not cross, add each mic in BirdNET-Go by its `host:port`
plus path instead.

## Usage

One binary per host serves any number of capture devices: each entry in the
`devices:` list gets its own RTSP path on the shared `listen` port and its own
mDNS instance. Write a config (see `config.example.yaml`):

```yaml
listen: ":8554"
discovery:
  enabled: true
devices:
  - name: garden-mic       # unique instance name; also the mDNS label
    device: "hw:1,0"
    path: /garden          # unique RTSP path; defaults to /stream
    mode: opus             # "opus" (48 kHz mono) or "pcm" (L16, any rate, ultrasonic)
    rate: 48000
    channels: 1
    format: s16
    opus:
      bitrate: 64000
  - name: ultrasonic-mic   # add as many devices as the hardware supports
    device: "hw:2,0"
    path: /bat
    mode: pcm
    rate: 256000
    channels: 1
    format: s16
```

```bash
birdnet-go-remote-mic -list-devices          # enumerate capture devices
birdnet-go-remote-mic -config config.yaml    # capture and serve
```

Then pull each stream at `rtsp://<host>:8554<path>`, for example
`rtsp://<host>:8554/garden`. A single-device config is just a one-entry list.

## Debugging

Play or inspect the stream with standard tools (TCP-interleaved transport):

```bash
ffprobe -rtsp_transport tcp rtsp://<host>:8554/stream
ffplay  -rtsp_transport tcp rtsp://<host>:8554/stream
ffmpeg  -rtsp_transport tcp -i rtsp://<host>:8554/stream -t 5 out.wav
```

For a local end-to-end check without hardware, use the ALSA loopback
(`snd-aloop`): play a tone into `hw:Loopback,0` and point a device's
`device` at the capture side `hw:Loopback,1`.

### Multi-device behaviour

- A device that fails to open at startup is logged and skipped. With the
  management API enabled (the default) the process stays up so its status API
  keeps reporting every skipped device and its open error, even when no device
  opens at all. With management disabled there is nothing to keep alive, so a
  total open failure exits nonzero and lets a supervisor restart the process.
- A device that dies mid-run (a USB unplug) is retired: its path returns 404
  until the process restarts, while the other devices keep serving. With
  management enabled the process also stays up after the last device dies, so
  the failure stays inspectable over the API (in-process capture restart is a
  later phase); with management disabled it exits once every device has stopped.
  A retired device's mDNS advertisement persists until the process exits (a
  limitation of the dnssd responder), so a discoverer that picks it up gets 404.
- Practical limits are hardware, not software: ALSA `hw:` devices are
  single-client (the config rejects a device id used twice), USB isochronous
  bandwidth is shared per controller (watch for xruns when several high-rate
  or ultrasonic mics share one hub), and independent devices drift relative to
  each other over time (each stream is honest to its own capture clock).

## Development

```bash
task check   # build (amd64 + arm64, CGO off), vet, lint, gofmt, race tests
```

## What it does

- **Captures** audio from any number of local devices (USB or I2S) with no
  transcoding glue, one binary per host serving one RTSP stream per device.
- **Streams** over a self-implemented RTSP/RTP server, TCP-interleaved by
  default so the audio arrives lossless and firewall-friendly.
- **Two modes over one protocol:**
  - *Normal audio* uses Opus at 48 kHz mono, low bandwidth for ordinary
    birdsong.
  - *Ultrasonic* uses raw PCM (L16) at high sample rates (up to 256 or 384 kHz)
    for bat detection, where no lossy codec can carry the signal. On a LAN the
    uncompressed bandwidth is a non-issue (~4 Mbit/s at 256 kHz mono).
- **Announces itself** on the LAN over mDNS / DNS-SD, with a manual host:port
  fallback for networks where multicast does not cross.

## Design principles

- Pure Go, zero CGO, single static binary, arm64 Linux only (32-bit is out of
  scope).
- Reuse the existing [go-audio-stream](https://github.com/tphakala/go-audio-stream)
  transport, codec, and RTP machinery rather than reinventing it. The stream
  primitives are the inverse of what that library already does on ingest.
- Simple install, mirroring how BirdNET-Go deploys on the same hardware.

## License

MIT. See [LICENSE](LICENSE).
