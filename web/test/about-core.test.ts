// Unit tests for the About page's pure helpers (lib/about-core.ts): the
// licenses.json validator, including against the committed file that
// tools/licensegen writes, the component title, and the copyable system
// details. Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { componentTitle, parseLicenseDoc, supportDetails } from "../src/lib/about-core.js";
import type { ApplianceStatus, Device, SystemInfo } from "../src/lib/types.js";

// The compiled test runs from web/.test-out/test/, so web/ is two levels up.
const LICENSES_JSON = fileURLToPath(new URL("../../static/licenses.json", import.meta.url).href);

const entry = (over: Record<string, unknown> = {}): Record<string, unknown> => ({
  name: "example.com/a",
  version: "v1.0.0",
  license: "MIT",
  files: [{ name: "LICENSE", text: "Permission is hereby granted" }],
  ...over,
});

test("the committed licenses.json parses, with remote-mic first and every component licensed", () => {
  const doc = parseLicenseDoc(JSON.parse(readFileSync(LICENSES_JSON, "utf8")));
  assert.ok(doc, "licenses.json does not match the shape the About page reads; run task licenses:generate");
  assert.equal(doc.project.name, "remote-mic");
  assert.equal(doc.project.license, "Apache-2.0");
  assert.deepEqual(doc.project.files.map((f) => f.name), ["LICENSE", "NOTICE"]);
  assert.ok(doc.components.length > 0);
  for (const c of doc.components) {
    assert.notEqual(c.license, "", `${c.name} has no license name`);
    assert.ok(c.files.every((f) => f.text.length > 0), `${c.name} has an empty license text`);
  }
});

test("parseLicenseDoc keeps a valid document and drops an empty version", () => {
  const doc = parseLicenseDoc({ project: entry({ name: "remote-mic", version: undefined }), components: [entry(), entry({ version: "" })] });
  assert.ok(doc);
  assert.equal(doc.project.version, undefined);
  assert.equal(doc.components[0].version, "v1.0.0");
  assert.equal("version" in doc.components[1], false);
});

test("parseLicenseDoc rejects anything but the generator's shape", () => {
  const bad: unknown[] = [
    null,
    "<html>proxy error</html>",
    [],
    { components: [] }, // no project
    { project: entry(), components: "x" },
    { project: entry({ files: [] }), components: [] }, // no license text
    { project: entry(), components: [entry({ license: 3 })] },
    { project: entry(), components: [entry({ version: 1 })] },
    { project: entry(), components: [entry({ files: [{ name: "LICENSE" }] })] },
    { project: entry(), components: [entry({ name: 3 })] },
    { project: entry(), components: [entry({ files: [{ text: "x" }] })] },
    { project: entry(), components: [entry({ files: undefined })] },
  ];
  for (const v of bad) assert.equal(parseLicenseDoc(v), null, JSON.stringify(v));
});

test("componentTitle adds the version only when there is one", () => {
  assert.equal(componentTitle({ name: "a", version: "v1", license: "MIT", files: [] }), "a v1");
  assert.equal(componentTitle({ name: "Inter (web UI font)", license: "OFL-1.1", files: [] }), "Inter (web UI font)");
});

test("supportDetails lists the build and host without identifying details", () => {
  const status = { version: "v0.3.0" } as ApplianceStatus;
  const system = {
    platform: "linux/arm64",
    os: "Debian GNU/Linux 13",
    kernel: "6.12.0",
    hostname: "field-mic",
    cpuModel: "Cortex-A53",
    cpuCores: 4,
    network: [{ name: "wlan0", up: true, addresses: ["192.0.2.10"], rxBytes: 0, txBytes: 0 }],
  } as SystemInfo;
  const got = supportDetails(status, system);
  assert.equal(got, "remote-mic version: v0.3.0\nPlatform: linux/arm64\nOS: Debian GNU/Linux 13\nKernel: 6.12.0\nCPU: Cortex-A53 (4 cores)");
  assert.ok(!got.includes("field-mic") && !got.includes("192.0.2.10"));
  assert.equal(supportDetails(null, null), "remote-mic version: unknown");
});

test("supportDetails leaves out what the host does not report", () => {
  const status = { version: "" } as ApplianceStatus;
  const bare = { platform: "", hostname: "h", cpuCores: 0, network: [] } as unknown as SystemInfo;
  assert.equal(supportDetails(status, bare), "remote-mic version: unknown\nPlatform: unknown");
  const noCores = { platform: "linux/arm", cpuModel: "ARMv6", cpuCores: 0, hostname: "h", network: [] } as unknown as SystemInfo;
  assert.equal(supportDetails(status, noCores), "remote-mic version: unknown\nPlatform: linux/arm\nCPU: ARMv6");
});

