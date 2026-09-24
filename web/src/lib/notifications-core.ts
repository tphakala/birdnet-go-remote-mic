// I/O-free reconcile logic for the notification center. No DOM, no fetch, no
// localStorage, no timers: every function is a transform over a CoreState value
// (mutated in place and returned), so the whole thing is unit-tested with
// node:test and no browser. The stateful shell (lib/notifications.ts) owns I/O
// and calls into here.
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
// dismissed. capacity is the server's ring depth, which bounds how much history
// the client keeps between re-syncs.
//
// anchor pins the server's monotonic uptime to the browser clock: at browserMs
// the server's uptime was uptimeMs. Every entry is placed in time from its own
// uptimeMs through this pair (see uptimeToMs) rather than from its wall-clock
// time, so neither browser/server clock skew nor a server clock step (an
// RTC-less Pi syncing NTP after boot) misplaces it. Null until the first
// snapshot or live event arrives.
export interface CoreState {
  bootId: string | null;
  items: Map<number, Notification>;
  readWatermark: number;
  dismissed: Set<number>;
  nextId: number;
  capacity: number;
  anchor: ClockAnchor | null;
}

// ClockAnchor is one simultaneous reading of the browser clock and the server's
// monotonic uptime.
export interface ClockAnchor {
  browserMs: number;
  uptimeMs: number;
}

// DEFAULT_CAPACITY is the server's ring depth (notify.defaultCapacity), used
// until a snapshot reports the real one.
export const DEFAULT_CAPACITY = 500;

// initialState returns the empty state a fresh browser starts from.
export function initialState(): CoreState {
  return {
    bootId: null,
    items: new Map<number, Notification>(),
    readWatermark: 0,
    dismissed: new Set<number>(),
    nextId: 0,
    capacity: DEFAULT_CAPACITY,
    anchor: null,
  };
}

// uptimeToMs maps a server uptime onto the browser clock through the anchor. It
// returns NaN before any anchor exists; callers render that as an unknown time.
export function uptimeToMs(state: CoreState, uptimeMs: number): number {
  if (!state.anchor || !Number.isFinite(uptimeMs)) return NaN;
  return state.anchor.browserMs - (state.anchor.uptimeMs - uptimeMs);
}

// eventTimeMs is when an entry happened, on the browser clock.
export function eventTimeMs(state: CoreState, n: Notification): number {
  return uptimeToMs(state, n.uptimeMs);
}

// applySnapshot folds a fetched snapshot into the state. It adopts the snapshot
// boot identity, and when that identity actually changed (the appliance
// restarted since this browser last synced) it resets the per-browser read
// state, since ids restart from 1 and would otherwise be misread as already
// seen. Items merge by id, nextId, the ring capacity and the clock anchor are
// refreshed, and the dismissed set is pruned to ids still present so it cannot
// grow without bound as old entries age out of the ring. bootChanged is true only on a genuine
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

  const present = new Set<number>();
  for (const n of snap.notifications) {
    state.items.set(n.id, n);
    present.add(n.id);
  }

  // nextId is monotonic within a boot. A snapshot taken before a live event we
  // already folded in reports a lower nextId; rolling state.nextId back to it
  // would make that already-seen id (or the next one) read as a gap. Keep the
  // larger. A boot change legitimately restarts the sequence.
  state.nextId = bootChanged ? snap.nextId : Math.max(state.nextId, snap.nextId);

  // Re-anchor on every snapshot, which also absorbs any browser clock step since
  // the last sync. nowMs is read after the response arrived, so every mapped
  // time runs late by the request's latency: usually well under a second, which
  // minute-resolution labels hide, but a fetch that spans a laptop sleep skews
  // them until the next re-sync.
  state.anchor = { browserMs: nowMs, uptimeMs: snap.uptimeMs };
  if (Number.isSafeInteger(snap.capacity) && snap.capacity >= 1) state.capacity = snap.capacity;

  // The snapshot is authoritative for every id below snap.nextId: any such id it
  // omits has aged out of the ring or been cleared. This must compare against
  // nextId, NOT the oldest id present: a long-lived pinned active condition
  // keeps a low id in the snapshot while the ring floor moves far past it, so an
  // oldest-based floor would never prune the ids in that gap and they would leak
  // forever and inflate the unread count. Ids at or above nextId are live
  // arrivals that landed after the snapshot was taken, so they are kept. Prune
  // items and the dismissed set in lockstep (dismissed to whatever items remain)
  // so neither grows without bound and a dropped entry cannot resurrect.
  for (const [id] of state.items) {
    if (id < snap.nextId && !present.has(id)) state.items.delete(id);
  }
  for (const id of state.dismissed) {
    if (!state.items.has(id)) state.dismissed.delete(id);
  }

  return { state, bootChanged };
}

