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
  const connected: boolean[] = [];
  const { list, counts } = harness({
    retain: () => keep,
    remove: (e, k) => {
      removed.push(k);
      connected.push(e.el.parent !== null);
    },
  });
  list.sync(items("a", "b"));
  list.sync(items("a"));
  assert.deepEqual(removed, [], "holding is not removing");
  keep = false;
  list.sync(items("a"));
  assert.deepEqual(removed, ["b"]);
  assert.deepEqual(connected, [false], "a held entry's node is already out of the parent");
  const creates = counts.creates;
  list.sync(items("a", "b"));
  assert.equal(counts.creates, creates + 1, "a dropped entry is created again");
});

test("update receives the created entry, then the rebuilt entry after a rebuild", () => {
  const updated: Entry[] = [];
  const { list } = harness({
    stale: (e, i) => e.v !== (i.v ?? ""),
    update: (e) => {
      updated.push(e);
    },
  });
  list.sync([{ id: "a", v: "1" }]);
  const created = list.get("a");
  list.sync([{ id: "a", v: "2" }]);
  const rebuilt = list.get("a");
  assert.notEqual(created, rebuilt);
  assert.equal(updated.length, 2);
  assert.equal(updated[0], created);
  assert.equal(updated[1], rebuilt);
});

test("a held entry that went stale while hidden is rebuilt, and replace gets its detached old entry", () => {
  const log: string[] = [];
  const { list, parent, counts } = harness({
    retain: (_e, k) => k === "b",
    stale: (e, i) => e.v !== (i.v ?? ""),
    replace: (o, n) => log.push(`${o.v}->${n.v} connected=${o.el.parent !== null}`),
  });
  list.sync([{ id: "a", v: "1" }, { id: "b", v: "1" }]);
  list.sync([{ id: "a", v: "1" }]);
  const creates = counts.creates;
  list.sync([{ id: "a", v: "1" }, { id: "b", v: "2" }]);
  assert.equal(counts.creates, creates + 1, "the stale held entry is created again");
  assert.deepEqual(log, ["1->2 connected=false"]);
  assert.equal(list.get("b")?.v, "2");
  assert.deepEqual(names(parent), ["a", "b"]);
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
  const connected = new Map<string, boolean>();
  const { list, parent } = harness({
    retain: (_e, k) => k === "c",
    remove: (e, k) => {
      removed.push(k);
      connected.set(k, e.el.parent !== null);
    },
  });
  list.sync(items("a", "b", "c"));
  list.sync(items("a", "b"));
  list.clear();
  assert.deepEqual(removed.sort(), ["a", "b", "c"]);
  assert.deepEqual([...connected].sort(), [["a", true], ["b", true], ["c", false]], "shown nodes are attached, the held one is not");
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

// The tests below make a hook throw and check what the list and the DOM look
// like afterwards. tracker builds a list of versioned entries whose name is
// id+version ("c2"), counts how often each entry object is retired (passed to
// remove, or to replace as the old entry) and throws a sentinel once from the
// armed hook.
interface TItem {
  id: string;
  v: string;
}
interface TEntry {
  id: string;
  v: string;
  el: FakeNode;
}
type Phase = "key" | "stale" | "retain" | "nodes" | "remove" | "create" | "update" | "replaceBefore" | "replaceAfter";

function tracker(swap = false) {
  const placeholder = new FakeNode("placeholder");
  const parent = parentWith(placeholder);
  const all: TEntry[] = [];
  const retired = new Map<TEntry, number>();
  const seenInRemove: { keys: string[]; size: number }[] = [];
  const sentinel = new Error("hook failed");
  let armed: { phase: Phase; name: string } | null = null;
  const fire = (phase: Phase, name: string): void => {
    if (armed?.phase !== phase || armed.name !== name) return;
    armed = null;
    throw sentinel;
  };
  const retire = (e: TEntry): void => {
    retired.set(e, (retired.get(e) ?? 0) + 1);
  };
  const holder: { list?: KeyedList<TItem, TEntry> } = {};
  const list = new KeyedList<TItem, TEntry>(parent as unknown as Element, {
    key: (i) => {
      fire("key", i.id + i.v);
      return i.id;
    },
    create: (i) => {
      fire("create", i.id + i.v);
      const e = { id: i.id, v: i.v, el: new FakeNode(i.id + i.v) };
      all.push(e);
      return e;
    },
    nodes: (e) => {
      fire("nodes", e.el.name);
      return [asEl(e.el)];
    },
    update: (e) => fire("update", e.el.name),
    stale: (e, i) => {
      fire("stale", e.el.name);
      return e.v !== i.v;
    },
    replace: (o, n) => {
      retire(o);
      fire("replaceBefore", o.el.name);
      if (swap) o.el.replaceWith(n.el);
      fire("replaceAfter", o.el.name);
    },
    remove: (e) => {
      retire(e);
      const l = holder.list;
      if (l) seenInRemove.push({ keys: [...l.keys()], size: l.size });
      fire("remove", e.el.name);
    },
    retain: (e, k) => {
      fire("retain", e.el.name);
      return k === "d";
    },
  });
  holder.list = list;
  return {
    list,
    parent,
    all,
    seenInRemove,
    sentinel,
    arm: (phase: Phase, name: string) => {
      armed = { phase, name };
    },
    retires: (e: TEntry | undefined) => retired.get(e as TEntry) ?? 0,
    entry: (name: string): TEntry => {
      const e = all.find((x) => x.el.name === name);
      assert.ok(e, `entry ${name} was created`);
      return e;
    },
  };
}

// propagates checks that fn throws the very error the hook threw.
function propagates(fn: () => void, sentinel: Error): void {
  try {
    fn();
  } catch (e) {
    assert.equal(e, sentinel, "the hook's error propagates unchanged");
    return;
  }
  assert.fail("expected the hook's error");
}

const versioned = (...specs: string[]): TItem[] => specs.map((s) => ({ id: s.slice(0, 1), v: s.slice(1) }));
const S0 = versioned("a1", "b1", "c1", "d1");
// S1 drops b, holds d, creates e, rebuilds c, keeps a and reorders.
const S1 = versioned("e1", "c2", "a1");

const THROW_POINTS: [string, Phase, string][] = [
  ["key of the rebuilt item", "key", "c2"],
  ["stale of the rebuilt entry", "stale", "c1"],
  ["retain of the dropped entry", "retain", "b1"],
  ["nodes of the dropped entry", "nodes", "b1"],
  ["remove of the dropped entry", "remove", "b1"],
  ["nodes of the held entry", "nodes", "d1"],
  ["create of the new entry", "create", "e1"],
  ["update of the new entry", "update", "e1"],
  ["nodes of the entry being rebuilt", "nodes", "c1"],
  ["create of the rebuilt entry", "create", "c2"],
  ["replace before it swaps", "replaceBefore", "c1"],
  ["replace after it swaps", "replaceAfter", "c1"],
  ["update of the rebuilt entry", "update", "c2"],
  ["update of the kept entry", "update", "a1"],
  ["nodes of the kept entry while ordering", "nodes", "a1"],
  ["nodes of the new entry while ordering", "nodes", "e1"],
];

for (const [label, phase, name] of THROW_POINTS) {
  test(`a hook that throws in ${label} leaves the list in step with the DOM`, () => {
    const t = tracker(true);
    t.list.sync(S0);
    t.arm(phase, name);
    propagates(() => t.list.sync(S1), t.sentinel);
    const listed = new Set([...t.list.values()].map((e) => e.el));
    const children = t.parent.kids.filter((k) => k.name !== "placeholder");
    for (const k of children) assert.ok(listed.has(k), `${k.name} is in the parent but not in the list`);
    for (const e of t.list.values()) assert.equal(t.retires(e), 0, `${e.el.name} is listed after its remove ran`);
    assert.ok(t.parent.kids.some((k) => k.name === "placeholder"), "the placeholder stays");
  });

  test(`after a throw in ${label} the next sync converges and creates nothing twice`, () => {
    const t = tracker(true);
    t.list.sync(S0);
    t.arm(phase, name);
    propagates(() => t.list.sync(S1), t.sentinel);
    t.list.sync(S1);
    assert.deepEqual(names(t.parent), ["e1", "c2", "a1", "placeholder"]);
    assert.deepEqual([...t.list.keys()], ["e", "c", "a"]);
    const made = t.all.map((e) => e.el.name).sort();
    assert.deepEqual(made, ["a1", "b1", "c1", "c2", "d1", "e1"], "one entry per id and version");
    t.list.sync([]);
    t.list.clear();
    for (const e of t.all) assert.equal(t.retires(e), 1, `${e.el.name} is retired exactly once`);
    assert.deepEqual(names(t.parent), ["placeholder"]);
  });
}

test("an entry created in a sync that threw is removed once when it is no longer wanted", () => {
  const t = tracker();
  t.list.sync(S0);
  t.arm("update", "e1");
  propagates(() => t.list.sync(S1), t.sentinel);
  t.list.sync(versioned("a1"));
  assert.equal(t.retires(t.entry("e1")), 1);
  assert.ok(!t.parent.kids.some((k) => k.name === "e1"));
});

test("an entry whose remove hook ran is not removed again after a later hook throws", () => {
  const t = tracker();
  t.list.sync(S0);
  t.arm("update", "a1");
  propagates(() => t.list.sync(S1), t.sentinel);
  assert.equal(t.retires(t.entry("b1")), 1);
  t.list.sync(S1);
  assert.equal(t.retires(t.entry("b1")), 1, "the converging sync does not run remove for b again");
});

test("a remove hook that throws still forgets its entry and takes its nodes out", () => {
  const t = tracker();
  t.list.sync(S0);
  t.arm("remove", "b1");
  propagates(() => t.list.sync(S1), t.sentinel);
  assert.ok(!t.parent.kids.some((k) => k.name === "b1"));
  assert.ok(![...t.list.keys()].includes("b"));
  t.list.sync(S1);
  assert.equal(t.retires(t.entry("b1")), 1);
});

test("a rebuilt entry swapped in by replace stays tracked when update throws", () => {
  const t = tracker(true);
  t.list.sync(S0);
  t.arm("update", "c2");
  propagates(() => t.list.sync(S1), t.sentinel);
  assert.ok(t.parent.kids.some((k) => k.name === "c2"), "the swapped-in node is in the parent");
  assert.equal(t.list.get("c")?.el.name, "c2");
  t.list.sync(versioned("a1"));
  assert.deepEqual(names(t.parent), ["a1", "placeholder"], "no untracked node is left behind");
});

test("reads inside hooks of a sync that throws still see the list from before it", () => {
  const t = tracker();
  t.list.sync(S0);
  t.arm("update", "a1");
  propagates(() => t.list.sync(S1), t.sentinel);
  assert.deepEqual(t.seenInRemove, [{ keys: ["a", "b", "c", "d"], size: 4 }]);
  assert.ok(![...t.list.keys()].includes("b"));
});

test("clear that throws in a remove hook forgets what it removed and leaves the rest recorded", () => {
  const t = tracker();
  t.list.sync(S0);
  t.list.sync(versioned("a1", "b1", "c1"));
  t.arm("remove", "b1");
  propagates(() => t.list.clear(), t.sentinel);
  for (const name of ["a1", "b1"]) {
    assert.equal(t.retires(t.entry(name)), 1, `${name} is retired once`);
    assert.ok(!t.parent.kids.some((k) => k.name === name), `${name} is out of the parent`);
  }
  assert.equal(t.retires(t.entry("c1")), 0);
  assert.ok(t.parent.kids.some((k) => k.name === "c1"));
  assert.deepEqual([...t.list.keys()], ["c"]);
  t.list.clear();
  for (const e of t.all) assert.equal(t.retires(e), 1, `${e.el.name} is retired exactly once`);
  assert.deepEqual(names(t.parent), ["placeholder"]);
});
