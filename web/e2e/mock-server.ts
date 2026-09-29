// A zero-dependency mock of the remote-mic management API for the rendered UI
// sweep (web/e2e/sweep.ts). It serves a compiled UI directory as static files
// and answers every /api/v1 endpoint the UI calls with fixed fixture data, so
// the sweep renders the real UI against realistic, repeatable state without a
// sound card or a running appliance.
//
// Run it on its own to look at the UI by hand:
//
//   node web/e2e/mock-server.ts <dist dir> [port]
//
// Node runs this file directly (type stripping), so it uses only erasable
// TypeScript syntax. Besides the Node built-ins it imports the event names
// from web/src/lib/sse.ts, whose module-level code needs nothing a browser
// has and Node lacks, and the API types from web/src/lib/types.ts as
// type-only imports, which keeps the fixtures in step with what the UI
// expects.

import { createServer } from "node:http";
import type { IncomingMessage, ServerResponse } from "node:http";
import { LEVELS_EVENT, NOTIFICATION_EVENT } from "../src/lib/sse.ts";
import { readFile, stat } from "node:fs/promises";
import { extname, join, normalize, resolve, sep } from "node:path";

import type {
  ApplianceStatus,
  AvailableDevice,
  CertificateInfo,
  Config,
  ConfigUpdateResult,
  Device,
  DeviceConfig,
  DeviceLevels,
  Health,
  LevelsEvent,
  Notification,
  NotificationSnapshot,
  SystemInfo,
  UpdateStatus,
} from "../src/lib/types.ts";

export const DEFAULT_PORT = 18300;

const VERSION = "v1.4.0";
const BOOT_ID = "mock-boot-0001";
// The appliance has been up for a while, so uptimes and relative times in the
// UI show realistic multi-day values rather than seconds.
const BOOT_UPTIME_MS = 3 * 86_400_000 + 5 * 3_600_000 + 17 * 60_000;
const startedAt = Date.now();

function uptimeMs(): number {
  return BOOT_UPTIME_MS + (Date.now() - startedAt);
}

// Devices: every state the dashboard renders. The serving stereo device fans
// out into two streams (channel 1 as Opus, channel 2 as PCM), which is why
// channels (first stream) and streamedChannels (all streams) differ.
const DEVICES: Device[] = [
  {
    name: "Garden",
    device: "usb:0d8c:0014:s=CM108A1:if=0,0",
    path: "/garden",
    mode: "opus",
    format: "s16",
    rate: 48000,
    channels: [1],
    streamedChannels: [1, 2],
    state: "serving",
    negotiatedRate: 48000,
    negotiatedChannels: 2,
    negotiatedFormat: "s24_3le",
    clientConnected: true,
    clientCount: 2,
    droppedFrames: 12,
    overruns: 1,
    opus: { bitrate: 96000 },
    friendlyName: "C-Media USB Audio Device",
    hwAddr: "hw:2,0",
    idStable: true,
    supportedRates: [44100, 48000, 96000],
    supportedChannels: [1, 2],
    streams: [
      { path: "/garden", clientConnected: true, clientCount: 2, droppedFrames: 12 },
      { path: "/garden-right", clientConnected: false, clientCount: 0, droppedFrames: 0 },
    ],
  },
  {
    name: "Bat Detector",
    device: "usb:16d0:0b40:s=UM384K02:if=0,0",
    path: "/bats",
    mode: "pcm",
    format: "s16",
    rate: 384000,
    channels: [1],
    streamedChannels: [1],
    state: "serving",
    negotiatedRate: 384000,
    negotiatedChannels: 1,
    negotiatedFormat: "s16",
    clientConnected: false,
    clientCount: 0,
    droppedFrames: 0,
    overruns: 0,
    friendlyName: "Dodotronic Ultramic 384K",
    hwAddr: "hw:3,0",
    idStable: true,
    supportedRates: [192000, 384000],
    supportedChannels: [1],
    streams: [{ path: "/bats", clientConnected: false, clientCount: 0, droppedFrames: 0 }],
  },
  {
    name: "Pond",
    device: "usb:1397:0508:p=1-1.2:if=0,0",
    path: "/pond",
    mode: "opus",
    format: "s16",
    rate: 48000,
    channels: [1, 2],
    streamedChannels: [1, 2],
    state: "failed",
    clientConnected: false,
    clientCount: 0,
    droppedFrames: 0,
    overruns: 3,
    opus: { bitrate: 64000 },
    error: "capture read failed: input/output error (device disconnected after 2h14m of capture)",
    downCause: "disconnected",
    friendlyName: "Behringer UMC202HD 192k",
    idStable: true,
    supportedRates: [44100, 48000, 96000, 192000],
    supportedChannels: [2],
  },
  {
    name: "Hedge",
    device: "usb:0c76:161f:p=1-1.4:if=0,0",
    path: "/hedge",
    mode: "opus",
    format: "s16",
    rate: 48000,
    channels: [1],
    streamedChannels: [1],
    state: "disabled",
    opus: { bitrate: 128000 },
    clientConnected: false,
    clientCount: 0,
    droppedFrames: 0,
    friendlyName: "USB PnP Audio Device",
    idStable: true,
  },
  {
    name: "Feeder",
    device: "hw:5,0",
    path: "/feeder",
    mode: "pcm",
    format: "s16",
    rate: 96000,
    channels: [1],
    streamedChannels: [1],
    state: "skipped",
    clientConnected: false,
    clientCount: 0,
    droppedFrames: 0,
    error: "device not connected: no capture device matches hw:5,0",
    downCause: "not-connected",
    idStable: false,
  },
];

