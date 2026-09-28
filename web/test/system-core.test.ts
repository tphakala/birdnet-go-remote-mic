// Unit tests for the System view's pure tile, information-row and override
// logic. Run with Node's built-in test runner (see the web:test task).

import test from "node:test";
import assert from "node:assert/strict";

import { formatByteSize, INFO_ORDER, infoRows, overrideLines, overridesSignature, TILE_SLOTS, tileSpecs, withPlaceholders } from "../src/lib/system-core.ts";
import { CERT_LABELS, certRows } from "../src/lib/certificate-core.ts";
import type { ApplianceStatus, CertificateInfo, SystemInfo, UpdateStatus } from "../src/lib/types.ts";

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

test("infoRows orders both columns and adds the release rows only when updates are supported", () => {
  const now = Date.parse("2026-09-01T00:00:10Z");
  const rows = infoRows(sysInfo({ update }), status(), now, uptime, relative);
  assert.deepEqual(rows.filter((r) => r.group === "hw").map((r) => r.label), ["Platform", "CPU", "Memory", "Storage"]);
  assert.deepEqual(
    rows.filter((r) => r.group === "sw").map((r) => r.label),
    ["Hostname", "OS", "Kernel", "Version", "Latest Release", "Last Check", "Uptime"],
  );
  const value = (label: string): string | undefined => rows.find((r) => r.label === label)?.value;
  assert.equal(value("CPU"), "Cortex-A72 (4 cores)");
  assert.equal(value("Memory"), "4.0 GB");
  assert.equal(value("Last Check"), "10000 ms ago");
  assert.equal(value("Uptime"), "up 3600");

  const dev = infoRows(sysInfo({ update: { ...update, supported: false } }), status(), now, uptime, relative);
  assert.equal(dev.some((r) => r.label === "Latest Release" || r.label === "Last Check"), false);
});

test("infoRows shows what arrived: status alone, or system alone", () => {
  assert.deepEqual(infoRows(null, status(), 0, uptime, relative).map((r) => r.label), ["Version", "Uptime"]);
  const sysOnly = infoRows(sysInfo({ os: undefined, kernel: undefined, cpuModel: undefined }), null, 0, uptime, relative);
  assert.deepEqual(sysOnly.map((r) => r.label), ["Platform", "CPU", "Memory", "Storage", "Hostname"]);
  assert.equal(sysOnly.find((r) => r.label === "CPU")?.value, "4 cores");
  assert.deepEqual(infoRows(null, null, 0, uptime, relative), []);
});

test("INFO_ORDER is the order infoRows uses for a release build on a full host", () => {
  const rows = infoRows(sysInfo({ update }), status(), 0, uptime, relative);
  assert.deepEqual(rows.map((r) => [r.group, r.label]), INFO_ORDER.map((r) => [r.group, r.label]));
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
