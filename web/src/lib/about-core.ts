// Pure logic for the About page (views/about.ts): the project links, the shape
// of licenses.json (written by tools/licensegen from the build's module graph,
// so the page never drifts from what the binary links), and the plain-text
// system details a bug report asks for. No DOM, network, or storage here.
import type { ApplianceStatus, Device, SystemInfo } from "./types.js";

export const REPO_URL = "https://github.com/tphakala/birdnet-go-remote-mic";
export const ISSUES_URL = `${REPO_URL}/issues`;
export const DISCUSSIONS_URL = `${REPO_URL}/discussions`;
export const SPONSOR_URL = "https://github.com/sponsors/tphakala";
export const AUTHOR_URL = "https://github.com/tphakala";
export const BIRDNET_GO_URL = "https://github.com/tphakala/birdnet-go";

// The static file the About page loads, relative to the page (web:build copies
// web/static into the embedded UI; static files are served without the token).
export const LICENSES_PATH = "licenses.json";

export interface LicenseFile {
  name: string;
  text: string;
}

export interface LicenseEntry {
  name: string;
  // Absent for the Go runtime and the fonts, which have no module version.
  version?: string;
  // The license names the generator recognized, such as "MIT" or "Apache-2.0, MIT".
  license: string;
  files: LicenseFile[];
}

