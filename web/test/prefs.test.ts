// Unit tests for OnceNotice (lib/prefs.ts), the once-per-page "preferences not
// saved" notice shared by every per-browser preference. Run with node:test
// over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { OnceNotice } from "../src/lib/prefs.js";

test("the notice shows on the first report only", () => {
  const n = new OnceNotice();
  let shown = 0;
  n.setHandler(() => shown++);
  n.report();
  n.report();
  n.report();
  assert.equal(shown, 1);
});

test("a report before the handler is set is not counted", () => {
  const n = new OnceNotice();
  n.report();
  let shown = 0;
  n.setHandler(() => shown++);
  n.report();
  assert.equal(shown, 1);
});
