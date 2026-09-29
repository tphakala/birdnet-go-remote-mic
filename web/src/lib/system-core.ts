// I/O-free logic for the System view's telemetry tiles, System Information
// rows and serve-override note. No DOM: the cards in views/system/ own the
// elements and render what these functions decide.

import type { ApplianceStatus, ConfigOverride, NetworkInterface, SystemInfo } from "./types.ts";
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

// InfoLabel names every System Information row; the card keys its icons by it.
export type InfoLabel =
  | "Platform" | "CPU" | "Memory" | "Storage"
  | "Connection" | "IPv4" | "IPv6" | "Wi-Fi Network" | "Signal" | "Band"
  | "Hostname" | "OS" | "Kernel" | "Version" | "Latest Release" | "Last Check" | "Uptime";

// InfoRow is one System Information line: which column group it belongs to,
// its identity, its label, and the value string. key identifies the row across
// renders; it is the label except for the network rows, where several
// interfaces repeat the same labels.
export interface InfoRow {
  group: "hw" | "net" | "sw";
  key: string;
  label: InfoLabel;
  value: string;
}

// row builds a hardware or software row, whose key is its label.
function row(group: "hw" | "sw", label: InfoLabel, value: string): InfoRow {
  return { group, key: label, label, value };
}

// signalQuality words a Wi-Fi signal strength in dBm.
export function signalQuality(dbm: number): "Excellent" | "Good" | "Fair" | "Weak" {
  if (dbm >= -50) return "Excellent";
  if (dbm >= -60) return "Good";
  if (dbm >= -70) return "Fair";
  return "Weak";
}

// WifiBand is a channel frequency as a band name and, where the frequency
// falls on a standard channel, its number.
export interface WifiBand {
  band: string;
  channel?: number;
}

// wifiBand names the band of a frequency in MHz and its channel number. A
// frequency outside the three Wi-Fi bands, or between channels, is shown as
// plain MHz (or a band with no channel).
export function wifiBand(mhz: number): WifiBand {
  const chan = (origin: number): number | undefined => {
    const c = (mhz - origin) / 5;
    return Number.isInteger(c) && c >= 1 ? c : undefined;
  };
  if (mhz >= 2401 && mhz <= 2495) return { band: "2.4 GHz", channel: mhz === 2484 ? 14 : chan(2407) };
  if (mhz >= 5150 && mhz <= 5895) return { band: "5 GHz", channel: chan(5000) };
  if (mhz >= 5925 && mhz <= 7125) return { band: "6 GHz", channel: mhz === 5935 ? 2 : chan(5950) };
  return { band: `${mhz} MHz` };
}

// bandText is the Band row's value: `5 GHz, channel 44`.
function bandText(b: WifiBand): string {
  return b.channel === undefined ? b.band : `${b.band}, channel ${b.channel}`;
}

// signalText is `-52 dBm (Good)`.
function signalText(dbm: number): string {
  return `${dbm} dBm (${signalQuality(dbm)})`;
}

// shownInterfaces are the links the UI lists: physical Ethernet and Wi-Fi that
// are up, in API order. An interface with no kind, or one this UI does not
// know, is not shown: the UI ships in the same binary as the API, so that only
// happens with a newer or unusual appliance, and a bridge or tunnel is noise
// on an appliance page anyway.
export function shownInterfaces(sys: SystemInfo): NetworkInterface[] {
  return sys.network.filter((n) => n.up && (n.kind === "ethernet" || n.kind === "wifi"));
}

// displayAddresses turns CIDR strings into addresses to show: the prefix
// dropped, IPv4 first, then IPv6 without link-local (fe80::/10) addresses,
// each family in API order.
export function displayAddresses(addresses: readonly string[]): { v4: string[]; v6: string[] } {
  const v4: string[] = [];
  const v6: string[] = [];
  for (const cidr of addresses) {
    const addr = cidr.split("/")[0] ?? "";
    if (addr.includes(":")) {
      if (!/^fe[89ab][0-9a-f]:/i.test(addr)) v6.push(addr);
    } else if (addr !== "") {
      v4.push(addr);
    }
  }
  return { v4, v6 };
}

// networkRows are the Network group's lines: per shown interface a Connection
// row that names it, its addresses, and for Wi-Fi the network name, signal and
// band, each left out when unknown. Keys carry the interface name so several
// interfaces never collide.
export function networkRows(sys: SystemInfo): InfoRow[] {
  const shown = shownInterfaces(sys);
  if (shown.length === 0) {
    return [{ group: "net", key: "net:Connection", label: "Connection", value: "No wired or Wi-Fi link" }];
  }
  const rows: InfoRow[] = [];
  for (const n of shown) {
    const add = (label: InfoLabel, value: string): void => {
      rows.push({ group: "net", key: `net:${n.name}:${label}`, label, value });
    };
    add("Connection", `${n.kind === "wifi" ? "Wi-Fi" : "Ethernet"} (${n.name})`);
    const { v4, v6 } = displayAddresses(n.addresses);
    if (v4.length > 0) add("IPv4", v4.join(", "));
    if (v6.length > 0) add("IPv6", v6.join(", "));
    const w = n.kind === "wifi" ? n.wifi : undefined;
    if (w?.ssid) add("Wi-Fi Network", w.ssid);
    if (w?.signalDbm !== undefined) add("Signal", signalText(w.signalDbm));
    if (w?.frequencyMhz !== undefined) add("Band", bandText(wifiBand(w.frequencyMhz)));
  }
  return rows;
}