const AVAILABLE: AvailableDevice[] = [
  {
    device: "usb:2752:0019:s=Y8ZQ2BM1:if=0,0",
    state: "available",
    hwAddr: "hw:4,0",
    idStable: true,
    friendlyName: "Focusrite Scarlett 2i2 4th Gen",
    supportedRates: [44100, 48000, 88200, 96000, 176400, 192000],
    supportedChannels: [2],
  },
];

// deviceConfig derives the persisted config entry from a runtime fixture, the
// way the appliance's config mirrors its devices, streams included for every
// device (two for Garden, one mirroring the flat fields otherwise).
function deviceConfig(d: Device): DeviceConfig {
  const cfg: DeviceConfig = {
    name: d.name,
    device: d.device,
    path: d.path,
    mode: d.mode,
    rate: d.rate,
    channels: d.channels,
    format: "s16",
    enabled: d.state !== "disabled",
    quietAlert: d.mode !== "pcm",
  };
  if (d.opus) cfg.opus = d.opus;
  // A read always carries streams, one entry for a single-stream device, so
  // the UI's stream paths render as they do against the appliance.
  cfg.streams =
    d.name === "Garden"
      ? [
          { path: "/garden", mode: "opus", channels: [1], opus: { bitrate: 96000 } },
          { path: "/garden-right", mode: "pcm", channels: [2] },
        ]
      : [{ path: d.path, mode: d.mode, channels: d.channels, ...(d.opus ? { opus: d.opus } : {}) }];
  return cfg;
}

// Config with every optional block materialized, as a read returns it.
const CONFIG: Config = {
  listen: ":8554",
  discovery: { enabled: true },
  management: { enabled: true, listen: ":8443", certDir: "/var/lib/remote-mic/certs" },
  auth: { token: "" },
  notifications: {
    enabled: true,
    audio: { quietDbfs: -70, quietSeconds: 300, zeroSeconds: 10, clipPercent: 1, clipWindowSeconds: 60 },
    host: {
      cpuPercent: 90,
      cpuClearPercent: 75,
      tempCelsius: 80,
      tempClearCelsius: 70,
      diskPercent: 90,
      diskClearPercent: 85,
      memFreePercent: 5,
      memFreeMiB: 32,
    },
  },
  updates: { check: true },
  devices: DEVICES.map(deviceConfig),
};

const HEALTH: Health = { status: "ok", version: VERSION, authRequired: false };

function status(): ApplianceStatus {
  return {
    version: VERSION,
    uptimeSeconds: Math.floor(uptimeMs() / 1000),
    rtspListen: ":8554",
    discoveryEnabled: true,
    authRequired: false,
    devicesServing: DEVICES.filter((d) => d.state === "serving").length,
    devicesTotal: DEVICES.length,
    overrides: [{ field: "management.listen", effective: ":9443", persisted: ":8443" }],
  };
}

const UPDATE: UpdateStatus = {
  currentVersion: VERSION,
  supported: true,
  checkEnabled: true,
  latestVersion: "v1.5.0",
  notesUrl: "https://github.com/tphakala/birdnet-go-remote-mic/releases/tag/v1.5.0",
  available: true,
  lastCheck: "2026-09-26T06:12:40Z",
  installMethod: "service",
  canApply: true,
  phase: "idle",
};

