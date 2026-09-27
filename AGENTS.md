# AGENTS.md

Instructions for AI coding agents (Claude Code, Antigravity/Gemini, and others)
working in this repository. `AGENTS.md` files are the single source of project
guidance; do not add a `CLAUDE.md` or `GEMINI.md` that could drift from them.

Keep every `AGENTS.md` under 20 KB (`task agents:size` enforces it): some agents
silently truncate larger files. Put module-specific rules in that module's own
`AGENTS.md` and list it here. Some agents do not load nested files on their
own, so read the matching file below before editing there:

| Working on | Also read |
|---|---|
| `internal/` (any package under it) | `internal/AGENTS.md` |
| `web/` (TypeScript UI, CSS, `index.html`) | `web/AGENTS.md` |

## What this is

`remote-mic` (module `github.com/tphakala/birdnet-go-remote-mic`) is a pure-Go,
zero-CGO, single-static-binary remote microphone appliance for
[BirdNET-Go](https://github.com/tphakala/birdnet-go). It captures audio from
local ALSA devices and serves it over a self-implemented TCP-interleaved
RTSP/RTP server, advertised over mDNS (`_rtsp._tcp`), plus an HTTPS management
API and an embedded web UI.

- Stream modes: `opus` (48 kHz, mono or stereo) and `pcm` (raw L16 at the
  capture rate, up to 384 kHz, for ultrasonic bat detection).
- One capture per device, fanned out to one or more RTSP streams, each with its
  own path, mode, and channel subset.
- Linux only. arm64 is primary (Pi Zero 2 W and up), 32-bit arm (GOARM=6) is a
  release target, amd64 is for development.
- Go 1.27. No `replace` directives; the module builds from public sources.

Sibling libraries (same author, public). A bug in capture, RTSP framing, or the
codec usually belongs in one of these, not here:
`github.com/tphakala/go-audio-capture` (pure-Go ALSA capture, ioctl level),
`github.com/tphakala/go-audio-stream` (RTSP messages, RTP/RTCP marshal,
`packet/l16`, `packet/opus`, SDP), `github.com/tphakala/go-opus` (Opus encoder).

## Architecture rules

- The sound card is the clock. The capture read loop drives everything; there
  is no pacer. The capture pump runs on a `runtime.LockOSThread` goroutine.
- Stamp RTCP Sender Reports from `Frame.Captured`, never from send time, or TCP
  backpressure skews the receiver's clock.
- The RTP hot path writes into a reused buffer with a 4-byte interleave prefix.
  Do not call `rtsp.MarshalInterleaved` per RTP packet (it allocates); once per
  SR interval is fine. Avoid per-period allocations: the target is a Pi Zero 2 W.
- Optimize for the unattended state. The appliance usually runs 24/7 with
  BirdNET-Go pulling one or more streams and no browser open, and a configured
  stream may have no client playing. Work that exists only for a consumer that
  may be absent (the web UI or management API: levels metering, sampling,
  formatting, JSON encoding; an RTSP client: encoding) must not run on a timer
  or in the capture path without that consumer: gate it on a live consumer, as
  `internal/levels` does (`Meter.Observe` returns early and the sampler skips
  its tick while the hub's `subs` is zero) and the encode stages do (gated on
  the RTSP feed's active flag), or compute it lazily on request, as
  `sysinfo.CPUGauge` does. Background work is justified only for capture,
  streaming to a playing client, hotplug recovery, or condition monitors that
  raise alerts.
- Recover automatically and safely. The appliance runs unattended for months,
  often out of reach, so any unexpected event (device unplugged or erroring,
  overrun, encoder fault, client vanishing, corrupt or missing file, clock
  step, network change) must end in automatic recovery, not a wait for a human:
  - Degrade per device and per stream, never the whole process. The
    management API stays up with zero devices serving.
  - Retry with backoff and a bounded rate, never a hot loop, and never
    re-notify on every attempt (use the notification center's flap and
    hysteresis helpers).
  - Never guess when a guess can do harm: a card-index device id is not
    restarted unattended, because after a hotplug the index may name a
    different microphone. Never destroy operator data (installed certificate,
    config file) to self-heal from a possibly transient error.
  - Make every failure and recovery visible: a notification onset on failure,
    a clear on recovery, plus a log line.
  - systemd (`Restart=always`, `RestartSec=5` in `internal/service/unit.go`) is
    a backstop, not a recovery strategy: in-process recovery keeps the other
    streams running.
  - A goroutine panic kills the process. Code running callbacks or third-party
    logic on a long-lived goroutine recovers per call (see `deliverTap` in
    `internal/levels`).
- Rotating or enabling the access token must evict live RTSP sessions: the
  writer checks `Auth.Snapshot()` (enabled flag plus generation, read together)
  on every frame and tears down a session authorized under an older generation.
  Clients that keep alive with RTCP only would otherwise stream forever.
- Config changes apply live through `internal/reload`; prefer in-place
  reconciliation over process restarts.
- Keep platform-neutral packages free of Linux-only imports; put Linux-only
  code in `//go:build linux` files so vet on every GOARCH stays green.

## Go conventions (Go 1.27)

The module targets Go 1.27, likely newer than your training data. Use current
idioms: the `modernize`, `intrange` and `usetesting` linters enforce most of
them, and `go fix ./...` applies most fixes automatically. Never lower the `go` directive or
write code "for compatibility". Check an API with `go doc <pkg>.<Symbol>`
rather than guessing.

- Pure Go, `CGO_ENABLED=0`, one static binary. No new dependency when the
  standard library or an existing one does the job; justify any addition in
  the PR.
- No `init()`. Package-level variables are sentinel errors, read-only tables,
  compiled regexps, `go:embed` data, interface checks
  (`var _ I = (*T)(nil)`), the ldflags `version`, and swappable test seams
  (such as `execSelf`). The few process-wide mutable ones (the `sysinfo`
  host-fact caches, `runtimeGen`, `probeOpenSlot`) are concurrency-safe and
  say why they exist; other mutable state belongs to a struct with an owner.
- `ctx context.Context` is the first parameter. Do not store one in a struct;
  the exception is the lifetime context of a struct that starts goroutines,
  marked `//nolint:containedctx // <why>`.
- Prefer `slices`, `maps`, `strings.Cut`, `SplitSeq`/`FieldsSeq`, `min`/`max`
  and `new(expr)` over hand-written equivalents. Indexing `strings.Fields` is
  fine for fixed-column formats like `/proc/stat`.
- `iter.Seq`/`iter.Seq2` for lazy or streaming sequences and in place of
  callback APIs. Return a slice when the caller needs its length, indexing or
  sorting.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, compare with `errors.Is`,
  extract with `errors.AsType`. Sentinels are `ErrXxx`, types are `XxxError`
  (errname). `errors.New` for constant messages.
- Logging is the standard `log` package, lowercase messages that lead with the
  subsystem (`pprof: ...`) or device (`device %q disconnected: ...`).
- Comments explain why, not what; match the surrounding density and tone.

Safety:

- Every goroutine has an owner that stops it (context or close) and waits for
  it (`sync.WaitGroup.Go`, or a done channel).
- Typed atomics only (`atomic.Bool`, `atomic.Int64`, `atomic.Pointer[T]`):
  `atomic.Int64` is 8-byte aligned on 32-bit arm, where a bare `int64` used
  with the atomic functions is not guaranteed to be. One word goes in an
  atomic; an invariant over several fields takes a mutex or one
  `atomic.Pointer` to an immutable value (`auth.Guard` does this).
- Bounds-check before narrowing an integer that came from config, a file, or
  the network. Intended wraparound (RTP sequence numbers, timestamps) says so
  in a comment.

Performance, for the per-period path (capture, fan-out, pipeline, RTP writer):

- It does not allocate. A change there keeps or adds a `testing.AllocsPerRun`
  test (see `internal/audio/fanout_test.go`) or a `b.ReportAllocs()`
  benchmark.
- No `fmt`, `log`, or string building per period; append into reused buffers.
- `sync.Pool`, iterators, and other tuning only with a benchmark showing a
  gain.

## Tests

- Standard library `testing` only (no testify), table-driven where it helps,
  failures in `got X, want Y` form. Helpers call `t.Helper()`.
- Mark independent tests `t.Parallel()`, except `AllocsPerRun` tests, which
  count allocations process-wide. Use `testing/synctest` for timer or ticker
  code, `t.Context()` rather than `context.Background()` plus cancel, and
  `b.Loop()` in benchmarks. No `time.Sleep` for synchronization.
- Tests must pass with or without a sound card: never open real ALSA hardware
  (use `audio.NewFakeSource` and the existing seams), and no network beyond
  loopback.

## Linting policy

golangci-lint v2 with `.golangci.yaml` (adapted from go-audio-stream's); the
enabled linters are listed there. Zero findings is the bar:
`max-issues-per-linter` and `max-same-issues` are 0 and `new: false`, so
pre-existing code is linted too.

- Fix findings rather than suppress them. A justified suppression names the
  linter and says why on the same line:
  `//nolint:nilnil // a nil lock with no error means "serve without a lock"`.
  A bare `//nolint` is not acceptable.
- `exhaustive` accepts a `default:`, but prefer listing every enum case.
- Project-wide bans go in gocritic ruleguard matchers in `rules/*.go` (build
  tag `ruleguard`).

## Releases

Tags `v*` are built by GoReleaser (`.goreleaser.yaml`,
`.github/workflows/release.yml`): Linux amd64/arm64/arm (GOARM=6) tarballs,
`.deb` packages, and a Homebrew formula in `tphakala/homebrew-tap`. Builds use
`-pgo=cmd/remotemic/default.pgo` by explicit path, so a missing profile fails
the release. The profile is GOARCH-independent; collect a new one with
`remote-mic serve --pprof 127.0.0.1:6060` (unauthenticated, loopback only).

Each release also gets a signed manifest (`manifest.json` plus
`manifest.json.sig`) for update checks, written after GoReleaser by
`tools/releasemanifest` with the `RELEASE_MANIFEST_KEY` secret (a base64
Ed25519 seed). The workflow runs `check-key` before publishing, so a key
missing from `internal/releasemanifest/keys.go` fails the release early. The
schema is additive like `/api/v1`: add optional fields, never repurpose one; a
field old readers must not ignore goes in `requires`. A `Schema` bump ships
under a new file name beside `manifest.json`, since installed appliances keep
fetching that one. Rotate the key by shipping the new public key in `keys.go`
first, then switching the secret.

Self-updated appliances keep the units their original install wrote, the
installed (older) binary is the updater that checks the new one, and
recovery after a power loss runs in the new, unconfirmed binary. So these
are contracts between versions: add, never rename or repurpose, or every
later update is refused or rolled back for good:
- the `service apply-update --bin-path --state-dir` invocation, and the
  appliance unit's `serve --cert-dir` and `serve --check` lines (the
  staging directory derives from `--cert-dir`);
- the first line of `remote-mic version`, exactly `remote-mic <version>`,
  which the old updater compares with the manifest;
- the staging file names in `internal/update/files.go` and the JSON fields
  of `health.json`, `status.json` and `request.json`;
- the install journal (`<bin>.pending`) and its fields.

## Build, test, and the gate

```
task check        # the full local gate; run it before every push
```

It runs `agents:size`, `web:verify` (tsc, oxlint, html-validate a11y, node:test,
web build), CGO-off builds for amd64/arm64/arm, `go vet` on amd64/arm64/arm/386,
golangci-lint, `api:lint` (vacuum), `api:verify` (generated code matches the
spec), `licenses:verify`, gofmt, and `go test -race`.

`web/dist` is generated by `task web:build` and is NOT committed. Bare `go`
commands need the compiled assets or the stub UI:

```
CGO_ENABLED=0 go build -tags skipfrontend ./...
go test -race -tags skipfrontend ./...
golangci-lint run ./...          # .golangci.yaml already sets skipfrontend
```

`cmd/remotemic` and every `_linux.go` file build only for Linux: on another
host pass `GOOS=linux` to build, vet, and golangci-lint, and run the full test
suite on Linux.

Other tasks: `task test`, `task lint`, `task fmt`, `task web:test`,
`task api:generate`, and `task web:sweep` (optional, not in `check`: renders
the UI in Chromium against a mock API; see `web/AGENTS.md`). The web
toolchain is pinned in `Taskfile.yml` and run via `npx -p`; there is no
`package.json` or `node_modules`.

`task licenses:generate` (`tools/licensegen`) regenerates
`THIRD_PARTY_LICENSES.md` and `web/static/licenses.json` (the About page's
data) from the modules linked into every release target plus the bundled
fonts. Commit both with any change to the linked modules; the Licenses
workflow does this itself on Dependabot pull requests. remote-mic itself is
Apache-2.0 (`LICENSE`, `NOTICE`).

## Code style and workflow

- Commit subjects follow Conventional Commits with a scope where it helps
  (`feat(audio): ...`, `fix(service): ...`, `docs: ...`). Hard-wrap commit
  bodies at about 72 columns. PR descriptions are GitHub Markdown with one
  line per paragraph, no hard wrapping.
- Code changes go through a pull request and are squash-merged. Docs-only,
  formatting-only, and config/metadata changes may go straight to `main` after
  `task check` passes.
- Design documents, plans, specs, and research notes are never committed;
  `docs/` is gitignored for them.
- The roadmap and bug tracker are this repository's GitHub issues; reference
  them as `#123`.
- Keep `README.md` accurate when a change alters user-visible behavior,
  config, or install steps.

## Management API conventions

- `/api/v1` is additive-only: never remove or repurpose a field or endpoint.
- Errors are RFC 9457 `application/problem+json`. Wire schemas use camelCase;
  YAML config uses snake_case.
- Live data (levels, notifications) is SSE-only on one multiplexed
  `GET /api/v1/events` with a named `heartbeat` every 15 s (the UI uses `fetch`
  streaming because `EventSource` cannot send `Authorization`).
- After editing `api/openapi.yaml`: `task api:lint`, `task api:generate`, then
  commit the regenerated `internal/mgmtapi/gen.go` with the spec, and update
  `web/src/lib/types.ts` and `api.ts` in the same change.

## Layout

- `cmd/remotemic`: the binary (all files `//go:build linux`). Subcommands
  `serve` (default), `token`, `devices`, `service`, `version`. `--config`
  defaults to `$REMOTEMIC_CONFIG`, else `./config.yaml`. `appliance.go` wires
  capture, pipeline, RTSP, monitors, and reload. `default.pgo` is the PGO
  profile.
- `internal/`: the packages; see `internal/AGENTS.md`.
- `tools/releasemanifest`: writes, signs and verifies the release manifest
  (`keygen`, `check-key`, `generate`, `verify`), and decides whether a
  release becomes "latest" (`make-latest`). `tools/licensegen` writes the
  third-party license files.
- `web/`: vanilla TypeScript UI embedded with `go:embed`. See `web/AGENTS.md`.
- `api/openapi.yaml`: the OpenAPI 3.1 contract, committed as source.
- `rules/rules.go`: gocritic ruleguard rules.

## Gotchas

- go-audio-capture: `Negotiate` must issue PREPARE after SW_PARAMS, or Start
  returns EBADFD. ioctl struct sizes and numbers are verified against
  `sound/asound.h` separately for LP64 and ILP32; do not guess them.
- go-audio-stream `rtsp.Client.Dial` only sends OPTIONS; a test client must
  call `Describe`, `Setup`, and `Play` explicitly to receive frames.
- `go get` with a branch name containing a slash fails ("disallowed version
  string"); pin a commit SHA instead.
- Notification `time` is wall-clock; a Pi without an RTC can step its clock
  after NTP syncs, so do not assume it is monotonic. Place entries and measure
  durations from `uptimeMs` (monotonic) instead, as the web UI does.
- Local test servers: use free high ports (18000 and up), track the PIDs you
  start, and kill only those. Never kill a process just because it holds a
  port or sound device you wanted.
