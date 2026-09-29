// Unit tests for the System view's pure tile, information-row and override
// logic. Run with Node's built-in test runner (see the web:test task).

import test from "node:test";
import assert from "node:assert/strict";

import {
  displayAddresses, formatByteSize, INFO_ORDER, infoRows, networkRows, networkSummary, overrideLines, overridesSignature,
  shownInterfaces, signalQuality, TILE_SLOTS, tileSpecs, wifiBand, withPlaceholders,
} from "../src/lib/system-core.ts";
import { CERT_LABELS, certRows } from "../src/lib/certificate-core.ts";
import type { ApplianceStatus, CertificateInfo, NetworkInterface, SystemInfo, UpdateStatus } from "../src/lib/types.ts";

const GiB = 1073741824;
const MiB = 1048576;

function sysInfo(over: Partial<SystemInfo> = {}): SystemInfo {
  return {
    platform: "Raspberry Pi 4 Model B",
    os: "Debian GNU/Linux 13",
    kernel: "6.12.0",
    hostname: "mic",
    cpuModel: "Cortex-A72",
    cpuCores: 4,
    cpuPercent: 12.34,
    memTotalBytes: 4 * GiB,
    memUsedBytes: 512 * MiB,
    diskTotalBytes: 32 * GiB,
    diskUsedBytes: 8 * GiB,
    tempCelsius: 42.5,
    network: [],
    ...over,
  };
}

function status(over: Partial<ApplianceStatus> = {}): ApplianceStatus {
  return {
    version: "v1.2.3",
    uptimeSeconds: 3600,
    rtspListen: ":8554",
    discoveryEnabled: true,
    authRequired: false,
    devicesServing: 1,
    devicesTotal: 1,
    ...over,
  };
}

const update: UpdateStatus = {
  currentVersion: "v1.2.3",
  supported: true,
  checkEnabled: true,
  available: false,
  canApply: true,
  installMethod: "service",
  phase: "idle",
  latestVersion: "v1.2.3",
  lastCheck: "2026-09-01T00:00:00Z",
};

const uptime = (s: number): string => `up ${s}`;
const relative = (from: number, to: number): string => `${to - from} ms ago`;

test("formatByteSize uses GB from one GiB, whole numbers from ten, and MB below", () => {
  assert.equal(formatByteSize(512 * MiB), "512 MB");
  assert.equal(formatByteSize(GiB), "1.0 GB");
  assert.equal(formatByteSize(3.5 * GiB), "3.5 GB");
  assert.equal(formatByteSize(31.6 * GiB), "32 GB");
});

test("tileSpecs lists CPU, Memory, Temperature and Disk in order", () => {
  const specs = tileSpecs(sysInfo());
  assert.deepEqual(specs.map((s) => s.key), ["cpu", "mem", "temp", "disk"]);
  const [cpu, mem, temp, disk] = specs;
  assert.deepEqual(cpu, { key: "cpu", label: "CPU Utilization", sub: "4 Cores", value: "12.3", unit: "%", barPct: 12.34 });
  assert.equal(mem?.value, "512");
  assert.equal(mem?.sub, "4.0 GB Total");
  assert.equal(mem?.barPct, 12.5);
  assert.equal(temp?.value, "42.5");
  assert.equal(temp?.barPct, (42.5 / 85) * 100);
  assert.equal(disk?.value, "8.0");
  assert.equal(disk?.barPct, 25);
});

test("TILE_SLOTS lays out the tiles a full host reports, with their labels", () => {
  assert.deepEqual(
    tileSpecs(sysInfo()).map((s) => ({ key: s.key, label: s.label })),
    TILE_SLOTS.map((s) => ({ ...s })),
  );
});

test("tileSpecs drops Memory and Disk without a total and shows n/a without a reading", () => {
  const specs = tileSpecs(sysInfo({ memTotalBytes: 0, diskTotalBytes: 0, cpuPercent: undefined, tempCelsius: undefined, cpuCores: 0 }));
  assert.deepEqual(specs.map((s) => s.key), ["cpu", "temp"]);
  const [cpu, temp] = specs;
  assert.equal(cpu?.value, "n/a");
  assert.equal(cpu?.sub, "");
  assert.equal(cpu?.barPct, undefined);
  assert.equal(temp?.value, "n/a");
  assert.equal(temp?.barPct, undefined);
});

