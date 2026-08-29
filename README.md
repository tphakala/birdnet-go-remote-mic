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
normative design and roadmap live as issues in a separate location.

## Discovery

The appliance advertises itself over mDNS/DNS-SD as `_rtsp._tcp` (so
`avahi-browse -r _rtsp._tcp` and `dns-sd -B _rtsp._tcp` see it too), with TXT
records BirdNET-Go reads to adopt it: `codec`, `rate`, `ch`, `path`, and a
`txtvers`. It sends goodbye packets on shutdown so stale entries clear promptly.
Set `discovery.enabled: false` to turn it off; on a network where multicast does
not cross, add the mic in BirdNET-Go by its `host:port` instead.

## Usage

Write a config (see `config.example.yaml`):

```yaml
name: garden-mic
listen: ":8554"
mode: pcm          # "pcm" (L16, any rate, ultrasonic) or "opus" (48 kHz mono)
audio:
  device: "hw:1,0"
  rate: 256000
  channels: 1
  format: s16
```

```
birdnet-go-remote-mic -list-devices          # enumerate capture devices
birdnet-go-remote-mic -config config.yaml    # capture and serve
```

Then pull the stream at `rtsp://<host>:8554/stream`.

## Debugging

Play or inspect the stream with standard tools (TCP-interleaved transport):

```
ffprobe -rtsp_transport tcp rtsp://<host>:8554/stream
ffplay  -rtsp_transport tcp rtsp://<host>:8554/stream
ffmpeg  -rtsp_transport tcp -i rtsp://<host>:8554/stream -t 5 out.wav
```

For a local end-to-end check without hardware, use the ALSA loopback
(`snd-aloop`): play a tone into `hw:Loopback,0` and point the appliance's
`audio.device` at the capture side `hw:Loopback,1`.

## Development

```
task check   # build (amd64 + arm64, CGO off), vet, lint, gofmt, race tests
```

## What it does

- **Captures** audio from a local device (USB or I2S) with no transcoding glue.
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
