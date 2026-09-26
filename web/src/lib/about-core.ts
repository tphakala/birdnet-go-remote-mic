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
// independent of the browser's locale.
function deviceDetails(devices: readonly Device[]): string[] {
  if (devices.length === 0) return ["Capture devices: none"];
  const sorted = [...devices].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  const lines = [`Capture devices: ${sorted.length}`];
  sorted.forEach((d, i) => lines.push("", ...deviceBlock(d, i + 1)));
  return lines;
}

const USB_ID = /^usb:([0-9a-f]{4}):([0-9a-f]{4})(?::|$)/i;

// deviceIdKind describes a configured device id without the parts that identify
// the unit: a USB id keeps only vendor:product (never the s= serial or the p=
// port), a stable ALSA card-name id and a card index are named by kind only.
// idStable is absent from an older appliance, so the id's own form decides then,
// matching config.IsCardIndexID.
function deviceIdKind(d: Device): string {
  const id = d.device.trim();
  const usb = USB_ID.exec(id);
  const stable = d.idStable ?? (usb !== null || (id.startsWith("hw:") && id.includes("=")));
  let kind = "card index";
  if (usb) kind = `USB ${usb[1].toLowerCase()}:${usb[2].toLowerCase()}`;
  else if (stable) kind = "ALSA card name";
  return `${kind}, ${stable ? "stable" : "not stable (can change after a reboot or replug)"}`;
}

// redact strips what an error message may quote from the device's config: the
// id (with its serial) and the stream path, plus any other USB id beyond its
// vendor:product.
function redact(text: string, d: Device): string {
  let out = text;
  const usb = USB_ID.exec(d.device.trim());
  if (d.device.trim()) out = out.replaceAll(d.device.trim(), usb ? `usb:${usb[1]}:${usb[2]}` : "<device id>");
  if (d.path && d.path !== "/") out = out.replaceAll(d.path, "<path>");
  return out.replace(/\b(usb:[0-9a-f]{4}:[0-9a-f]{4}):[^\s"';)]*/gi, "$1");
}

const channelList = (ch: readonly number[]): string => (ch.length > 0 ? ch.join(",") : "none");

function deviceBlock(d: Device, n: number): string[] {
  const lines = [`Device ${n}: ${d.friendlyName || "(no name reported)"}`];
  lines.push(`  ID: ${deviceIdKind(d)}`);
  let state: string = d.state;
  if (d.state !== "serving") {
    if (d.downCause) state += ` (${d.downCause})`;
    if (d.error) state += `: ${redact(d.error, d)}`;
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
  lines.push(`  First stream: ${d.mode === "opus" ? "Opus" : "PCM"}, channels ${channelList(d.channels)}`);
  lines.push(`  Overruns: ${d.overruns ?? "not reported"}, dropped frames: ${d.droppedFrames}`);
  return lines;
}