export interface LicenseDoc {
  project: LicenseEntry;
  components: LicenseEntry[];
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function parseEntry(v: unknown): LicenseEntry | null {
  if (!isRecord(v) || typeof v.name !== "string" || typeof v.license !== "string" || !Array.isArray(v.files)) return null;
  if (v.version !== undefined && typeof v.version !== "string") return null;
  const files: LicenseFile[] = [];
  for (const f of v.files) {
    if (!isRecord(f) || typeof f.name !== "string" || typeof f.text !== "string") return null;
    files.push({ name: f.name, text: f.text });
  }
  // A component with no license text would render an empty disclosure; the
  // generator never writes one, so treat it as a malformed file.
  if (files.length === 0) return null;
  return { name: v.name, license: v.license, files, ...(v.version ? { version: v.version } : {}) };
}

// parseLicenseDoc validates licenses.json, returning null for anything that is
// not the generator's shape (a stale or truncated file, a proxy error page), so
// the page shows a load error instead of rendering half a list.
export function parseLicenseDoc(v: unknown): LicenseDoc | null {
  if (!isRecord(v) || !Array.isArray(v.components)) return null;
  const project = parseEntry(v.project);
  if (!project) return null;
  const components: LicenseEntry[] = [];
  for (const c of v.components) {
    const e = parseEntry(c);
    if (!e) return null;
    components.push(e);
  }
  return { project, components };
}

// componentTitle names a component with its version, as the summary line shows it.
export function componentTitle(e: LicenseEntry): string {
  return e.version ? `${e.name} ${e.version}` : e.name;
}

// supportDetails is the plain text the "Copy System Details" button puts on the
// clipboard for a bug report: the build, the host, and the capture devices, and
// nothing identifying (no hostname, addresses, token, USB serial, device name,
// or stream path), since the report is public. The version line always appears
// and the Platform line whenever host details are known, each reading "unknown"
// when empty; any other field the host does not report is left out. The device
// section appears whenever devices is given, even empty, so a report shows that
// no device was configured.
export function supportDetails(status: ApplianceStatus | null, system: SystemInfo | null, devices?: readonly Device[]): string {
  const lines = [`remote-mic version: ${status?.version || "unknown"}`];
  if (system) {
    lines.push(`Platform: ${system.platform || "unknown"}`);
    if (system.os) lines.push(`OS: ${system.os}`);
    if (system.kernel) lines.push(`Kernel: ${system.kernel}`);
    if (system.cpuModel) lines.push(system.cpuCores > 0 ? `CPU: ${system.cpuModel} (${system.cpuCores} cores)` : `CPU: ${system.cpuModel}`);
  }
  if (devices) lines.push("", ...deviceDetails(devices));
  return lines.join("\n");
}

// deviceDetails is one block per capture device, ordered by the configured
// device name so the text does not depend on the API's order. The name itself
// is left out: it is the DNS-SD instance name the operator chose, and the stream
// path is usually derived from it. A plain code-unit comparison keeps the order
// independent of the browser's locale. Every device's configured id feeds
// each block's error scrub, since one device's error can quote another's, and
// a note above the blocks explains the placeholders whenever an error shows.
function deviceDetails(devices: readonly Device[]): string[] {
  if (devices.length === 0) return ["Capture devices: none"];
  const sorted = [...devices].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  const known = knownValues(devices);
  const lines = [`Capture devices: ${sorted.length}`];
  if (sorted.some((d) => d.state !== "serving" && d.error)) lines.push(PRIVACY_NOTE);
  sorted.forEach((d, i) => lines.push("", ...deviceBlock(d, i + 1, known)));
  return lines;
}

// Said above the device blocks whenever an error text was scrubbed, so the
// placeholders do not read as a bug.
const PRIVACY_NOTE = "Values in <angle brackets> and after usb:vendor:product were removed for privacy.";

const USB_ID = /^usb:([0-9a-f]{4}):([0-9a-f]{4})(?::|$)/i;

// usbLabel is a USB id reduced to vendor:product, the part that names the
// model and not the unit, or null when id is not a USB id.
function usbLabel(id: string): string | null {
  const m = USB_ID.exec(id);
  return m ? `usb:${m[1].toLowerCase()}:${m[2].toLowerCase()}` : null;
}

// deviceIdKind describes a configured device id without the parts that identify
// the unit: a USB id keeps only vendor:product (never the s= serial or the p=
// port), a stable ALSA card-name id and a card index are named by kind only.
// idStable is absent from an older appliance, so the id's own form decides then,
// as config.IsCardIndexID does: any usb: id, or an hw: id naming its card.
function deviceIdKind(d: Device): string {
  const id = d.device.trim();
  const usb = usbLabel(id);
  const stable = d.idStable ?? (id.startsWith("usb:") || (id.startsWith("hw:") && id.includes("=")));
  let kind = "card index";
  if (usb) kind = `USB ${usb.slice("usb:".length)}`;
  else if (stable) kind = "ALSA card name";
  return `${kind}, ${stable ? "stable" : "not stable (can change after a reboot or replug)"}`;
}

// A replacement of one configured value in error text.
interface Known {
  value: string;
  placeholder: string;
}

// Values shorter than this are not replaced literally: a card-index id such as
// "1" identifies nothing, and replacing it would garble numbers in the text.
const MIN_KNOWN_LENGTH = 3;
// A card-index id ("hw:1,0", "2,0") names a slot, not the unit, and the
// hardware address it matches is what a same-hardware error needs to show.
const CARD_INDEX_ID = /^(?:hw:)?\d+(?:,\d+)?$/;

// knownValues is every configured device id across ALL devices except card
// indexes (which identify nothing), longest first,
// since an error on one device can quote another's, and an id the catch-all
// rules do not recognize (a malformed one) is printed unquoted. Names and
// paths are not listed: the appliance quotes a name with %q, which the quoted
// rule covers, and a stream path always starts with "/" and holds no
// whitespace, which the path rule covers whole, including a path that
// extends another ("/garden/night").
function knownValues(devices: readonly Device[]): Known[] {
  return devices
    .map((d) => d.device.trim())
    .filter((id) => id.length >= MIN_KNOWN_LENGTH && !CARD_INDEX_ID.test(id))
    .map((id) => ({ value: id, placeholder: usbLabel(id) ?? "<device id>" }))
    .sort((a, b) => b.value.length - a.value.length);
}

const escapeRegExp = (s: string): string => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// A token here runs to whitespace, a quote or a parenthesis, and stops before a
// comma or colon that ends it (", " in a list, ": " before a message), so a USB
// id's own "if=0,0" and inner colons stay inside it.
const TOKEN_TAIL = String.raw`(?:[^\s"'(),:]|[,:](?!\s|$))*`;
const USB_TOKEN = new RegExp(String.raw`\busb:` + TOKEN_TAIL, "gi");
// No word boundary before hw: so a plughw:CARD= prefix is caught too.
const CARD_TOKEN = new RegExp(String.raw`hw:CARD=` + TOKEN_TAIL, "gi");
// A Go %q string, escapes included, so a quote inside a name cannot end it early.
const QUOTED = /"(?:[^"\\]|\\.)*"/g;
// Single-quoted and backtick spans: the appliance quotes with %q today, but a
// dependency's message could quote either way, and the scrub must not rely on it.
// An apostrophe inside a word ("can't") does not open a span.
const OTHER_QUOTED = /(?<![\p{L}\p{N}])'[^'\n]*'|`[^`\n]*`/gu;
// Control characters, line and paragraph separators, and bidi overrides (from a
// hardware-supplied name) would start a fabricated line in the report or
// reorder it on screen.
const CONTROL = /[\p{Cc}\p{Zl}\p{Zp}\u202A-\u202E\u2066-\u2069]+/gu;
// An absolute path wherever it starts (after a space, a colon, a bracket, a
// placeholder), to the next whitespace: a stream path may hold any other
// character. A trailing colon or closing parenthesis is the message's own and
// is kept. /proc and /dev/snd paths name kernel interfaces, not the
// operator's setup, and are kept for the diagnosis.
const PATH_TOKEN = /(?<![A-Za-z0-9_.-])\/(?!proc\/|dev\/snd\/)\S+/g;
// What a scrubbed quoted span may still hold and be kept: exactly a
// placeholder this scrub writes, a vendor:product id, or one of the fixed
// selector names go-audio-capture quotes in its id-format hints.
const SAFE_QUOTED = /^"(?:<device id>|usb:[0-9a-f]{4}:[0-9a-f]{4}|s=|p=|:if=)"$/;

