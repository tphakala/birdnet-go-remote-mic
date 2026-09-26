// Pure logic for the About page (views/about.ts): the project links, the shape
// of licenses.json (written by tools/licensegen from the build's module graph,
// so the page never drifts from what the binary links), and the plain-text
// system details a bug report asks for. No DOM, network, or storage here.
import type { ApplianceStatus, SystemInfo } from "./types.js";

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
// clipboard for a bug report: the build and the host, and nothing identifying
// (no hostname, addresses, or token), since the report is public.
export function supportDetails(status: ApplianceStatus | null, system: SystemInfo | null): string {
  const lines = [`remote-mic version: ${status?.version || "unknown"}`];
  if (system) {
    lines.push(`Platform: ${system.platform}`);
    if (system.os) lines.push(`OS: ${system.os}`);
    if (system.kernel) lines.push(`Kernel: ${system.kernel}`);
    if (system.cpuModel) lines.push(`CPU: ${system.cpuModel} (${system.cpuCores} cores)`);
  }
  return lines.join("\n");
}