const SYSTEM: SystemInfo = {
  platform: "linux/arm64",
  os: "Debian GNU/Linux 13 (trixie)",
  kernel: "6.12.47+rpt-rpi-v8",
  hostname: "remote-mic-garden",
  cpuModel: "Cortex-A53",
  cpuCores: 4,
  cpuPercent: 23.4,
  memTotalBytes: 512 * 1024 * 1024,
  memUsedBytes: 187 * 1024 * 1024,
  diskTotalBytes: 29_700_000_000,
  diskUsedBytes: 4_210_000_000,
  tempCelsius: 52.6,
  network: [
    {
      name: "wlan0",
      mac: "b8:27:eb:12:34:56",
      up: true,
      // A long global IPv6 address exercises wrapping at 320 px.
      addresses: ["192.168.1.40/24", "2001:db8:1234:5678:ba27:ebff:fe12:3456/64", "fe80::ba27:ebff:fe12:3456/64"],
      rxBytes: 18_734_112_904,
      txBytes: 402_118_553_771,
      kind: "wifi",
      wifi: { ssid: "garden-ap", signalDbm: -63, frequencyMhz: 2437 },
    },
    { name: "eth0", mac: "b8:27:eb:65:43:21", up: false, addresses: [], rxBytes: 0, txBytes: 0, kind: "ethernet" },
    // Up, with an address, but not a physical link: must not appear in the card.
    { name: "docker0", mac: "02:42:ac:11:00:01", up: true, addresses: ["172.17.0.1/16"], rxBytes: 0, txBytes: 0, kind: "other" },
  ],
  update: UPDATE,
};

const CERTIFICATE: CertificateInfo = {
  subject: "CN=remote-mic-garden",
  issuer: "CN=remote-mic-garden",
  selfSigned: true,
  managed: true,
  dnsNames: ["remote-mic-garden", "remote-mic-garden.local", "localhost"],
  ipAddresses: ["192.168.1.40", "127.0.0.1", "::1"],
  notBefore: "2026-06-01T08:00:00Z",
  notAfter: "2036-05-30T08:00:00Z",
  fingerprintSha256:
    "3F:A1:9C:07:5B:E2:44:D8:91:6A:0F:2C:B7:13:E5:48:9D:70:C6:2B:5E:A8:F1:03:67:DC:84:1A:BE:52:09:F7",
};

const CERT_PEM = "-----BEGIN CERTIFICATE-----\nMIIBmock\n-----END CERTIFICATE-----\n";

// notif fills the fields every notification shares; ago is how long before the
// snapshot the entry was published.
function notif(
  id: number,
  agoMs: number,
  fields: Pick<Notification, "severity" | "category" | "kind" | "title" | "message"> &
    Partial<Pick<Notification, "key" | "source">>,
): Notification {
  const up = uptimeMs() - agoMs;
  return {
    id,
    bootId: BOOT_ID,
    time: new Date(Date.now() - agoMs).toISOString(),
    uptimeMs: up,
    ...fields,
  };
}

// The notification snapshot: active conditions (onsets without a clear) of
// each severity, a resolved condition (onset plus clear), and one-off events of
// every severity, so the bell panel and the Events view render every row style.
function snapshot(): NotificationSnapshot {
  const notifications: Notification[] = [
    notif(1, 9 * 3_600_000, { severity: "info", category: "system", kind: "event", title: "Appliance started", message: `remote-mic ${VERSION} started with 5 configured devices.` }),
    notif(2, 8 * 3_600_000, { severity: "info", category: "stream", kind: "event", source: "192.168.1.20:51234", title: "Client connected", message: "An RTSP client started playing /garden." }),
    notif(3, 6 * 3_600_000, { severity: "warning", category: "system", kind: "onset", key: "host.temp", title: "High SoC temperature", message: "SoC temperature reached 81.2 °C (threshold 80 °C)." }),
    notif(4, 5 * 3_600_000, { severity: "info", category: "system", kind: "clear", key: "host.temp", title: "SoC temperature back to normal", message: "SoC temperature fell to 69.8 °C." }),
    notif(5, 3 * 3_600_000, { severity: "warning", category: "config", kind: "event", title: "Config override active", message: "management.listen is overridden by a command-line flag for this run." }),
    notif(6, 2 * 3_600_000, { severity: "error", category: "device", kind: "onset", key: "device.Pond.down", source: "Pond", title: "Device disconnected", message: "Pond stopped capturing: input/output error. It restarts when it is plugged back in." }),
    notif(7, 90 * 60_000, { severity: "warning", category: "audio", kind: "onset", key: "audio.Garden.quiet", source: "Garden", title: "Very quiet audio", message: "Garden has stayed below -70 dBFS for 5 minutes. Check the microphone and its cable." }),
    notif(8, 40 * 60_000, { severity: "error", category: "audio", kind: "onset", key: "audio.Garden.clip", source: "Garden", title: "Audio clipping", message: "Garden clipped on 3.4% of samples over the last minute. Lower the input gain." }),
    notif(9, 25 * 60_000, { severity: "error", category: "stream", kind: "event", source: "/garden", title: "Stream stalled", message: "The RTSP client on /garden stopped reading; dropped 12 frames before it recovered." }),
    notif(10, 10 * 60_000, { severity: "info", category: "device", kind: "onset", key: "device.Feeder.missing", source: "Feeder", title: "Device not connected", message: "Feeder (hw:5,0) is not connected. It is bound by card index, so it is not restarted automatically." }),
  ];
  return {
    bootId: BOOT_ID,
    serverTime: new Date().toISOString(),
    uptimeMs: uptimeMs(),
    capacity: 500,
    nextId: notifications.length + 1,
    notifications,
  };
}

