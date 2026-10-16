// KeyedList owns a parent element and one entry per key, in screen order
// (unless the last sync threw), and reconciles them against the next items:
// it creates what is new, updates
// what stays, rebuilds what went stale, removes what is gone and puts the
// nodes in order without moving any that already are (see "Reconcile, never
// rebuild" in web/src/AGENTS.md). The keep/create/rebuild/drop decision is
// keyedPlan (lib/keyed-core.ts); what an entry is, how it changes and what
// happens to focus when it goes stay with the view, as hooks. No document
// access here, so node:test drives it with a fake DOM.

import { keyedPlan } from "./keyed-core.ts";
import { orderChildren } from "./ui.ts";

export interface KeyedListHooks<T, E, K> {
  // key identifies an item; it must depend on the item alone.
  key(item: T): K;
  // create builds an entry with detached nodes (the list inserts them). If it
  // throws, it must undo its own side effects: there is no entry to remove.
  create(item: T): E;
  // nodes are the entry's elements, in order. Read on every sync, since an
  // entry's element may change after create. For an entry that is going away
  // it is read before the remove or replace hook runs, and those are the nodes
  // removed afterwards: a node a hook adds is the hook's to remove.
  nodes(entry: E): readonly Element[];
  // update runs for every shown item, a created one included.
  update?(entry: E, item: T): void;
  // stale says the entry must be rebuilt for item (default: never).
  stale?(entry: E, item: T): boolean;
  // replace runs after a rebuild created next and before the old nodes go;
  // it may swap them in place (not for a held entry, whose old nodes are
  // already out of the DOM). The old nodes are removed afterwards if still
  // attached. If it throws, next is kept for the key and the old nodes are
  // still removed.
  replace?(old: E, next: E): void;
  // remove runs for an entry that is gone while a shown entry's nodes are
  // still in the DOM, so the view can read where focus is; before is the keys
  // as they were before this sync, in screen order unless the last sync threw.
  // The nodes are removed afterwards. The nodes of a held entry, or of an
  // entry a sync that threw never attached, are already out of the DOM. It
  // runs at most once per entry, even when it throws: the entry is then
  // forgotten and its nodes removed.
  remove?(entry: E, key: K, before: readonly K[]): void;
  // retain keeps a gone entry out of the DOM but held, to be revived without
  // create when its key is wanted again and it is not stale (default: never).
  retain?(entry: E, key: K): boolean;
}

export class KeyedList<T, E, K = string> {
  private readonly parent: Element;
  private readonly hooks: KeyedListHooks<T, E, K>;
  private shown = new Map<K, E>();
  private held = new Map<K, E>();

  constructor(parent: Element, hooks: KeyedListHooks<T, E, K>) {
    this.parent = parent;
    this.hooks = hooks;
  }

  // sync reconciles the list with items, in the order given. A repeated key
  // keeps its first item. Reads of the list (get, keys, size, ...) see the
  // state before the sync until it finishes, so a hook can still look up the
  // entry that is going away and its neighbours. A hook that throws aborts
  // the sync and the error propagates, but what the sync already did stays
  // recorded (a created entry is kept, though a later sync may have to attach
  // it; a removed one is forgotten), so the next sync converges; the reads above hold on that path too. A hook must
  // not call sync or clear on its own list.
  sync(items: Iterable<T>): void {
    const h = this.hooks;
    const wanted = new Map<K, T>();
    for (const item of items) {
      const k = h.key(item);
      if (!wanted.has(k)) wanted.set(k, item);
    }
    const before = [...this.shown.keys()];
    const entryOf = (k: K): E | undefined => this.shown.get(k) ?? this.held.get(k);
    const plan = keyedPlan(this.shown.keys(), this.held.keys(), wanted.keys(), {
      stale: (k) => {
        const e = entryOf(k);
        const item = wanted.get(k);
        return e !== undefined && item !== undefined && (h.stale?.(e, item) ?? false);
      },
      retain: (k) => {
        const e = entryOf(k);
        return e !== undefined && (h.retain?.(e, k) ?? false);
      },
    });

    // Work on copies and publish them in the finally, so that a hook that
    // throws leaves the list as the sync left it, while reads inside the
    // hooks still see the list as it was before the sync.
    // Each step records its change as soon as it has made it.
    let shown = new Map(this.shown);
    const held = new Map(this.held);
    try {
      for (const k of plan.drop) {
        const e = shown.get(k) ?? held.get(k);
        if (e === undefined) continue;
        const nodes = h.nodes(e);
        try {
          h.remove?.(e, k, before);
        } finally {
          for (const n of nodes) n.remove();
          shown.delete(k);
          held.delete(k);
        }
      }
      for (const k of plan.hold) {
        const e = shown.get(k);
        if (e === undefined) continue;
        for (const n of h.nodes(e)) n.remove();
        shown.delete(k);
        held.set(k, e);
      }

      const created = new Set(plan.create);
      const rebuilt = new Set(plan.rebuild);
      for (const k of plan.order) {
        const item = wanted.get(k);
        if (item === undefined) continue;
        let entry: E | undefined;
        if (created.has(k)) {
          entry = h.create(item);
          shown.set(k, entry);
        } else if (rebuilt.has(k)) {
          const old = shown.get(k) ?? held.get(k);
          const oldNodes = old === undefined ? [] : h.nodes(old);
          entry = h.create(item);
          shown.set(k, entry);
          held.delete(k);
          if (old !== undefined) {
            try {
              h.replace?.(old, entry);
            } finally {
              for (const n of oldNodes) n.remove();
            }
          }
        } else {
          entry = shown.get(k) ?? held.get(k);
          if (entry === undefined) continue;
          if (held.delete(k)) shown.set(k, entry);
        }
        h.update?.(entry, item);
      }

      // Order last, so a node that is already in place is not touched: with
      // the same keys in the same order nothing moves.
      const ordered = new Map<K, E>();
      for (const k of plan.order) {
        const e = shown.get(k);
        if (e !== undefined) ordered.set(k, e);
      }
      shown = ordered;
      orderChildren(this.parent, this.flatNodes(shown.values()));
    } finally {
      this.shown = shown;
      this.held = held;
    }
  }

  // get returns the shown entry for key; a held entry is not returned.
  get(key: K): E | undefined {
    return this.shown.get(key);
  }

  // keys iterates the shown keys in screen order (after a sync that threw,
  // until the next sync, the order may differ from the screen).
  keys(): IterableIterator<K> {
    return this.shown.keys();
  }

  values(): IterableIterator<E> {
    return this.shown.values();
  }

  entries(): IterableIterator<[K, E]> {
    return this.shown.entries();
  }

  get size(): number {
    return this.shown.size;
  }

  // clear runs the remove hook for every shown and held entry, removes their
  // nodes and forgets them. It stops at the first hook that throws; the
  // entries it has not reached stay in the list and a later clear or sync
  // finishes them.
  clear(): void {
    const before = [...this.shown.keys()];
    const shown = new Map(this.shown);
    const held = new Map(this.held);
    try {
      for (const m of [this.shown, this.held]) {
        for (const [k, e] of m) {
          const nodes = this.hooks.nodes(e);
          try {
            this.hooks.remove?.(e, k, before);
          } finally {
            for (const n of nodes) n.remove();
            shown.delete(k);
            held.delete(k);
          }
        }
      }
    } finally {
      this.shown = shown;
      this.held = held;
    }
  }

  private *flatNodes(entries: Iterable<E>): Generator<Element> {
    for (const e of entries) yield* this.hooks.nodes(e);
  }
}
