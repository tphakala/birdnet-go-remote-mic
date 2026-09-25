// Unit tests for the app store's pure refresh helpers: gatedRefresh (one gated
// read of a polled resource) and ChangeTracker (announce only on change). Run
// with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { LatestGate } from "../src/lib/latest-core.js";
import { ChangeTracker, gatedRefresh } from "../src/lib/store-core.js";

// deferred returns a promise with its resolve and reject exposed, so a test
// controls the order in which overlapping reads land.
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test("gatedRefresh applies a response and reports success", async () => {
  const gate = new LatestGate();
  const applied: number[] = [];
  const ok = await gatedRefresh(gate, () => Promise.resolve(7), (v) => applied.push(v));
  assert.equal(ok, true);
  assert.deepEqual(applied, [7]);
});

test("gatedRefresh drops a superseded response but still reports success", async () => {
  const gate = new LatestGate();
  const applied: string[] = [];
  const older = deferred<string>();
  const newer = deferred<string>();
  const olderRun = gatedRefresh(gate, () => older.promise, (v) => applied.push(v));
  const newerRun = gatedRefresh(gate, () => newer.promise, (v) => applied.push(v));
  newer.resolve("new");
  assert.equal(await newerRun, true);
  older.resolve("old");
  assert.equal(await olderRun, true, "fresher data is in place, so no load error");
  assert.deepEqual(applied, ["new"], "the older body must not overwrite the newer one");
});

test("gatedRefresh applies a slow response while a newer read is still in flight", async () => {
  // The starvation case: dropping a response only because a newer read started
  // would starve the view on a link slower than the poll.
  const gate = new LatestGate();
  const applied: string[] = [];
  const slow = deferred<string>();
  const next = deferred<string>();
  const slowRun = gatedRefresh(gate, () => slow.promise, (v) => applied.push(v));
  const nextRun = gatedRefresh(gate, () => next.promise, (v) => applied.push(v));
  slow.resolve("slow");
  assert.equal(await slowRun, true);
  next.resolve("next");
  assert.equal(await nextRun, true);
  assert.deepEqual(applied, ["slow", "next"]);
});

test("gatedRefresh reports a failure with nothing newer applied", async () => {
  const gate = new LatestGate();
  const errors: unknown[] = [];
  const boom = new Error("boom");
  const ok = await gatedRefresh(
    gate,
    () => Promise.reject(boom),
    () => assert.fail("apply must not run on a failed read"),
    (err) => errors.push(err),
  );
  assert.equal(ok, false);
  assert.deepEqual(errors, [boom], "onError receives the fetch error");
});

test("gatedRefresh treats a failure after a newer applied response as success", async () => {
  const gate = new LatestGate();
  const errors: unknown[] = [];
  const older = deferred<number>();
  const olderRun = gatedRefresh(gate, () => older.promise, () => assert.fail("older must not apply"), (err) => errors.push(err));
  assert.equal(await gatedRefresh(gate, () => Promise.resolve(2), () => {}), true);
  older.reject(new Error("late failure"));
  assert.equal(await olderRun, true, "fresher data is in place");
  assert.equal(errors.length, 1, "the failure is still reported to onError");
});

test("gatedRefresh: a fetch that throws while normalizing never marks its token applied", async () => {
  // A newer read that throws inside fetch (validation before accept) must not
  // drop an older valid response still in flight.
  const gate = new LatestGate();
  const applied: string[] = [];
  const older = deferred<string>();
  const olderRun = gatedRefresh(gate, () => older.promise, (v) => applied.push(v));
  const newerOk = await gatedRefresh(gate, () => Promise.reject(new TypeError("not an array")), (v: string) => applied.push(v));
  assert.equal(newerOk, false);
  older.resolve("valid");
  assert.equal(await olderRun, true);
  assert.deepEqual(applied, ["valid"]);
});

test("gatedRefresh drops a read that was in flight across invalidate", async () => {
  const gate = new LatestGate();
  const applied: string[] = [];
  const inFlight = deferred<string>();
  const run = gatedRefresh(gate, () => inFlight.promise, (v) => applied.push(v));
  gate.invalidate(); // an authoritative write landed (applyConfig)
  inFlight.resolve("stale");
  assert.equal(await run, true);
  assert.deepEqual(applied, []);
});

test("ChangeTracker announces the first value, even an empty list", () => {
  const t = new ChangeTracker();
  assert.equal(t.changed([]), true);
  assert.equal(t.changed([]), false);
});

test("ChangeTracker reports only structural changes", () => {
  const t = new ChangeTracker();
  assert.equal(t.changed({ a: 1, b: [1, 2] }), true);
  assert.equal(t.changed({ a: 1, b: [1, 2] }), false, "a fresh but equal object is unchanged");
  assert.equal(t.changed({ a: 1, b: [1, 3] }), true);
  assert.equal(t.changed({ a: 1, b: [1, 3] }), false);
  assert.equal(t.changed({ a: 1, b: [1, 3], c: true }), true, "an added field is a change");
});

test("ChangeTracker announces again after reset", () => {
  const t = new ChangeTracker();
  assert.equal(t.changed({ uptime: 5 }), true);
  t.reset();
  assert.equal(t.changed({ uptime: 5 }), true, "a failed read re-arms the next announcement");
  assert.equal(t.changed({ uptime: 5 }), false);
});

test("ChangeTracker treats null as a value distinct from an unseen one", () => {
  const t = new ChangeTracker();
  assert.equal(t.changed(null), true);
  assert.equal(t.changed(null), false);
  assert.equal(t.changed({}), true);
});