// liveNotification is the error the sweep pushes over SSE to raise a toast. It
// takes the snapshot's next id, so the client sees no gap and keeps its state.
function liveNotification(): Notification {
  return notif(snapshot().nextId, 0, {
    severity: "error",
    category: "device",
    kind: "event",
    source: "Garden",
    title: "Capture overrun",
    message: "Garden lost audio to a capture overrun (ALSA xrun) and recovered. Check CPU load if this repeats.",
  });
}

// LEVEL_PERIOD ticks (10 Hz) make one level cycle, laid out so the meter's
// peak-hold readout (hold about 0.75 s, then fall 30 dB/s) passes through
// every width it can show: a clipped full-scale sample (0.0 dBFS, CLIP
// latched) that decays through -x.x and -xx.x dBFS to -inf over the silence
// after it, the near-floor -59.9 dBFS (still shown as -inf), and a hot -3.2
// dBFS. Silence is the -99 dBFS floor the API clamps to. A meter row must keep
// its height across all of them.
const LEVEL_PERIOD = 40;

function peakAt(tick: number): { peak: number; clipped: boolean } {
  switch (tick % LEVEL_PERIOD) {
    case 0:
      return { peak: 0, clipped: true };
    case 30:
      return { peak: -59.9, clipped: false };
    case 35:
      return { peak: -3.2, clipped: false };
    default:
      return { peak: -99, clipped: false };
  }
}

function levelsAt(tick: number): LevelsEvent {
  const devices: DeviceLevels[] = DEVICES.filter((d) => d.state === "serving").map((d) => ({
    name: d.name,
    channels: Array.from({ length: d.negotiatedChannels ?? 1 }, (_, channel) => {
      // Each further channel runs half a cycle behind, so the two rows of a
      // stereo device never show the same readout.
      const s = peakAt(tick + (channel * LEVEL_PERIOD) / 2);
      return {
        channel,
        peakDbfs: s.peak,
        rmsDbfs: Math.max(-99, s.peak - 9),
        clipped: s.clipped,
      };
    }),
  }));
  return { devices };
}

const MIME: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
  ".txt": "text/plain; charset=utf-8",
};

function sendJSON(res: ServerResponse, code: number, body: unknown): void {
  res.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(JSON.stringify(body));
}

function notFound(res: ServerResponse, path: string): void {
  res.writeHead(404, { "Content-Type": "application/problem+json" });
  res.end(JSON.stringify({ title: "Not Found", status: 404, detail: `no mock for ${path}` }));
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Uint8Array[] = [];
  for await (const chunk of req) chunks.push(chunk as Uint8Array);
  return Buffer.concat(chunks).toString("utf8");
}

export interface AvailableEdit {
  friendlyName?: string;
  maxRate?: number;
}

export interface MockServer {
  url: string;
  // pushNotification sends the live error notification to every open event
  // stream, so the UI raises its error toast.
  pushNotification(): void;
  // failPaths makes GET requests to these paths (for example "/api/v1/devices"
  // or "/licenses.json") answer 503, until it is called again; an empty list
  // ends the failure.
  failPaths(paths: readonly string[]): void;
  // editAvailable changes the one available device the way a poll would find
  // it changed (a new friendly name, a lower top probed rate), until it is
  // called again; no argument puts the fixture back.
  editAvailable(edit?: AvailableEdit): void;
  close(): Promise<void>;
}

