# birdnet-go-remote-mic

A small, pure-Go remote microphone appliance for [BirdNET-Go](https://github.com/tphakala/birdnet-go).

Runs as a single static binary on a Raspberry Pi Zero 2 W (arm64) or similar,
captures local audio, and streams it over the LAN as standard RTSP/RTP.
BirdNET-Go discovers the mic automatically over mDNS and pulls the stream with
its native ingest client. No ffmpeg, no third-party media server, no external
processes: just one binary you control.

## Status

Early planning. The normative design and roadmap live as issues in the private
tracker; this README is a high-level overview only.

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
