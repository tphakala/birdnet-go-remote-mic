// Unit tests for KeyedList (lib/keyed-list.ts) over the fake DOM of
// fake-dom.ts: what a sync creates, updates, moves and removes, and the order
// of the hooks around a removal and a rebuild.

import test from "node:test";
import assert from "node:assert/strict";

import { KeyedList, type KeyedListHooks } from "../src/lib/keyed-list.ts";
import { FakeNode, FakeParent, names, parentWith } from "./fake-dom.ts";

interface Item {
  id: string;
  v?: string;
}
interface Entry {
  el: FakeNode;
  v: string;
}

const asEl = (n: FakeNode): Element => n as unknown as Element;

// harness builds a KeyedList of Entry over a fresh FakeParent and counts the
// create and update calls.
function harness(over: Partial<KeyedListHooks<Item, Entry, string>> = {}, parent = new FakeParent()) {
  const counts = { creates: 0, updates: 0 };
  const list = new KeyedList<Item, Entry>(parent as unknown as Element, {
    key: (i) => i.id,
    create: (i) => {
      counts.creates++;
      return { el: new FakeNode(i.id), v: i.v ?? "" };
    },
    nodes: (e) => [asEl(e.el)],
    update: () => {
      counts.updates++;
    },
    ...over,
  });
  return { list, parent, counts };
}

const items = (...ids: string[]): Item[] => ids.map((id) => ({ id }));

test("a second sync with the same items moves no nodes and creates nothing", () => {
  const { list, parent, counts } = harness();
  list.sync(items("a", "b", "c"));
  assert.deepEqual(names(parent), ["a", "b", "c"]);
  const moves = parent.moves;
  const creates = counts.creates;
  const removes = parent.removes;
  counts.updates = 0;
  list.sync(items("a", "b", "c"));
  assert.equal(parent.moves, moves, "steady state must move no node");
  assert.equal(counts.creates, creates, "steady state must create nothing");
  assert.equal(parent.removes, removes, "steady state must remove nothing");
  assert.equal(counts.updates, 3, "update runs once per item");
});

test("sync places new entries in item order around a placeholder that stays last", () => {
  const placeholder = new FakeNode("placeholder");
  const parent = parentWith(placeholder);
  const { list } = harness({}, parent);
  list.sync(items("b", "d"));
  assert.deepEqual(names(parent), ["b", "d", "placeholder"]);
  list.sync(items("a", "b", "c", "d"));
  assert.deepEqual(names(parent), ["a", "b", "c", "d", "placeholder"]);
  list.sync([]);
  assert.deepEqual(names(parent), ["placeholder"], "an unmanaged child is never removed");
});

test("the remove hook sees the entry's node still in the parent, then the node is gone", () => {
  const seen: { connected: boolean; key: string; before: readonly string[] }[] = [];
  const { list, parent } = harness({
    remove: (e, key, before) => seen.push({ connected: e.el.parent !== null, key, before }),
  });
  list.sync(items("a", "b", "c"));
  list.sync(items("a", "c"));
  assert.deepEqual(seen, [{ connected: true, key: "b", before: ["a", "b", "c"] }]);
  assert.deepEqual(names(parent), ["a", "c"]);
});

test("reads inside a remove hook see the list before this sync", () => {
  const inside: { got: boolean; size: number; keys: string[] }[] = [];
  const holder: { list?: KeyedList<Item, Entry> } = {};
  const { list } = harness({
    remove: (_e, key) => {
      const l = holder.list;
      if (l) inside.push({ got: l.get(key) !== undefined, size: l.size, keys: [...l.keys()] });
    },
  });
  holder.list = list;
  list.sync(items("a", "b", "c"));
  list.sync(items("c", "d"));
  assert.deepEqual(inside, [
    { got: true, size: 3, keys: ["a", "b", "c"] },
    { got: true, size: 3, keys: ["a", "b", "c"] },
  ]);
  assert.deepEqual([...list.keys()], ["c", "d"]);
});

test("a stale entry is rebuilt: create, then replace with old and new, then the old node is removed", () => {
  const log: string[] = [];
  const { list, parent } = harness({
    stale: (e, i) => e.v !== (i.v ?? ""),
    replace: (o, n) => log.push(`replace ${o.v}->${n.v} oldConnected=${o.el.parent !== null}`),
  });
  list.sync([{ id: "a", v: "1" }, { id: "b", v: "1" }]);
  const oldA = list.get("a");
  list.sync([{ id: "a", v: "2" }, { id: "b", v: "1" }]);
  assert.deepEqual(log, ["replace 1->2 oldConnected=true"]);
  assert.equal(oldA?.el.parent, null, "the old node is gone");
  assert.equal(list.get("a")?.v, "2");
  assert.deepEqual(names(parent), ["a", "b"]);
  assert.equal(parent.kids.length, 2);
});

