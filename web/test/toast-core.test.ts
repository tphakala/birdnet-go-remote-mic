// Pins the toast view decision: the default type, the alert region, the
// lifetime and the dismiss button, and that an off-contract type is neutral.

import test from "node:test";
import assert from "node:assert/strict";

import { ERROR_TTL_MS, INFO_TTL_MS, toastView } from "../src/lib/toast-core.ts";

test("an omitted type is an ok toast in the polite region, gone in 3.2 s, with no dismiss button", () => {
  assert.deepEqual(toastView(), { type: "ok", alert: false, ttlMs: INFO_TTL_MS, dismissible: false });
  assert.equal(INFO_TTL_MS, 3200);
});

test("a long duration makes any toast dismissible", () => {
  assert.deepEqual(toastView(undefined, 9000), { type: "ok", alert: false, ttlMs: 9000, dismissible: true });
  // The unconfirmed-save toast: a warning held as long as an error.
  assert.deepEqual(toastView("warn", ERROR_TTL_MS), { type: "warn", alert: false, ttlMs: ERROR_TTL_MS, dismissible: true });
});

test("an error is an alert that stays 8 s and can be dismissed", () => {
  assert.deepEqual(toastView("error"), { type: "error", alert: true, ttlMs: ERROR_TTL_MS, dismissible: true });
  assert.equal(ERROR_TTL_MS, 8000);
  assert.equal(toastView("error", 1000).ttlMs, 1000);
});

test("info stays info", () => {
  assert.equal(toastView("info").type, "info");
});

test("an off-contract type is a polite info toast, never a success or an Object.prototype member", () => {
  for (const type of ["critical", "", "constructor", "__proto__", "toString"]) {
    assert.deepEqual(toastView(type), { type: "info", alert: false, ttlMs: INFO_TTL_MS, dismissible: false }, type);
  }
});