// device builds a serving device record with every required field defaulted, so
// a test sets only what it is about.
const device = (over: Partial<Device> & { name: string }): Device => ({
  device: "hw:CARD=Loopback,DEV=1",
  path: `/${over.name}`,
  mode: "pcm",
  format: "s16",
  rate: 48000,
  channels: [1],
  state: "serving",
  clientConnected: false,
  droppedFrames: 0,
  ...over,
});

// A USB microphone with two streams (Opus on channel 1, PCM on channels 1-2),
// and a card-index device that is down. The API lists them in reverse name
// order, which the output must not follow.
const usbMic = device({
  name: "zz-garden",
  device: "usb:1235:8218:s=SERIAL123:if=0,0",
  path: "/k7Qp2ZxTOKENPATH",
  friendlyName: "Scarlett Solo 4th Gen",
  idStable: true,
  mode: "opus",
  channels: [1],
  streamedChannels: [1, 2],
  negotiatedRate: 48000,
  negotiatedChannels: 2,
  negotiatedFormat: "s32",
  overruns: 3,
  droppedFrames: 12,
});
const downCard = device({
  name: "aa-bats",
  device: "hw:2,0",
  path: "/bats-SECRETPATH",
  idStable: false,
  rate: 384000,
  state: "failed",
  downCause: "open-failed",
  // An error may quote the configured id and path; neither may survive.
  error: "open hw:2,0 for /bats-SECRETPATH: device busy (also saw usb:0d8c:0014:s=OTHERSERIAL:if=0,0)",
  overruns: 0,
});

test("supportDetails lists each capture device, ordered by name, without identifying details", () => {
  const status = { version: "v0.3.0" } as ApplianceStatus;
  const got = supportDetails(status, null, [usbMic, downCard]);
  assert.equal(
    got,
    [
      "remote-mic version: v0.3.0",
      "",
      "Capture devices: 2",
      "",
      "Device 1: (no name reported)",
      "  ID: card index, not stable (can change after a reboot or replug)",
      "  State: failed (open-failed): open <device id> for <path>: device busy (also saw usb:0d8c:0014)",
      "  Configured: 384000 Hz, channels 1",
      "  First stream: PCM, channels 1",
      "  Overruns: 0, dropped frames: 0",
      "",
      "Device 2: Scarlett Solo 4th Gen",
      "  ID: USB 1235:8218, stable",
      "  State: serving",
      "  Configured: 48000 Hz, channels 1,2",
      "  Negotiated: 48000 Hz, 2 channels, s32",
      "  First stream: Opus, channels 1",
      "  Overruns: 3, dropped frames: 12",
    ].join("\n"),
  );
  // Order follows the name, not the API's order.
  assert.equal(supportDetails(status, null, [downCard, usbMic]), got);
});

test("supportDetails never includes a USB serial, stream path, or device name", () => {
  const got = supportDetails(null, null, [usbMic, downCard]);
  for (const secret of ["SERIAL123", "OTHERSERIAL", "s=", "TOKENPATH", "SECRETPATH", "zz-garden", "aa-bats", "hw:2,0"]) {
    assert.ok(!got.includes(secret), `details leak ${JSON.stringify(secret)}:\n${got}`);
  }
});

test("supportDetails reports an empty device list and omits the section when devices are unknown", () => {
  const status = { version: "v0.3.0" } as ApplianceStatus;
  assert.equal(supportDetails(status, null, []), "remote-mic version: v0.3.0\n\nCapture devices: none");
  assert.equal(supportDetails(status, null), "remote-mic version: v0.3.0");
});

test("supportDetails classifies an id by its form when an older appliance omits idStable", () => {
  const port = device({ name: "a", device: "usb:16D0:06F3:p=0000:00:14.0-3:if=0,0", overruns: undefined });
  const got = supportDetails(null, null, [port, device({ name: "b" }), device({ name: "c", device: "hw:3" })]);
  assert.ok(/Device 1: .*\n  ID: USB 16d0:06f3, stable\n/.test(got), got);
  assert.ok(/Device 2: .*\n  ID: ALSA card name, stable\n/.test(got), got);
  assert.ok(/Device 3: .*\n  ID: card index, not stable/.test(got), got);
  assert.ok(/Overruns: not reported, dropped frames: 0/.test(got), got);
  assert.ok(!got.includes("14.0-3"), "the USB port path leaked");
});
