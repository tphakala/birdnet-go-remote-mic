// Pins the toast glyph table and the notification severity mapping: info is a
// neutral glyph, distinct from the green check an action confirmation uses.

import test from "node:test";
import assert from "node:assert/strict";

import { TOAST_ICONS } from "../src/components/toast.ts";
import { TOAST_TYPES } from "../src/lib/toast-core.ts";
import { SEVERITY_TO_TOAST } from "../src/components/notification-row.ts";

test("every toast type has its own glyph, so info never shows the success check", () => {
  const glyphs = Object.values(TOAST_ICONS);
  assert.equal(new Set(glyphs).size, glyphs.length);
});

test("TOAST_TYPES and TOAST_ICONS name the same types", () => {
  assert.deepEqual([...TOAST_TYPES].sort(), Object.keys(TOAST_ICONS).sort());
});

test("notification severities map to info, warn and error, never to the ok confirmation type", () => {
  assert.deepEqual(SEVERITY_TO_TOAST, { error: "error", warning: "warn", info: "info" });
});
