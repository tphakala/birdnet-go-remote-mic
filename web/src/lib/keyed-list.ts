// KeyedList owns a parent element and one entry per key, in screen order, and
// reconciles them against the next items: it creates what is new, updates
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
  create(item: T): E;
  // nodes are the entry's elements, in order. Read on every sync, since an
  // entry's element may change after create.
  nodes(entry: E): readonly Element[];
  // update runs for every shown item, a created one included.
  update?(entry: E, item: T): void;
  // stale says the entry must be rebuilt for item (default: never).
  stale?(entry: E, item: T): boolean;
  // replace runs after a rebuild created next and before the old nodes go; it
  // may swap them in place. The old nodes are removed afterwards if still
  // attached.
  replace?(old: E, next: E): void;
  // remove runs for an entry that is gone while its nodes are still in the
  // DOM, so the view can read where focus is; before is the keys in screen
  // order as they were before this sync. The nodes are removed afterwards.
  remove?(entry: E, key: K, before: readonly K[]): void;
  // retain keeps a gone entry out of the DOM but held, to be revived without
  // create when its key is wanted again (default: never).
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
  // entry that is going away and its neighbours.
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

    const nextHeld = new Map(this.held);
    for (const k of plan.drop) {
      const e = entryOf(k);
      if (e === undefined) continue;
      h.remove?.(e, k, before);
      this.detach(e);
      nextHeld.delete(k);
    }
    for (const k of plan.hold) {
      const e = this.shown.get(k);
      if (e === undefined) continue;
      this.detach(e);
      nextHeld.set(k, e);
    }

    const created = new Set(plan.create);
    const rebuilt = new Set(plan.rebuild);
    const nextShown = new Map<K, E>();
    for (const k of plan.order) {
      const item = wanted.get(k);
      if (item === undefined) continue;
      let entry: E | undefined;
      if (created.has(k)) {
        entry = h.create(item);
      } else if (rebuilt.has(k)) {
        const old = entryOf(k);
        entry = h.create(item);
        if (old !== undefined) {
          h.replace?.(old, entry);
          this.detach(old);
        }
      } else {
        entry = entryOf(k);
      }
      if (entry === undefined) continue;
      h.update?.(entry, item);
      nextShown.set(k, entry);
      nextHeld.delete(k);
    }

    // Order last, so a node that is already in place is not touched: with the
    // same keys in the same order nothing moves.
    orderChildren(this.parent, this.flatNodes(nextShown.values()));
    this.shown = nextShown;
    this.held = nextHeld;
  }

  // get returns the shown entry for key; a held entry is not returned.
  get(key: K): E | undefined {
    return this.shown.get(key);
  }

  // keys iterates the shown keys in screen order.
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
  // nodes and forgets them.
  clear(): void {
    const before = [...this.shown.keys()];
    for (const m of [this.shown, this.held]) {
      for (const [k, e] of m) {
        this.hooks.remove?.(e, k, before);
        this.detach(e);
      }
    }
    this.shown = new Map();
    this.held = new Map();
  }

  private detach(entry: E): void {
    for (const n of this.hooks.nodes(entry)) n.remove();
  }

  private *flatNodes(entries: Iterable<E>): Generator<Element> {
    for (const e of entries) yield* this.hooks.nodes(e);
  }
}
