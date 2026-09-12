// Pure reconcile logic for the notification center. No DOM, no fetch, no
// localStorage, no timers: every function is a transform over plain values, so
// the whole thing is unit-tested with node:test and no browser. The stateful
// shell (lib/notifications.ts) owns I/O and calls into here.
//
// The model: the server's ring and active-condition set are the truth. A client
// bootstraps from a snapshot, applies live events as hints, and re-syncs from a
// fresh snapshot on reconnect or a detected id gap. Read state (the watermark
// and the dismissed set) is per browser and survives reloads, but is reset when
// the appliance restarts (a new bootId), because ids restart from 1 each boot.

import type { Notification, NotificationSnapshot } from "./types.js";

// CoreState is the full client state. items is keyed by id (the server assigns
// monotonic per-boot ids starting at 1). readWatermark is the highest id the
// user has marked read; an entry is unread when its id is above it and it is not
// dismissed. serverOffsetMs corrects event timestamps for clock skew on an
// RTC-less host: it is (browser now - server now) sampled at the last snapshot.
export interface CoreState {
  bootId: string | null;
  items: Map<number, Notification>;
  readWatermark: number;
  dismissed: Set<number>;
  nextId: number;
  serverOffsetMs: number;
}

// initialState returns the empty state a fresh browser starts from.
export function initialState(): CoreState {
  return {
    bootId: null,
    items: new Map<number, Notification>(),
    readWatermark: 0,
    dismissed: new Set<number>(),
    nextId: 0,
    serverOffsetMs: 0,
  };
}

// applySnapshot folds a fetched snapshot into the state. It adopts the snapshot
// boot identity, and when that identity actually changed (the appliance
// restarted since this browser last synced) it resets the per-browser read
// state, since ids restart from 1 and would otherwise be misread as already
// seen. Items merge by id, nextId and the clock offset are refreshed, and the
// dismissed set is pruned to ids still present so it cannot grow without bound
// as old entries age out of the ring. bootChanged is true only on a genuine
// restart, never on the first load.
export function applySnapshot(
  state: CoreState,
  snap: NotificationSnapshot,
  nowMs: number,
): { state: CoreState; bootChanged: boolean } {
  const bootChanged = state.bootId !== null && state.bootId !== snap.bootId;
  if (bootChanged) {
    // A restart wipes the server's history and restarts ids from 1, so the old
    // boot's items and read state are meaningless now; the snapshot reseeds.
    state.items = new Map<number, Notification>();
    state.readWatermark = 0;
    state.dismissed = new Set<number>();
  }
  state.bootId = snap.bootId;

  let oldest = Number.POSITIVE_INFINITY;
  for (const n of snap.notifications) {
    state.items.set(n.id, n);
    if (n.id < oldest) oldest = n.id;
  }
  state.nextId = snap.nextId;

  const serverMs = Date.parse(snap.serverTime);
  state.serverOffsetMs = Number.isNaN(serverMs) ? 0 : nowMs - serverMs;

  // The snapshot is authoritative: entries below the oldest id it reports have
  // aged out of the ring (pinned active conditions are always included, so they
  // keep the floor low enough to survive). Prune items and dismissed ids in
  // lockstep to that floor so neither grows without bound and an aged-out entry
  // cannot resurrect as unread once its dismissed flag is forgotten.
  for (const [id] of state.items) {
    if (id < oldest) state.items.delete(id);
  }
  for (const id of state.dismissed) {
    if (id < oldest) state.dismissed.delete(id);
  }

  return { state, bootChanged };
}

// applyLive folds one streamed event into the state. gap is true when the id
// jumped past the one expected next, meaning an event was dropped and the shell
// should re-sync from a fresh snapshot. isNewError is the toast trigger: a
// genuinely new error-severity entry that is unread and not dismissed. A
// duplicate (already known, for instance replayed after an SSE reconnect) sets
// neither, and the id-keyed map means it cannot double count.
export function applyLive(
  state: CoreState,
  n: Notification,
): { state: CoreState; gap: boolean; isNewError: boolean } {
  const known = state.items.has(n.id);
  const gap = n.id > state.nextId;
  const isNewError =
    !known &&
    n.severity === "error" &&
    n.id > state.readWatermark &&
    !state.dismissed.has(n.id);

  state.items.set(n.id, n);
  if (n.id >= state.nextId) state.nextId = n.id + 1;

  return { state, gap, isNewError };
}

// unreadCount is the badge number: entries above the read watermark that have
// not been dismissed.
export function unreadCount(state: CoreState): number {
  let count = 0;
  for (const [id] of state.items) {
    if (id > state.readWatermark && !state.dismissed.has(id)) count++;
  }
  return count;
}

// activeConditions returns the onsets whose condition is still active: for each
// key, the latest onset/clear entry wins, and the condition is active when that
// latest entry is an onset. Ascending by id so the panel renders them in a
// stable order.
export function activeConditions(state: CoreState): Notification[] {
  const latestByKey = new Map<string, Notification>();
  const sorted = [...state.items.values()].sort((a, b) => a.id - b.id);
  for (const n of sorted) {
    if (!n.key) continue;
    if (n.kind === "onset" || n.kind === "clear") latestByKey.set(n.key, n);
  }
  return [...latestByKey.values()]
    .filter((n) => n.kind === "onset")
    .sort((a, b) => a.id - b.id);
}

// markAllRead advances the watermark past every known entry so the badge goes
// to zero. The max guard keeps it monotonic if called with nothing newer.
export function markAllRead(state: CoreState): CoreState {
  let maxId = state.readWatermark;
  for (const [id] of state.items) {
    if (id > maxId) maxId = id;
  }
  state.readWatermark = maxId;
  return state;
}

// clearAll dismisses every known entry so the panel list empties. Dismissal
// rather than deletion means a redisplay cannot resurrect a cleared entry, and
// the dismissed set is what gets pruned against future snapshots. Unread falls
// to zero because the count excludes dismissed ids.
export function clearAll(state: CoreState): CoreState {
  for (const [id] of state.items) state.dismissed.add(id);
  return state;
}

// serialize returns the persisted subset (the read state keyed to a boot), not
// the items, which are always rebuilt from the server.
export function serialize(state: CoreState): string {
  return JSON.stringify({
    bootId: state.bootId,
    readWatermark: state.readWatermark,
    dismissed: [...state.dismissed],
  });
}

// deserialize rebuilds a seed state from the persisted subset, tolerating any
// malformed or absent input by falling back to the empty state field by field.
export function deserialize(json: string | null): CoreState {
  const state = initialState();
  if (!json) return state;
  try {
    const parsed: unknown = JSON.parse(json);
    if (!parsed || typeof parsed !== "object") return state;
    const obj = parsed as Record<string, unknown>;
    if (typeof obj.bootId === "string") state.bootId = obj.bootId;
    if (typeof obj.readWatermark === "number" && Number.isFinite(obj.readWatermark)) {
      state.readWatermark = obj.readWatermark;
    }
    if (Array.isArray(obj.dismissed)) {
      state.dismissed = new Set(
        obj.dismissed.filter((x: unknown): x is number => typeof x === "number"),
      );
    }
  } catch {
    // Garbage in storage must not break the UI: keep the empty state.
    return initialState();
  }
  return state;
}