// applyLive folds one streamed event into the state. resync is true when the
// frame's bootId differs from ours: the appliance restarted, its ids restart
// from 1 and would collide with our current ones (a new id 1 looking like a
// duplicate of an old id 1, with no gap reported), so the state is left
// untouched and the shell must reload a fresh snapshot (applySnapshot then
// resets on the boot change). gap is true when the id jumped past the one
// expected next, meaning an event was dropped and the shell should re-sync.
// isNewError is the toast trigger: a genuinely new error-severity entry that is
// unread and not dismissed. A duplicate (already known, for instance replayed
// after an SSE reconnect) sets neither, and the id-keyed map means it cannot
// double count. nowMs anchors the clock when no anchor exists yet, and the
// history is then pruned to the server's ring depth (see pruneToRing), so a
// long-lived connection stays bounded without a re-sync.
export function applyLive(
  state: CoreState,
  n: Notification,
  nowMs: number,
): { state: CoreState; gap: boolean; isNewError: boolean; resync: boolean } {
  if (state.bootId === null) {
    // First event before any snapshot: adopt its boot identity now, so a later
    // snapshot from a different boot is recognized as a boot change and resets,
    // rather than being merged into this event's history with a stale nextId.
    state.bootId = n.bootId;
  } else if (n.bootId !== state.bootId) {
    return { state, gap: false, isNewError: false, resync: true };
  }

  const known = state.items.has(n.id);
  const gap = n.id > state.nextId;
  const isNewError =
    !known &&
    n.severity === "error" &&
    n.id > state.readWatermark &&
    !state.dismissed.has(n.id);

  state.items.set(n.id, n);
  if (n.id >= state.nextId) state.nextId = n.id + 1;
  // The entry was published moments ago, so its uptime reads as "now" well
  // enough to anchor on until the first snapshot replaces it.
  state.anchor ??= { browserMs: nowMs, uptimeMs: n.uptimeMs };
  pruneToRing(state);

  return { state, gap, isNewError, resync: false };
}

// pruneToRing drops what the server's ring has already trimmed: ids are assigned
// without gaps, so the ring holds exactly the last `capacity` ids below nextId,
// and anything older survives on the server only as the onset of a still-active
// condition. The client mirrors that bound, keeping those onsets, and prunes the
// dismissed set by the same floor so neither grows between re-syncs. The work
// runs only once an item falls below the floor; until then a dismissed id below
// it waits for that moment or for the next snapshot.
export function pruneToRing(state: CoreState): CoreState {
  const floor = state.nextId - state.capacity;
  let stale = false;
  for (const id of state.items.keys()) {
    if (id < floor) {
      stale = true;
      break;
    }
  }
  if (!stale) return state;
  const pinned = new Set(activeConditions(state).map((n) => n.id));
  // Deleting the current key while iterating a Map is safe in JS.
  for (const id of state.items.keys()) {
    if (id < floor && !pinned.has(id)) state.items.delete(id);
  }
  // Prune dismissed ids by the same floor, never by "not held": before the first
  // snapshot the items hold only live frames, while dismissed ids restored from
  // storage may name entries not fetched yet. Dropping those would bring cleared
  // entries back into the bell once the snapshot lands.
  for (const id of state.dismissed) {
    if (id < floor && !pinned.has(id)) state.dismissed.delete(id);
  }
  return state;
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

// clearAll empties the history list and zeroes the badge, but deliberately
// keeps currently-active conditions visible: an operator clicking Clear to tidy
// up must not lose sight of a device that is still failed. It dismisses every
// entry except the active-condition onsets (dismissal, not deletion, so a
// redisplay cannot resurrect a cleared entry) and marks everything read so the
// unread count drops to zero. The active onsets stay undismissed and unread-
// excluded-by-watermark, so the panel shows only what is still ongoing.
export function clearAll(state: CoreState): CoreState {
  const activeIds = new Set(activeConditions(state).map((n) => n.id));
  for (const [id] of state.items) {
    if (!activeIds.has(id)) state.dismissed.add(id);
  }
  markAllRead(state);
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

// isNotification validates an untrusted value against the Notification contract.
// The UI indexes severity (icon and toast), renders title/message/category/
// source, and keys condition logic on kind, so a frame missing a field or
// carrying an off-contract severity would render "undefined" or misbehave. A
// malformed live frame or snapshot entry is rejected rather than folded in.
// severity is checked against the enum; category and kind stay lenient strings
// (an unknown kind is simply not treated as a condition, an unknown category is
// a harmless chip), which tolerates the server adding a value without a client
// release.
export function isNotification(v: unknown): v is Notification {
  if (typeof v !== "object" || v === null) return false;
  const n = v as Record<string, unknown>;
  return (
    // ids are positive integers assigned in sequence; applyLive does nextId and
    // gap arithmetic with this value, so a fractional or unsafe id is rejected.
    typeof n.id === "number" &&
    Number.isSafeInteger(n.id) &&
    n.id >= 1 &&
    typeof n.bootId === "string" &&
    typeof n.time === "string" &&
    // Every placement in time and every duration is computed from uptimeMs.
    typeof n.uptimeMs === "number" &&
    Number.isFinite(n.uptimeMs) &&
    n.uptimeMs >= 0 &&
    (n.severity === "error" || n.severity === "warning" || n.severity === "info") &&
    typeof n.category === "string" &&
    typeof n.kind === "string" &&
    typeof n.title === "string" &&
    typeof n.message === "string" &&
    // key (condition identity, a Map key) and source (rendered) are optional but
    // must be strings when present, or they would break matching or rendering.
    (n.key === undefined || typeof n.key === "string") &&
    (n.source === undefined || typeof n.source === "string")
  );
}