test("infoRows orders the groups and adds the release rows only when updates are supported", () => {
  const now = Date.parse("2026-09-01T00:00:10Z");
  const rows = infoRows(sysInfo({ update }), status(), now, uptime, relative);
  assert.deepEqual(rows.filter((r) => r.group === "hw").map((r) => r.label), ["Platform", "CPU", "Memory", "Storage"]);
  assert.deepEqual(rows.filter((r) => r.group === "net").map((r) => r.label), ["Connection"]);
  assert.deepEqual(
    rows.filter((r) => r.group === "sw").map((r) => r.label),
    ["Hostname", "OS", "Kernel", "Version", "Latest Release", "Last Check", "Next Check", "Uptime"],
  );
  const value = (label: string): string | undefined => rows.find((r) => r.label === label)?.value;
  assert.equal(value("CPU"), "Cortex-A72 (4 cores)");
  assert.equal(value("Memory"), "4.0 GB");
  assert.equal(value("Last Check"), "10000 ms ago");
  assert.equal(value("Next Check"), "Soon", "checks on but not yet scheduled");
  assert.equal(value("Uptime"), "up 3600");

  const dev = infoRows(sysInfo({ update: { ...update, supported: false } }), status(), now, uptime, relative);
  assert.equal(dev.some((r) => r.label === "Latest Release" || r.label === "Last Check" || r.label === "Next Check"), false);
  // Checks off: the row says so instead of counting down.
  const off = infoRows(sysInfo({ update: { ...update, checkEnabled: false, nextCheck: "2026-09-01T00:05:00Z" } }), status(), now, uptime, relative);
  assert.equal(off.find((r) => r.label === "Next Check")?.value, "Off");
  const due = infoRows(sysInfo({ update: { ...update, nextCheck: "2026-09-01T23:47:10Z" } }), status(), now, uptime, relative);
  assert.equal(due.find((r) => r.label === "Next Check")?.value, "in 23h 47m");
});

test("infoRows shows what arrived: status alone, or system alone", () => {
  assert.deepEqual(infoRows(null, status(), 0, uptime, relative).map((r) => r.label), ["Version", "Uptime"]);
  const sysOnly = infoRows(sysInfo({ os: undefined, kernel: undefined, cpuModel: undefined }), null, 0, uptime, relative);
  assert.deepEqual(sysOnly.map((r) => r.label), ["Platform", "CPU", "Memory", "Storage", "Connection", "Hostname"]);
  assert.equal(sysOnly.find((r) => r.label === "CPU")?.value, "4 cores");
  assert.deepEqual(infoRows(null, null, 0, uptime, relative), []);
});

test("INFO_ORDER is the order infoRows uses for a release build on a full host", () => {
  const rows = infoRows(sysInfo({ update, network: [eth("eth0", ["192.0.2.10/24"])] }), status(), 0, uptime, relative);
  assert.deepEqual(rows.map((r) => [r.group, r.label]), INFO_ORDER.map((r) => [r.group, r.label]));
});

test("withPlaceholders replaces the Network placeholders with the interfaces that arrived", () => {
  const sys = sysInfo({ network: [wifi("wlan0", ["192.0.2.10/24"], { ssid: "x" }), eth("eth0", ["192.0.2.11/24"])] });
  const merged = withPlaceholders(infoRows(sys, null, 0, uptime, relative));
  const net = merged.filter((r) => r.group === "net").map((r) => r.key);
  assert.deepEqual(net, [
    "net:wlan0:Connection", "net:wlan0:IPv4", "net:wlan0:Wi-Fi Network",
    "net:eth0:Connection", "net:eth0:IPv4",
  ]);
  // Grouped in INFO_ORDER's place: after Hardware, before Software.
  assert.deepEqual(merged.map((r) => r.group).filter((g, i, a) => g !== a[i - 1]), ["hw", "net", "sw"]);
  assert.equal(new Set(merged.map((r) => r.key)).size, merged.length, "keys are unique");
});