// scrub makes a device's error text safe to paste publicly. Configured ids go
// first, while they still match literally, and only as whole tokens. Then
// catch-all rules cover what the web UI does not know: any USB or card-name
// id, any quoted string (Go quotes device names and malformed ids with %q;
// single quotes and backticks too), and any absolute path (every stream's,
// including those the record omits). Quotes go before paths, so a path inside
// a quoted name cannot split the quote and expose the rest of the name.
// Control characters become spaces first, so the text stays on its one report
// line.
function scrub(text: string, known: readonly Known[]): string {
  let out = oneLine(text);
  for (const k of known) {
    out = out.replace(new RegExp(`(?<![A-Za-z0-9_.-])${escapeRegExp(k.value)}(?![A-Za-z0-9_.-])`, "g"), k.placeholder);
  }
  out = out.replace(USB_TOKEN, (t) => usbLabel(t) ?? "usb:<id>");
  out = out.replace(CARD_TOKEN, "hw:CARD=<card>");
  out = out.replace(QUOTED, (q) => (SAFE_QUOTED.test(q) ? q : `"<redacted>"`));
  out = out.replace(OTHER_QUOTED, (q) => `${q[0]}<redacted>${q[0]}`);
  return out.replace(PATH_TOKEN, (t) => `<path>${/[:)]$/.exec(t)?.[0] ?? ""}`);
}

const oneLine = (s: string): string => s.replace(CONTROL, " ");

// ALSA's long card name can end in the bus position ("... at usb-0000:01:00.0-1.2,
// high speed"); the kernel's short name is the part before it.
const shortCardName = (name: string): string => name.replace(/\s+at\s+usb-.*$/, "");

const channelList = (ch: readonly number[]): string => (ch.length > 0 ? ch.join(",") : "none");

function deviceBlock(d: Device, n: number, known: readonly Known[]): string[] {
  const lines = [`Device ${n}: ${(d.friendlyName && shortCardName(oneLine(d.friendlyName))) || "(no name reported)"}`];
  lines.push(`  ID: ${deviceIdKind(d)}`);
  let state: string = d.state;
  if (d.state !== "serving") {
    if (d.downCause) state += ` (${d.downCause})`;
    if (d.error) state += `: ${scrub(d.error, known)}`;
  }
  lines.push(`  State: ${state}`);
  // The record projects only the first stream's selection; streamedChannels is
  // the union over every stream, which is what the capture opens.
  lines.push(`  Configured: ${d.rate} Hz, channels ${channelList(d.streamedChannels ?? d.channels)}`);
  const neg: string[] = [];
  if (d.negotiatedRate) neg.push(`${d.negotiatedRate} Hz`);
  if (d.negotiatedChannels) neg.push(`${d.negotiatedChannels} ${d.negotiatedChannels === 1 ? "channel" : "channels"}`);
  if (d.negotiatedFormat) neg.push(d.negotiatedFormat);
  if (neg.length > 0) lines.push(`  Negotiated: ${neg.join(", ")}`);
  // Plain words rather than the UI's modeLabel badges ("OPUS", "PCM L16"):
  // this is prose for a bug report.
  lines.push(`  First stream: ${d.mode === "opus" ? "Opus" : "PCM"}, channels ${channelList(d.channels)}`);
  lines.push(`  Overruns: ${d.overruns ?? "not reported"}, dropped frames: ${d.droppedFrames}`);
  return lines;
}
