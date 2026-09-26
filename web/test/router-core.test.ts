// Unit tests for lib/router-core.ts: which hash fragments name a route, and the
// document title per route. Run with node:test over the compiled output (see
// web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { documentTitle, isViewName } from "../src/lib/router-core.js";

test("isViewName accepts each route", () => {
  for (const v of ["dashboard", "events", "system", "about"]) {
    assert.equal(isViewName(v), true, v);
  }
});

test("isViewName rejects inherited names, near misses and undefined", () => {
  for (const v of ["toString", "__proto__", "constructor", "hasOwnProperty", "valueOf", "", "Dashboard", "dashboard/", "settings"]) {
    assert.equal(isViewName(v), false, v);
  }
  assert.equal(isViewName(undefined), false);
});

test("documentTitle names the view, then the app", () => {
  assert.equal(documentTitle("events"), "Events - BirdNET-Go Remote Mic");
  assert.equal(documentTitle("about"), "About - BirdNET-Go Remote Mic");
});