test("withPlaceholders keeps the rows that arrived and fills the rest with empty values in order", () => {
  const partial = infoRows(null, status(), 0, uptime, relative);
  const merged = withPlaceholders(partial);
  assert.deepEqual(merged.map((r) => r.label), INFO_ORDER.map((r) => r.label));
  assert.equal(merged.find((r) => r.label === "Version")?.value, "v1.2.3");
  assert.equal(merged.find((r) => r.label === "Uptime")?.value, "up 3600");
  assert.equal(merged.filter((r) => r.value === "").length, INFO_ORDER.length - 2);
});

test("overrideLines names known fields and shows an empty persisted value as the default", () => {
  assert.deepEqual(
    overrideLines([
      { field: "listen", effective: ":9554", persisted: ":8554" },
      { field: "management.listen", effective: ":9443", persisted: "" },
      { field: "some.other", effective: "a", persisted: "b" },
    ]),
    [
      "RTSP listen address: serving :9554 (config file: :8554)",
      "Management listen address: serving :9443 (config file: (default))",
      "some.other: serving a (config file: b)",
    ],
  );
});

test("overridesSignature changes with any field and is empty for no overrides", () => {
  const a = [{ field: "listen", effective: ":9554", persisted: ":8554" }];
  assert.equal(overridesSignature([]), "");
  assert.equal(overridesSignature(a), overridesSignature(a.map((o) => ({ ...o }))));
  assert.notEqual(overridesSignature(a), overridesSignature([{ field: "listen", effective: ":9554", persisted: "" }]));
  assert.notEqual(overridesSignature(a), overridesSignature([{ field: "listen", effective: ":7554", persisted: ":8554" }]));
  assert.notEqual(overridesSignature(a), overridesSignature([{ field: "management.listen", effective: ":9554", persisted: ":8554" }]));
});

test("certRows follows CERT_LABELS and falls back to a dash for empty name lists", () => {
  const cert: CertificateInfo = {
    subject: "CN=mic",
    issuer: "CN=mic",
    selfSigned: true,
    managed: true,
    dnsNames: [],
    ipAddresses: ["192.0.2.1", "::1"],
    notBefore: "not a time",
    notAfter: "2036-05-30T08:00:00Z",
    fingerprintSha256: "AA",
  };
  const rows = certRows(cert);
  assert.deepEqual(rows.map(([label]) => label), [...CERT_LABELS]);
  const value = new Map(rows);
  assert.equal(value.get("Type"), "Self-issued (subject matches issuer)");
  assert.equal(value.get("DNS names"), "-");
  assert.equal(value.get("IP addresses"), "192.0.2.1, ::1");
  assert.equal(value.get("Valid from"), "not a time");
});

// eth and wifi build an up interface of each kind; addresses are CIDR strings.
function eth(name: string, addresses: string[] = [], over: Partial<NetworkInterface> = {}): NetworkInterface {
  return { name, up: true, addresses, rxBytes: 0, txBytes: 0, kind: "ethernet", ...over };
}
function wifi(name: string, addresses: string[], link: NonNullable<NetworkInterface["wifi"]>, over: Partial<NetworkInterface> = {}): NetworkInterface {
  return { name, up: true, addresses, rxBytes: 0, txBytes: 0, kind: "wifi", wifi: link, ...over };
}

test("signalQuality words every boundary", () => {
  const cases: Array<[number, string]> = [
    [-30, "Excellent"], [-50, "Excellent"], [-51, "Good"], [-60, "Good"], [-61, "Fair"], [-70, "Fair"], [-71, "Weak"], [-95, "Weak"],
  ];
  for (const [dbm, want] of cases) assert.equal(signalQuality(dbm), want, `${dbm} dBm`);
});

test("wifiBand names the band and channel, and falls back to MHz", () => {
  assert.deepEqual(wifiBand(2412), { band: "2.4 GHz", channel: 1 });
  assert.deepEqual(wifiBand(2437), { band: "2.4 GHz", channel: 6 });
  assert.deepEqual(wifiBand(2484), { band: "2.4 GHz", channel: 14 });
  assert.deepEqual(wifiBand(5180), { band: "5 GHz", channel: 36 });
  assert.deepEqual(wifiBand(5220), { band: "5 GHz", channel: 44 });
  assert.deepEqual(wifiBand(5935), { band: "6 GHz", channel: 2 });
  assert.deepEqual(wifiBand(5955), { band: "6 GHz", channel: 1 });
  assert.deepEqual(wifiBand(58320), { band: "58320 MHz" });
  // Inside a band but not on a channel: the band without a number.
  assert.deepEqual(wifiBand(5221), { band: "5 GHz", channel: undefined });
  assert.deepEqual(wifiBand(2400), { band: "2400 MHz" });
});

