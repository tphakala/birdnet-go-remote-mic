// Unit tests for the text helpers in lib/text.ts. Run with node:test (see
// web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { deviceIdTitle, sentence } from "../src/lib/text.ts";
import { DEVICE_FIELD_LABELS } from "../src/lib/device-settings-core.ts";

test("sentence capitalises and ends a backend message once", () => {
  assert.equal(sentence("timeout"), "Timeout.");
  assert.equal(sentence("already ends."), "Already ends.");
  assert.equal(sentence("asks?"), "Asks?");
  assert.equal(sentence("dangles:"), "Dangles.");
  assert.equal(sentence("  "), "");
  assert.equal(sentence(undefined), "");
});

test("deviceIdTitle names the id under the form's label", () => {
  assert.equal(deviceIdTitle("usb-Mic_123-00"), `${DEVICE_FIELD_LABELS.device}: usb-Mic_123-00`);
});
