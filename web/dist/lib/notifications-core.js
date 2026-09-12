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
// initialState returns the empty state a fresh browser starts from.
export function initialState() {
    return {
        bootId: null,
        items: new Map(),
        readWatermark: 0,
        dismissed: new Set(),
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
export function applySnapshot(state, snap, nowMs) {
    const bootChanged = state.bootId !== null && state.bootId !== snap.bootId;
    if (bootChanged) {
        // A restart wipes the server's history and restarts ids from 1, so the old
        // boot's items and read state are meaningless now; the snapshot reseeds.
        state.items = new Map();
        state.readWatermark = 0;
        state.dismissed = new Set();
    }
    state.bootId = snap.bootId;
    const present = new Set();
    for (const n of snap.notifications) {
        state.items.set(n.id, n);
        present.add(n.id);
    }
    // nextId is monotonic within a boot. A snapshot taken before a live event we
    // already folded in reports a lower nextId; rolling state.nextId back to it
    // would make that already-seen id (or the next one) read as a gap. Keep the
    // larger. A boot change legitimately restarts the sequence.
    state.nextId = bootChanged ? snap.nextId : Math.max(state.nextId, snap.nextId);
    const serverMs = Date.parse(snap.serverTime);
    state.serverOffsetMs = Number.isNaN(serverMs) ? 0 : nowMs - serverMs;
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
        if (id < snap.nextId && !present.has(id))
            state.items.delete(id);
    }
    for (const id of state.dismissed) {
        if (!state.items.has(id))
            state.dismissed.delete(id);
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
// double count.
export function applyLive(state, n) {
    if (state.bootId !== null && n.bootId !== state.bootId) {
        return { state, gap: false, isNewError: false, resync: true };
    }
    const known = state.items.has(n.id);
    const gap = n.id > state.nextId;
    const isNewError = !known &&
        n.severity === "error" &&
        n.id > state.readWatermark &&
        !state.dismissed.has(n.id);
    state.items.set(n.id, n);
    if (n.id >= state.nextId)
        state.nextId = n.id + 1;
    return { state, gap, isNewError, resync: false };
}
// unreadCount is the badge number: entries above the read watermark that have
// not been dismissed.
export function unreadCount(state) {
    let count = 0;
    for (const [id] of state.items) {
        if (id > state.readWatermark && !state.dismissed.has(id))
            count++;
    }
    return count;
}
// activeConditions returns the onsets whose condition is still active: for each
// key, the latest onset/clear entry wins, and the condition is active when that
// latest entry is an onset. Ascending by id so the panel renders them in a
// stable order.
export function activeConditions(state) {
    const latestByKey = new Map();
    const sorted = [...state.items.values()].sort((a, b) => a.id - b.id);
    for (const n of sorted) {
        if (!n.key)
            continue;
        if (n.kind === "onset" || n.kind === "clear")
            latestByKey.set(n.key, n);
    }
    return [...latestByKey.values()]
        .filter((n) => n.kind === "onset")
        .sort((a, b) => a.id - b.id);
}
// markAllRead advances the watermark past every known entry so the badge goes
// to zero. The max guard keeps it monotonic if called with nothing newer.
export function markAllRead(state) {
    let maxId = state.readWatermark;
    for (const [id] of state.items) {
        if (id > maxId)
            maxId = id;
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
export function clearAll(state) {
    const activeIds = new Set(activeConditions(state).map((n) => n.id));
    for (const [id] of state.items) {
        if (!activeIds.has(id))
            state.dismissed.add(id);
    }
    markAllRead(state);
    return state;
}
// serialize returns the persisted subset (the read state keyed to a boot), not
// the items, which are always rebuilt from the server.
export function serialize(state) {
    return JSON.stringify({
        bootId: state.bootId,
        readWatermark: state.readWatermark,
        dismissed: [...state.dismissed],
    });
}
// deserialize rebuilds a seed state from the persisted subset, tolerating any
// malformed or absent input by falling back to the empty state field by field.
export function deserialize(json) {
    const state = initialState();
    if (!json)
        return state;
    try {
        const parsed = JSON.parse(json);
        if (!parsed || typeof parsed !== "object")
            return state;
        const obj = parsed;
        if (typeof obj.bootId === "string")
            state.bootId = obj.bootId;
        if (typeof obj.readWatermark === "number" && Number.isFinite(obj.readWatermark)) {
            state.readWatermark = obj.readWatermark;
        }
        if (Array.isArray(obj.dismissed)) {
            state.dismissed = new Set(obj.dismissed.filter((x) => typeof x === "number"));
        }
    }
    catch {
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
export function isNotification(v) {
    if (typeof v !== "object" || v === null)
        return false;
    const n = v;
    return (Number.isFinite(n.id) &&
        typeof n.bootId === "string" &&
        typeof n.time === "string" &&
        (n.severity === "error" || n.severity === "warning" || n.severity === "info") &&
        typeof n.category === "string" &&
        typeof n.kind === "string" &&
        typeof n.title === "string" &&
        typeof n.message === "string");
}
