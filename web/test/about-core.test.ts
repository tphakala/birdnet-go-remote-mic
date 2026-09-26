// Unit tests for the About page's pure helpers (lib/about-core.ts): the
// licenses.json validator, including against the committed file that
// tools/licensegen writes, the component title, and the copyable system
// details. Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { componentTitle, parseLicenseDoc, supportDetails } from "../src/lib/about-core.js";
import type { ApplianceStatus, SystemInfo } from "../src/lib/types.js";

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
