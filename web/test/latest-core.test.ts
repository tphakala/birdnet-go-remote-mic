// Unit tests for LatestGate, the ordering guard the app store puts on every
// polled read. Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { LatestGate } from "../src/lib/latest-core.js";

test("a slow response applies while a newer one is still in flight", () => {
  const g = new LatestGate();
  const slow = g.begin();
  g.begin(); // the next poll tick starts before the slow read lands
  assert.equal(g.accept(slow), true);
});

test("every response applies when each is overtaken before it lands", () => {
  // The starvation case: each read resolves only after the next has started.
  const g = new LatestGate();
  let prev = g.begin();
  for (let i = 0; i < 5; i++) {
    const next = g.begin();
    assert.equal(g.accept(prev), true, `tick ${i}`);
    prev = next;
  }
});

test("an older response after a newer applied one is dropped", () => {
  const g = new LatestGate();
  const older = g.begin();
  const newer = g.begin();
  assert.equal(g.accept(newer), true);
  assert.equal(g.accept(older), false);
});

test("superseded reports only responses older than the applied one", () => {
  const g = new LatestGate();
  const first = g.begin();
  assert.equal(g.superseded(first), false, "nothing applied yet");
  const older = g.begin();
  const newer = g.begin();
  assert.equal(g.superseded(older), false, "in flight, nothing newer applied");
  assert.equal(g.accept(newer), true);
  assert.equal(g.superseded(older), true);
  assert.equal(g.superseded(newer), false, "the applied token itself");
  const later = g.begin();
  assert.equal(g.superseded(later), false);
});

test("invalidate drops in-flight tokens but accepts later ones", () => {
  const g = new LatestGate();
  const inFlight = g.begin();
  g.invalidate();
  assert.equal(g.accept(inFlight), false);
  assert.equal(g.superseded(inFlight), true);
  const after = g.begin();
  assert.equal(g.accept(after), true);
});
