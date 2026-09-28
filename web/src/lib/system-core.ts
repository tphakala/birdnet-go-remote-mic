// I/O-free logic for the System view's telemetry tiles, System Information
// rows and serve-override note. No DOM: the cards in views/system/ own the
// elements and render what these functions decide.

import type { ApplianceStatus, ConfigOverride, SystemInfo } from "./types.ts";
import { lastCheckText } from "./update-core.ts";

// formatByteSize renders a byte count as GB (>= 1 GB) or MB, for the static
// Memory and Storage totals in System Information. The live usage of each is in
// the telemetry tiles above.
export function formatByteSize(bytes: number): string {
  const gb = bytes / 1073741824;
  if (gb >= 1) return `${gb >= 10 ? Math.round(gb) : gb.toFixed(1)} GB`;
  return `${Math.round(bytes / 1048576)} MB`;
}

// TileSpec is one resource-gauge tile's data for a render pass. barPct
// undefined means the tile shows no progress bar.
export interface TileSpec {
  key: string;
  label: string;
  sub: string;
  value: string;
  unit: string;
  barPct: number | undefined;
}

// TILE_SLOTS are the gauges a host usually reports, in display order. The grid
// lays them out before the first system read, so the read fills them in place
// instead of growing the page.
export const TILE_SLOTS: ReadonlyArray<{ key: string; label: string }> = [
  { key: "cpu", label: "CPU Utilization" },
  { key: "mem", label: "Memory" },
  { key: "temp", label: "SoC Temperature" },
  { key: "disk", label: "Disk" },
];

// tileSpecs lists the live resource gauges in display order. Memory and Disk
// are left out when the host reports no total for them.
export function tileSpecs(sys: SystemInfo): TileSpec[] {
  const specs: TileSpec[] = [];
  specs.push({
    key: "cpu", label: "CPU Utilization", sub: sys.cpuCores > 0 ? `${sys.cpuCores} Cores` : "",
    value: sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a", unit: "%", barPct: sys.cpuPercent,
  });
  if (sys.memTotalBytes > 0) {
    specs.push({
      key: "mem", label: "Memory", sub: `${formatByteSize(sys.memTotalBytes)} Total`,
      value: String(Math.round(sys.memUsedBytes / 1048576)), unit: "MB used",
      barPct: (sys.memUsedBytes / sys.memTotalBytes) * 100,
    });
  }
  specs.push({
    key: "temp", label: "SoC Temperature", sub: "",
    value: sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a", unit: "deg C",
    barPct: sys.tempCelsius !== undefined ? (sys.tempCelsius / 85) * 100 : undefined,
  });
  if (sys.diskTotalBytes > 0) {
    specs.push({
      key: "disk", label: "Disk", sub: `${formatByteSize(sys.diskTotalBytes)} Total`,
      value: (sys.diskUsedBytes / 1073741824).toFixed(1), unit: "GB used",
      barPct: (sys.diskUsedBytes / sys.diskTotalBytes) * 100,
    });
  }
  return specs;
}

// InfoLabel names every System Information row; the card keys its rows and
// icons by it.
export type InfoLabel =
  | "Platform" | "CPU" | "Memory" | "Storage"
  | "Hostname" | "OS" | "Kernel" | "Version" | "Latest Release" | "Last Check" | "Uptime";

// InfoRow is one System Information line: which column it belongs to, its
// label, and the value string.
export interface InfoRow {
  group: "hw" | "sw";
  label: InfoLabel;
  value: string;
}

