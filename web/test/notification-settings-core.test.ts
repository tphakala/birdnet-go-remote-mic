// Unit tests for the pure Notifications settings-card logic. Run with Node's
// built-in test runner over the compiled output (see the web:test task): no
// browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import {
  NOTIFY_FIELDS,
  buildNotificationsPatch,
  fieldForServerPath,
  parseThreshold,
} from "../src/lib/notification-settings-core.js";

test("NOTIFY_FIELDS covers the five audio and eight host thresholds with unique keys and paths", () => {
  assert.equal(NOTIFY_FIELDS.length, 13);
  assert.equal(NOTIFY_FIELDS.filter((f) => f.group === "audio").length, 5);
  assert.equal(NOTIFY_FIELDS.filter((f) => f.group === "host").length, 8);
  const keys = new Set(NOTIFY_FIELDS.map((f) => f.key));
  const paths = new Set(NOTIFY_FIELDS.map((f) => f.server));
  assert.equal(keys.size, 13, "field keys are unique");
  assert.equal(paths.size, 13, "server paths are unique");
  for (const f of NOTIFY_FIELDS) {
    assert.ok(f.min < f.max, `${f.key} min below max`);
    assert.ok(f.server.startsWith(`notifications.${f.group}.`), `${f.key} path in its group`);
  }
});

test("parseThreshold accepts whole numbers (including negative), rejects blank and non-integers", () => {
  assert.equal(parseThreshold("90"), 90);
  assert.equal(parseThreshold("  -60 "), -60);
  assert.equal(parseThreshold("0"), 0);
  assert.equal(parseThreshold(""), null);
  assert.equal(parseThreshold("   "), null);
  assert.equal(parseThreshold("12.5"), null);
  assert.equal(parseThreshold("abc"), null);
  assert.equal(parseThreshold("Infinity"), null);
});

test("buildNotificationsPatch nests parsed integers under audio and host with the enabled flag", () => {
  const values: Record<string, string> = {};
  for (const f of NOTIFY_FIELDS) values[f.key] = String(f.min);
  const patch = buildNotificationsPatch(false, values);
  assert.equal(patch.enabled, false);
  assert.ok(patch.audio && patch.host);
  // Spot-check one field per group and confirm the value is a number, not a string.
  assert.equal(patch.audio?.quietDbfs, -99);
  assert.equal(patch.host?.cpuClearPercent, 1);
  assert.equal(typeof patch.host?.memFreeMiB, "number");
  // All thirteen fields are present when every box parses.
  assert.equal(Object.keys(patch.audio ?? {}).length, 5);
  assert.equal(Object.keys(patch.host ?? {}).length, 8);
});

test("buildNotificationsPatch omits a field whose box does not parse and drops an empty group", () => {
  const values: Record<string, string> = { cpuPercent: "95" };
  const patch = buildNotificationsPatch(true, values);
  assert.equal(patch.enabled, true);
  assert.equal(patch.audio, undefined, "no audio field parsed, so the sub-object is dropped");
  assert.deepEqual(patch.host, { cpuPercent: 95 });
});

test("fieldForServerPath resolves every catalogue path and the paired clear paths, null otherwise", () => {
  for (const f of NOTIFY_FIELDS) {
    assert.equal(fieldForServerPath(f.server)?.key, f.key);
  }
  // The "clear below onset" errors reuse the clear field's own path.
  assert.equal(fieldForServerPath("notifications.host.cpu_clear_percent")?.key, "cpuClearPercent");
  assert.equal(fieldForServerPath("notifications.host.temp_clear_celsius")?.key, "tempClearCelsius");
  assert.equal(fieldForServerPath("notifications.host.disk_clear_percent")?.key, "diskClearPercent");
  assert.equal(fieldForServerPath("notifications.audio.unknown"), null);
  assert.equal(fieldForServerPath("devices.0.name"), null);
});
