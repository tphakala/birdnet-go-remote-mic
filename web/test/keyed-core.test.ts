// Unit tests for keyedPlan (lib/keyed-core.ts): the keep, create, rebuild,
// drop and hold decision behind KeyedList, over plain key lists.

import test from "node:test";
import assert from "node:assert/strict";

import { keyedPlan } from "../src/lib/keyed-core.ts";

const never = { stale: () => false, retain: () => false };

test("keyedPlan plans nothing for the same keys in the same order", () => {
  const p = keyedPlan(["a", "b", "c"], [], ["a", "b", "c"], never);
  assert.deepEqual(p, { order: ["a", "b", "c"], create: [], rebuild: [], drop: [], hold: [] });
});

test("keyedPlan creates a new key at its place in the order, not appended", () => {
  const p = keyedPlan(["a", "b", "c"], [], ["a", "x", "b", "c"], never);
  assert.deepEqual(p.order, ["a", "x", "b", "c"]);
  assert.deepEqual(p.create, ["x"]);
  assert.deepEqual([p.rebuild, p.drop, p.hold], [[], [], []]);
});

test("keyedPlan drops a key that is gone, in shown order", () => {
  const p = keyedPlan(["a", "b", "c", "d"], [], ["b"], never);
  assert.deepEqual(p.drop, ["a", "c", "d"]);
  assert.deepEqual(p.order, ["b"]);
  assert.deepEqual([p.create, p.rebuild, p.hold], [[], [], []]);
});

test("keyedPlan rebuilds a stale key and does not also create it", () => {
  const p = keyedPlan(["a", "b"], [], ["a", "b", "c"], { stale: (k) => k === "b" || k === "c", retain: () => false });
  assert.deepEqual(p.rebuild, ["b"]);
  assert.deepEqual(p.create, ["c"]);
});

test("keyedPlan reorders without creating or rebuilding", () => {
  const p = keyedPlan(["a", "b", "c"], [], ["c", "a", "b"], never);
  assert.deepEqual(p.order, ["c", "a", "b"]);
  assert.deepEqual([p.create, p.rebuild, p.drop, p.hold], [[], [], [], []]);
});

test("keyedPlan holds a retained key and drops one that is not retained", () => {
  const p = keyedPlan(["a", "b", "c"], [], ["a"], { stale: () => false, retain: (k) => k === "b" });
  assert.deepEqual(p.hold, ["b"]);
  assert.deepEqual(p.drop, ["c"]);
});

test("keyedPlan revives a held key without creating it, and drops a held key no longer retained", () => {
  const p = keyedPlan(["a"], ["b", "c"], ["a", "b"], { stale: () => false, retain: () => false });
  assert.deepEqual(p.order, ["a", "b"]);
  assert.deepEqual(p.create, [], "a held key comes back without create");
  assert.deepEqual(p.rebuild, []);
  assert.deepEqual(p.drop, ["c"], "a held key that is gone and not retained is dropped");
  assert.deepEqual(p.hold, []);
});

test("keyedPlan keeps a retained held key held", () => {
  const p = keyedPlan(["a"], ["b"], ["a"], { stale: () => false, retain: () => true });
  assert.deepEqual([p.create, p.rebuild, p.drop, p.hold], [[], [], [], []]);
});

test("keyedPlan rebuilds a stale held key", () => {
  const p = keyedPlan([], ["b"], ["b"], { stale: () => true, retain: () => false });
  assert.deepEqual(p.rebuild, ["b"]);
  assert.deepEqual(p.create, []);
});

test("keyedPlan keeps the first of duplicate keys", () => {
  const p = keyedPlan(["a", "b"], [], ["b", "a", "b", "c", "c"], never);
  assert.deepEqual(p.order, ["b", "a", "c"]);
  assert.deepEqual(p.create, ["c"]);
});

test("keyedPlan asks stale only for kept keys and retain only for gone keys", () => {
  const staleAsked: string[] = [];
  const retainAsked: string[] = [];
  keyedPlan(["a", "b"], ["h"], ["b", "n", "h"], {
    stale: (k) => (staleAsked.push(k), false),
    retain: (k) => (retainAsked.push(k), false),
  });
  assert.deepEqual(staleAsked.sort(), ["b", "h"], "a new key is created, so it is never asked");
  assert.deepEqual(retainAsked, ["a"], "a kept key is never asked");
});

// A table of list sequences stands in for a property test: apply each plan to
// a simulated shown/held state and check what must hold after every step. "c"
// is always retained; "b" is stale at step 3 only.
test("keyedPlan across a sequence of lists", () => {
  const seq: string[][] = [["a", "b", "c"], ["a", "b", "c"], ["a", "x", "b", "c"], ["c", "b", "a"], ["b"], [], ["a", "b"]];
  let shown: string[] = [];
  let held: string[] = [];
  for (const [i, next] of seq.entries()) {
    const isStale = (k: string) => k === "b" && i === 3;
    const p = keyedPlan(shown, held, next, { stale: isStale, retain: (k) => k === "c" });
    const known = new Set([...shown, ...held]);
    for (const k of p.create) assert.ok(!known.has(k), `step ${i}: ${k} was created though known`);
    for (const k of next) {
      if (known.has(k) && !isStale(k)) {
        assert.ok(!p.create.includes(k) && !p.rebuild.includes(k), `step ${i}: ${k} was kept`);
      }
    }
    held = held.filter((k) => !p.drop.includes(k) && !p.order.includes(k)).concat(p.hold);
    shown = p.order;
    assert.deepEqual(shown, [...new Set(next)], `step ${i}: shown is the deduped next`);
    for (const k of p.drop) assert.ok(!held.includes(k), `step ${i}: dropped ${k} is held`);
    assert.ok(held.every((k) => !shown.includes(k)), `step ${i}: a key is both shown and held`);
    if (i === 3) assert.deepEqual(p.rebuild, ["b"]);
  }
  assert.deepEqual(shown, ["a", "b"]);
  assert.deepEqual(held, ["c"], "c was retained each time it left");
});