test("a replace hook that swaps in place leaves nothing to move", () => {
  const { list, parent } = harness({
    stale: (e, i) => e.v !== (i.v ?? ""),
    replace: (o, n) => o.el.replaceWith(n.el),
  });
  list.sync([{ id: "a", v: "1" }, { id: "b", v: "1" }]);
  const moves = parent.moves;
  const removes = parent.removes;
  list.sync([{ id: "a", v: "2" }, { id: "b", v: "1" }]);
  assert.equal(parent.moves, moves, "the replaced node is already in place");
  assert.equal(parent.removes, removes, "the old node was already gone");
  assert.deepEqual(names(parent), ["a", "b"]);
});

test("a retained entry leaves the DOM and comes back without create", () => {
  const { list, parent, counts } = harness({ retain: (_e, k) => k === "b" });
  list.sync(items("a", "b"));
  const b = list.get("b");
  list.sync(items("a"));
  assert.deepEqual(names(parent), ["a"]);
  assert.equal(list.get("b"), undefined, "get returns shown entries only");
  assert.equal(list.size, 1);
  const creates = counts.creates;
  list.sync(items("a", "b"));
  assert.equal(counts.creates, creates, "a held entry is revived, not created");
  assert.equal(list.get("b"), b);
  assert.deepEqual(names(parent), ["a", "b"]);
});

test("a held entry is dropped once it is no longer retained", () => {
  let keep = true;
  const removed: string[] = [];
  const { list, counts } = harness({ retain: () => keep, remove: (_e, k) => removed.push(k) });
  list.sync(items("a", "b"));
  list.sync(items("a"));
  assert.deepEqual(removed, [], "holding is not removing");
  keep = false;
  list.sync(items("a"));
  assert.deepEqual(removed, ["b"]);
  const creates = counts.creates;
  list.sync(items("a", "b"));
  assert.equal(counts.creates, creates + 1, "a dropped entry is created again");
});

test("nodes is read on every sync", () => {
  const { list, parent, counts } = harness();
  list.sync(items("a", "b"));
  const a = list.get("a");
  assert.ok(a);
  const swapped = new FakeNode("a2");
  a.el.replaceWith(swapped);
  a.el = swapped;
  const moves = parent.moves;
  const creates = counts.creates;
  list.sync(items("a", "b"));
  assert.equal(parent.moves, moves, "the swapped node is already in place");
  assert.equal(counts.creates, creates);
  assert.deepEqual(names(parent), ["a2", "b"]);
});

test("entries with two nodes keep them adjacent and in order", () => {
  const parent = new FakeParent();
  const list = new KeyedList<Item, { dt: FakeNode; dd: FakeNode }>(parent as unknown as Element, {
    key: (i) => i.id,
    create: (i) => ({ dt: new FakeNode(`${i.id}-dt`), dd: new FakeNode(`${i.id}-dd`) }),
    nodes: (e) => [asEl(e.dt), asEl(e.dd)],
  });
  list.sync(items("a", "b"));
  assert.deepEqual(names(parent), ["a-dt", "a-dd", "b-dt", "b-dd"]);
  list.sync(items("b", "c", "a"));
  assert.deepEqual(names(parent), ["b-dt", "b-dd", "c-dt", "c-dd", "a-dt", "a-dd"]);
  list.sync(items("c"));
  assert.deepEqual(names(parent), ["c-dt", "c-dd"]);
});

test("clear removes every node and calls remove, for shown and held entries", () => {
  const removed: string[] = [];
  const { list, parent } = harness({ retain: (_e, k) => k === "c", remove: (_e, k) => removed.push(k) });
  list.sync(items("a", "b", "c"));
  list.sync(items("a", "b"));
  list.clear();
  assert.deepEqual(removed.sort(), ["a", "b", "c"]);
  assert.deepEqual(names(parent), []);
  assert.equal(list.size, 0);
  list.sync(items("c"));
  assert.deepEqual(names(parent), ["c"], "a cleared held entry is created afresh");
});

test("keys iterate in the last sync's item order", () => {
  const { list } = harness();
  list.sync(items("a", "b", "c"));
  list.sync(items("c", "a", "d"));
  assert.deepEqual([...list.keys()], ["c", "a", "d"]);
  assert.deepEqual([...list.entries()].map(([k, e]) => `${k}:${e.el.name}`), ["c:c", "a:a", "d:d"]);
  assert.deepEqual([...list.values()].map((e) => e.el.name), ["c", "a", "d"]);
});

test("a duplicate key keeps the first item", () => {
  const { list, parent, counts } = harness();
  list.sync([{ id: "a", v: "first" }, { id: "b" }, { id: "a", v: "second" }]);
  assert.equal(counts.creates, 2);
  assert.equal(list.get("a")?.v, "first");
  assert.deepEqual(names(parent), ["a", "b"]);
});