// eventFilter reads a request's ?events= filter as the appliance does
// (internal/sse/sse.go parseEventFilter): absent or empty means every type,
// otherwise a comma-separated list of type names.
function eventFilter(req: IncomingMessage): Set<string> | null {
  const q = new URL(req.url ?? "/", "http://mock").searchParams.get("events") ?? "";
  const names = q.split(",").map((n) => n.trim()).filter(Boolean);
  return names.length === 0 ? null : new Set(names);
}

// startMockServer serves distDir and the mock API on 127.0.0.1:port (0 picks a
// free port). Three test hooks sit beside the API: POST /__mock/notify pushes
// the live error notification; POST /__mock/fail (or the returned failPaths)
// makes chosen GET paths answer 503; and POST /__mock/edit-available (or
// editAvailable) changes the available device between polls.
export async function startMockServer(distDir: string, port: number = DEFAULT_PORT): Promise<MockServer> {
  const root = resolve(distDir);
  // Each open event stream, with the event types its ?events= filter asked
  // for (null for every type), applied as the appliance applies it.
  const streams = new Map<ServerResponse, Set<string> | null>();
  const send = (name: string, frame: string): void => {
    for (const [res, wanted] of streams) if (wanted === null || wanted.has(name)) res.write(frame);
  };

  // One shared 10 Hz ticker drives every open stream, so all pages see the same
  // phase of the level cycle.
  let tick = 0;
  const levelTimer = setInterval(() => {
    send(LEVELS_EVENT, `event: ${LEVELS_EVENT}\ndata: ${JSON.stringify(levelsAt(tick++))}\n\n`);
  }, 100);
  const heartbeatTimer = setInterval(() => {
    // Heartbeats pass every filter.
    for (const res of streams.keys()) res.write("event: heartbeat\ndata: {}\n\n");
  }, 15_000);

  // Paths whose GET answers 503, as the appliance does when a read fails.
  let failing = new Set<string>();
  // What GET /devices/available answers: the fixture, or an edited copy.
  let available: AvailableDevice[] = AVAILABLE;

  async function serveStatic(pathname: string, res: ServerResponse): Promise<void> {
    const rel = pathname === "/" ? "index.html" : decodeURIComponent(pathname).replace(/^\/+/, "");
    const file = normalize(join(root, rel));
    // Refuse anything that normalizes outside the served directory.
    if (file !== root && !file.startsWith(root + sep)) return notFound(res, pathname);
    try {
      const info = await stat(file);
      if (!info.isFile()) return notFound(res, pathname);
      const body = await readFile(file);
      res.writeHead(200, { "Content-Type": MIME[extname(file)] ?? "application/octet-stream", "Cache-Control": "no-store" });
      res.end(body);
    } catch {
      notFound(res, pathname);
    }
  }

  async function serveAPI(method: string, path: string, req: IncomingMessage, res: ServerResponse): Promise<void> {
    const route = `${method} ${path}`;
    switch (route) {
      case "GET /healthz":
        return sendJSON(res, 200, HEALTH);
      case "GET /status":
        return sendJSON(res, 200, status());
      case "GET /devices":
        return sendJSON(res, 200, DEVICES);
      case "GET /devices/available":
        return sendJSON(res, 200, available);
      case "GET /config":
        return sendJSON(res, 200, CONFIG);
      case "GET /system":
        return sendJSON(res, 200, SYSTEM);
      case "GET /system/certificate":
        return sendJSON(res, 200, CERTIFICATE);
      case "GET /system/certificate/pem":
        res.writeHead(200, { "Content-Type": "application/x-pem-file" });
        return void res.end(CERT_PEM);
      case "GET /notifications":
        return sendJSON(res, 200, snapshot());
      case "GET /events":
        res.writeHead(200, {
          "Content-Type": "text/event-stream",
          "Cache-Control": "no-cache",
          Connection: "keep-alive",
          "X-Accel-Buffering": "no",
        });
        // The open comment the appliance writes once the stream is set up
        // (openComment in internal/sse/sse.go).
        res.write(": open\n\n");
        streams.set(res, eventFilter(req));
        req.on("close", () => streams.delete(res));
        return;
      // Mutations answer with a plausible success and change nothing, so every
      // page of the sweep starts from the same state.
      case "PATCH /config": {
        await readBody(req);
        const result: ConfigUpdateResult = { config: CONFIG, restartRequired: false };
        return sendJSON(res, 200, result);
      }
      case "POST /devices":
        await readBody(req);
        return sendJSON(res, 201, DEVICES[0]);
      case "POST /system/restart":
        return sendJSON(res, 202, { status: "restarting" });
      case "POST /system/update/check":
        return sendJSON(res, 200, UPDATE);
      case "POST /system/update":
        return sendJSON(res, 202, { ...UPDATE, phase: "downloading", phaseMessage: "Downloading v1.5.0" });
      case "POST /system/certificate/regenerate":
      case "PUT /system/certificate":
        await readBody(req);
        return sendJSON(res, 200, CERTIFICATE);
    }
    if (method === "DELETE" && path.startsWith("/devices/")) {
      res.writeHead(204);
      return void res.end();
    }
    notFound(res, path);
  }

  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://mock");
    const method = req.method ?? "GET";
    let work: Promise<void>;
    if (method === "GET" && failing.has(url.pathname)) {
      res.writeHead(503, { "Content-Type": "application/problem+json", "Cache-Control": "no-store" });
      res.end(JSON.stringify({ title: "Service Unavailable", status: 503, detail: "failing by request of the sweep" }));
      return;
    } else if (url.pathname.startsWith("/api/v1/")) {
      work = serveAPI(method, url.pathname.slice("/api/v1".length), req, res);
    } else if (url.pathname === "/__mock/notify" && method === "POST") {
      // Test hook for the sweep; not part of the appliance API.
      pushNotification();
      res.writeHead(204);
      res.end();
      return;
    } else if (url.pathname === "/__mock/fail" && method === "POST") {
      // Test hook for manual runs: ?paths=/api/v1/devices,/licenses.json fails
      // those paths, and no paths ends the failure.
      failPaths((url.searchParams.get("paths") ?? "").split(",").filter(Boolean));
      res.writeHead(204);
      res.end();
      return;
    } else if (url.pathname === "/__mock/edit-available" && method === "POST") {
      // Test hook for manual runs: ?name=&maxRate= edits the available device,
      // and no parameters put it back.
      const name = url.searchParams.get("name");
      const rate = url.searchParams.get("maxRate");
      editAvailable(name === null && rate === null ? undefined : { friendlyName: name ?? undefined, maxRate: rate === null ? undefined : Number(rate) });
      res.writeHead(204);
      res.end();
      return;
    } else {
      work = serveStatic(url.pathname, res);
    }
    work.catch((err: unknown) => {
      console.error("mock-server:", err);
      if (!res.headersSent) sendJSON(res, 500, { title: "Mock error", status: 500 });
      else res.end();
    });
  });

  function failPaths(paths: readonly string[]): void {
    failing = new Set(paths);
  }

  function editAvailable(edit?: AvailableEdit): void {
    const { friendlyName, maxRate } = edit ?? {};
    available = AVAILABLE.map((d) => ({
      ...d,
      friendlyName: friendlyName ?? d.friendlyName,
      supportedRates: maxRate === undefined ? d.supportedRates : d.supportedRates?.filter((r) => r <= maxRate),
    }));
  }

  function pushNotification(): void {
    send(NOTIFICATION_EVENT, `event: ${NOTIFICATION_EVENT}\ndata: ${JSON.stringify(liveNotification())}\n\n`);
  }

  await new Promise<void>((ok, fail) => {
    server.once("error", fail);
    server.listen(port, "127.0.0.1", () => ok());
  });
  const addr = server.address();
  const actualPort = typeof addr === "object" && addr ? addr.port : port;

  return {
    url: `http://127.0.0.1:${actualPort}`,
    pushNotification,
    failPaths,
    editAvailable,
    close(): Promise<void> {
      clearInterval(levelTimer);
      clearInterval(heartbeatTimer);
      for (const res of streams.keys()) res.end();
      streams.clear();
      return new Promise<void>((ok) => server.close(() => ok()));
    },
  };
}

if (import.meta.main) {
  const [distDir, portArg] = process.argv.slice(2);
  if (!distDir) {
    console.error("usage: node web/e2e/mock-server.ts <dist dir> [port]");
    process.exit(2);
  }
  const srv = await startMockServer(distDir, portArg ? Number(portArg) : DEFAULT_PORT);
  console.log(`mock-server: serving ${distDir} at ${srv.url}`);
}
