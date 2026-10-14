// Pins the toast glyph table and the notification severity mapping: info is a
// neutral glyph, distinct from the green check an action confirmation uses.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { TOAST_ICONS } from "../src/components/toast.ts";
import { SEVERITY_TO_TOAST } from "../src/components/notification-row.ts";

test("every toast type has its own glyph, so info never shows the success check", () => {
  const glyphs = Object.values(TOAST_ICONS);
  assert.equal(new Set(glyphs).size, glyphs.length);
});

test("notification severities map to info, warn and error, never to the ok confirmation type", () => {
  assert.deepEqual(SEVERITY_TO_TOAST, { error: "error", warning: "warn", info: "info" });
});

test("the dashboard telemetry tiles carry a unit element for the temperature and CPU readings", () => {
  const html = readFileSync(fileURLToPath(new URL("../static/index.html", import.meta.url)), "utf8");
  const dashboard = readFileSync(fileURLToPath(new URL("../src/views/dashboard.ts", import.meta.url)), "utf8");
  // dashboard.ts hides each unit by id when its reading is n/a.
  for (const [valueId, unitId] of [["soc-temp-display", "soc-temp-unit"], ["cpu-load-display", "cpu-load-unit"]]) {
    const cell = new RegExp(`id="${valueId}"[^>]*>[^<]*</span>\\s*<span class="telemetry-unit" id="${unitId}">`);
    assert.ok(cell.test(html), `${unitId} must follow ${valueId} in index.html`);
    assert.ok(dashboard.includes(`show("${valueId}", "${unitId}"`), `dashboard.ts must hide ${unitId} through show()`);
  }
});