// infoRows lists the System Information lines in display order from whichever
// of the system and status reads have arrived. Two columns: Hardware holds the
// physical machine's specs, Software holds the OS and appliance build. The live
// gauges (CPU %, memory, temp and disk usage) live in the telemetry tiles, so
// this card carries static facts and shows the memory and storage TOTALS rather
// than their usage. The two formatters are passed in, as lastCheckText takes
// its own, so this module does not import lib/ui.ts.
export function infoRows(
  sys: SystemInfo | null,
  st: ApplianceStatus | null,
  now: number,
  formatUptime: (seconds: number) => string,
  formatRelative: (fromMs: number, toMs: number) => string,
): InfoRow[] {
  const rows: InfoRow[] = [];
  if (sys) {
    rows.push({ group: "hw", label: "Platform", value: sys.platform || "-" });
    if (sys.cpuModel || sys.cpuCores) {
      const cpu = sys.cpuModel
        ? `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`
        : `${sys.cpuCores} cores`;
      rows.push({ group: "hw", label: "CPU", value: cpu });
    }
    if (sys.memTotalBytes > 0) rows.push({ group: "hw", label: "Memory", value: formatByteSize(sys.memTotalBytes) });
    if (sys.diskTotalBytes > 0) rows.push({ group: "hw", label: "Storage", value: formatByteSize(sys.diskTotalBytes) });
    rows.push({ group: "sw", label: "Hostname", value: sys.hostname || "-" });
    if (sys.os) rows.push({ group: "sw", label: "OS", value: sys.os });
    if (sys.kernel) rows.push({ group: "sw", label: "Kernel", value: sys.kernel });
  }
  if (st) rows.push({ group: "sw", label: "Version", value: st.version || "-" });
  // A build that names no release never checks, so it has no release rows.
  const u = sys?.update;
  if (u?.supported) {
    rows.push({ group: "sw", label: "Latest Release", value: u.latestVersion ?? "-" });
    rows.push({ group: "sw", label: "Last Check", value: lastCheckText(u.lastCheck, now, formatRelative) });
  }
  if (st) rows.push({ group: "sw", label: "Uptime", value: formatUptime(st.uptimeSeconds) });
  return rows;
}

// INFO_ORDER is every System Information row in display order: the rows a
// release build on a host that reports every fact shows.
export const INFO_ORDER: readonly InfoRow[] = [
  { group: "hw", label: "Platform", value: "" },
  { group: "hw", label: "CPU", value: "" },
  { group: "hw", label: "Memory", value: "" },
  { group: "hw", label: "Storage", value: "" },
  { group: "sw", label: "Hostname", value: "" },
  { group: "sw", label: "OS", value: "" },
  { group: "sw", label: "Kernel", value: "" },
  { group: "sw", label: "Version", value: "" },
  { group: "sw", label: "Latest Release", value: "" },
  { group: "sw", label: "Last Check", value: "" },
  { group: "sw", label: "Uptime", value: "" },
];

// withPlaceholders fills the rows a read has not supplied yet with empty
// values, in display order, so the card keeps the height it will have once
// both reads are in.
export function withPlaceholders(rows: readonly InfoRow[]): InfoRow[] {
  const have = new Map(rows.map((r) => [r.label, r]));
  return INFO_ORDER.map((slot) => have.get(slot.label) ?? slot);
}

// OVERRIDE_LABELS maps a serve-override's dotted config field to an operator-
// facing label. An unmapped field falls back to its dotted path.
const OVERRIDE_LABELS: Record<string, string> = {
  listen: "RTSP listen address",
  "management.listen": "Management listen address",
  "management.certDir": "Certificate directory",
  "management.enabled": "Management API",
  "discovery.enabled": "mDNS discovery",
};

// overrideLines renders one line per active serve override: why a persisted
// config value differs from what the appliance runs, because a serve CLI flag
// overrode it for this run.
export function overrideLines(overrides: readonly ConfigOverride[]): string[] {
  return overrides.map((o) => {
    const label = OVERRIDE_LABELS[o.field] ?? o.field;
    const persisted = o.persisted === "" ? "(default)" : o.persisted;
    return `${label}: serving ${o.effective} (config file: ${persisted})`;
  });
}

// overridesSignature identifies an override set, so a status poll that changes
// nothing does not rebuild (and re-announce) the note.
export function overridesSignature(overrides: readonly ConfigOverride[]): string {
  return overrides.map((o) => `${o.field}=${o.effective}|${o.persisted}`).join("\n");
}