// networkSummary is one interface's line for Copy System Details: its type,
// signal and band, and never an address, network name or MAC.
export function networkSummary(n: NetworkInterface): string {
  if (n.kind !== "wifi") return "Ethernet";
  const parts = ["Wi-Fi"];
  const w = n.wifi;
  if (w?.signalDbm !== undefined) parts.push(signalText(w.signalDbm));
  if (w?.frequencyMhz !== undefined) parts.push(wifiBand(w.frequencyMhz).band);
  return parts.join(", ");
}

// infoRows lists the System Information lines in display order from whichever
// of the system and status reads have arrived. Three groups: Hardware holds the
// physical machine's specs, Network the links that are up, Software the OS and
// appliance build. The live gauges (CPU %, memory, temp and disk usage) live in
// the telemetry tiles, so this card carries static facts and shows the memory
// and storage TOTALS rather than their usage. The two formatters are passed in,
// as lastCheckText takes its own, so this module does not import lib/ui.ts.
export function infoRows(
  sys: SystemInfo | null,
  st: ApplianceStatus | null,
  now: number,
  formatUptime: (seconds: number) => string,
  formatRelative: (fromMs: number, toMs: number) => string,
): InfoRow[] {
  const rows: InfoRow[] = [];
  if (sys) {
    rows.push(row("hw", "Platform", sys.platform || "-"));
    if (sys.cpuModel || sys.cpuCores) {
      const cpu = sys.cpuModel
        ? `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`
        : `${sys.cpuCores} cores`;
      rows.push(row("hw", "CPU", cpu));
    }
    if (sys.memTotalBytes > 0) rows.push(row("hw", "Memory", formatByteSize(sys.memTotalBytes)));
    if (sys.diskTotalBytes > 0) rows.push(row("hw", "Storage", formatByteSize(sys.diskTotalBytes)));
    rows.push(...networkRows(sys));
    rows.push(row("sw", "Hostname", sys.hostname || "-"));
    if (sys.os) rows.push(row("sw", "OS", sys.os));
    if (sys.kernel) rows.push(row("sw", "Kernel", sys.kernel));
  }
  if (st) rows.push(row("sw", "Version", st.version || "-"));
  // A build that names no release never checks, so it has no release rows.
  const u = sys?.update;
  if (u?.supported) {
    rows.push(row("sw", "Latest Release", u.latestVersion ?? "-"));
    rows.push(row("sw", "Last Check", lastCheckText(u.lastCheck, now, formatRelative)));
  }
  if (st) rows.push(row("sw", "Uptime", formatUptime(st.uptimeSeconds)));
  return rows;
}

// INFO_ORDER is every System Information row in display order: the rows a
// release build on a host that reports every fact shows, with the Network
// group as the two rows every connected appliance has.
export const INFO_ORDER: readonly InfoRow[] = [
  row("hw", "Platform", ""),
  row("hw", "CPU", ""),
  row("hw", "Memory", ""),
  row("hw", "Storage", ""),
  { group: "net", key: "net:Connection", label: "Connection", value: "" },
  { group: "net", key: "net:IPv4", label: "IPv4", value: "" },
  row("sw", "Hostname", ""),
  row("sw", "OS", ""),
  row("sw", "Kernel", ""),
  row("sw", "Version", ""),
  row("sw", "Latest Release", ""),
  row("sw", "Last Check", ""),
  row("sw", "Uptime", ""),
];

// withPlaceholders fills the rows a read has not supplied yet with empty
// values, in display order, so a read fills rows in place instead of growing
// the card. It lays out the rows of a release build on a host that reports
// every fact; a host or build without some of them drops those rows once both
// reads are in, rather than keeping rows that would read as missing values.
// The Network group's own rows depend on the host's interfaces, so once the
// system read is in they replace its placeholders wholesale.
export function withPlaceholders(rows: readonly InfoRow[]): InfoRow[] {
  const have = new Map(rows.map((r) => [r.key, r]));
  const net = rows.filter((r) => r.group === "net");
  const out: InfoRow[] = [];
  let netPlaced = false;
  for (const slot of INFO_ORDER) {
    if (slot.group !== "net") {
      out.push(have.get(slot.key) ?? slot);
    } else if (!netPlaced) {
      netPlaced = true;
      out.push(...(net.length > 0 ? net : INFO_ORDER.filter((r) => r.group === "net")));
    }
  }
  return out;
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