test("shownInterfaces lists only wired and Wi-Fi links that are up, in API order", () => {
  const sys = sysInfo({
    network: [
      eth("docker0", ["172.17.0.1/16"], { kind: "other" }),
      wifi("wlan0", [], {}),
      eth("eth0", [], { up: false }),
      eth("eth1"),
      eth("mystery", [], { kind: "satellite" }),
      eth("nokind", [], { kind: undefined }),
    ],
  });
  assert.deepEqual(shownInterfaces(sys).map((n) => n.name), ["wlan0", "eth1"]);
});

test("displayAddresses strips prefixes, puts IPv4 first and drops link-local IPv6", () => {
  const got = displayAddresses([
    "fe80::1/64", "2001:db8::1/64", "192.0.2.10/24", "febf::2/64", "fec0::3/64", "fd00::4/64", "198.51.100.7/24", "fe8::5/64",
  ]);
  assert.deepEqual(got.v4, ["192.0.2.10", "198.51.100.7"]);
  assert.deepEqual(got.v6, ["2001:db8::1", "fec0::3", "fd00::4", "fe8::5"]);
  assert.deepEqual(displayAddresses([]), { v4: [], v6: [] });
});

test("networkRows keys rows per interface and omits what is unknown", () => {
  const sys = sysInfo({
    network: [
      wifi("wlan0", ["192.0.2.10/24", "fe80::1/64", "2001:db8::10/64"], { ssid: "garden-ap", signalDbm: -52, frequencyMhz: 5220 }),
      eth("eth0", ["192.0.2.11/24"]),
    ],
  });
  assert.deepEqual(networkRows(sys).map((r) => [r.key, r.label, r.value]), [
    ["net:wlan0:Connection", "Connection", "Wi-Fi (wlan0)"],
    ["net:wlan0:IPv4", "IPv4", "192.0.2.10"],
    ["net:wlan0:IPv6", "IPv6", "2001:db8::10"],
    ["net:wlan0:Wi-Fi Network", "Wi-Fi Network", "garden-ap"],
    ["net:wlan0:Signal", "Signal", "-52 dBm (Good)"],
    ["net:wlan0:Band", "Band", "5 GHz, channel 44"],
    ["net:eth0:Connection", "Connection", "Ethernet (eth0)"],
    ["net:eth0:IPv4", "IPv4", "192.0.2.11"],
  ]);

  const bare = networkRows(sysInfo({ network: [wifi("wlan0", [], {})] }));
  assert.deepEqual(bare.map((r) => r.label), ["Connection"]);
  const onlySignal = networkRows(sysInfo({ network: [wifi("wlan0", ["fe80::1/64"], { signalDbm: -75 })] }));
  assert.deepEqual(onlySignal.map((r) => [r.label, r.value]), [["Connection", "Wi-Fi (wlan0)"], ["Signal", "-75 dBm (Weak)"]]);
});

test("networkRows says so when no wired or Wi-Fi link is up", () => {
  const sys = sysInfo({ network: [eth("eth0", [], { up: false }), eth("docker0", ["172.17.0.1/16"], { kind: "other" })] });
  assert.deepEqual(networkRows(sys).map((r) => [r.key, r.value]), [["net:Connection", "No wired or Wi-Fi link"]]);
});

test("networkSummary carries the link type, signal and band, never an address or network name", () => {
  const w = wifi("wlan0", ["192.0.2.10/24"], { ssid: "garden-ap", signalDbm: -52, frequencyMhz: 5220 }, { mac: "aa:bb:cc:dd:ee:ff" });
  assert.equal(networkSummary(w), "Wi-Fi, -52 dBm (Good), 5 GHz");
  assert.equal(networkSummary(wifi("wlan0", [], { frequencyMhz: 58320 })), "Wi-Fi, 58320 MHz");
  assert.equal(networkSummary(wifi("wlan0", [], {})), "Wi-Fi");
  assert.equal(networkSummary(eth("eth0", ["192.0.2.11/24"])), "Ethernet");
});
