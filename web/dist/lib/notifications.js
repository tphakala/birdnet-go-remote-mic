// The stateful shell around the pure notification core. It owns all the I/O the
// core deliberately avoids: fetching the snapshot, subscribing to the SSE
// stream and the app store's connection events, the debounced re-sync on a
// detected gap, the error toast, and localStorage persistence. The component
// renders from the "change" event this dispatches.
import { api } from "./api.js";
import { sse } from "./sse.js";
import { store } from "./store.js";
import { showToast } from "../components/toast.js";
import { applyLive, applySnapshot, clearAll as coreClearAll, deserialize, initialState, markAllRead as coreMarkAllRead, serialize, } from "./notifications-core.js";
// Persisted per browser. Only the read state is stored (see serialize); the
// items are always rebuilt from the server, so the key stays small.
const STORAGE_KEY = "remote-mic-notifications";
// Coalesce the burst of refetches a gap can trigger into one snapshot request.
const GAP_RELOAD_DELAY_MS = 400;
// On a stable connection with no reconnect or gap, applySnapshot never runs to
// prune, so live events accumulate unbounded. Once the client holds more than
// this (comfortably above the server's ~100-entry ring) a reload re-syncs and
// prunes back down. The server ring is the real bound; this just triggers it.
const MAX_LIVE_ITEMS = 200;
// isSnapshot rejects a response that is not a notification snapshot. A proxy or
// captive portal can answer a 200 with a non-JSON body, which api.request
// surfaces as a string; folding that into applySnapshot would reset the read
// state on a bogus bootId and then throw iterating a missing notifications array.
function isSnapshot(v) {
    if (typeof v !== "object" || v === null)
        return false;
    const s = v;
    return typeof s.bootId === "string" && Number.isFinite(s.nextId) && Array.isArray(s.notifications);
}
export class NotificationStore extends EventTarget {
    state;
    gapReloadTimer = null;
    // Monotonic generation for snapshot loads. The explicit startup load and the
    // connection-event load race; a GET that resolves after a newer load started
    // must not fold its now-stale snapshot over the fresher state.
    loadEpoch = 0;
    constructor() {
        super();
        this.state = this.readPersisted();
        sse.subscribe((name, data) => this.onSSE(name, data));
        // Re-sync on every (re)connect: the stream is best effort and may have
        // dropped events while down, so the snapshot is the source of truth. This
        // also performs the first load, since startPolling fires a "connected" event.
        store.addEventListener("connection", (e) => {
            if (e.detail)
                void this.load();
        });
    }
    getState() {
        return this.state;
    }
    // load fetches the authoritative snapshot and folds it in. A failure (a 501
    // when no source is mounted, a 401 handled by the shared auth flow, or a
    // transient error) is swallowed so the bell keeps working from its last state.
    async load() {
        const epoch = ++this.loadEpoch;
        let snap;
        try {
            snap = await api.getNotifications();
        }
        catch (err) {
            console.warn("Failed to load notifications:", err);
            return;
        }
        // A newer load() started while this GET was in flight; its result is fresher,
        // so drop this one rather than roll the state back to an older snapshot.
        if (epoch !== this.loadEpoch)
            return;
        if (!isSnapshot(snap)) {
            console.warn("Ignoring malformed notifications snapshot");
            return;
        }
        applySnapshot(this.state, snap, Date.now());
        this.persist();
        this.emitChange();
    }
    markAllRead() {
        coreMarkAllRead(this.state);
        this.persist();
        this.emitChange();
    }
    clearAll() {
        coreClearAll(this.state);
        this.persist();
        this.emitChange();
    }
    onSSE(name, data) {
        if (name !== "notification")
            return;
        // Defend against a malformed payload: the contract guarantees the shape, but
        // a bad frame must not poison the state with a missing or non-numeric id.
        if (!data || typeof data !== "object" || !Number.isFinite(data.id)) {
            return;
        }
        const n = data;
        const { gap, isNewError } = applyLive(this.state, n);
        // Fall back to the title so an error that carries no message still toasts
        // readable text rather than a bare icon.
        if (isNewError)
            showToast(n.message || n.title, "error");
        // Reload on a detected gap (re-sync dropped events) or once the in-memory set
        // has grown past its bound on a long-lived connection (re-sync prunes it).
        if (gap || this.state.items.size > MAX_LIVE_ITEMS)
            this.scheduleReload();
        this.persist();
        this.emitChange();
    }
    // scheduleReload debounces the gap-driven re-sync so a burst of out-of-order
    // or dropped events costs one snapshot fetch, not one per event.
    scheduleReload() {
        if (this.gapReloadTimer !== null)
            return;
        this.gapReloadTimer = window.setTimeout(() => {
            this.gapReloadTimer = null;
            void this.load();
        }, GAP_RELOAD_DELAY_MS);
    }
    readPersisted() {
        try {
            return deserialize(localStorage.getItem(STORAGE_KEY));
        }
        catch {
            // localStorage can throw in a private window or when storage is disabled.
            return initialState();
        }
    }
    persist() {
        try {
            localStorage.setItem(STORAGE_KEY, serialize(this.state));
        }
        catch {
            // A quota or availability error must not break the UI; the read state is
            // a convenience, rebuilt from the server on the next load either way.
        }
    }
    emitChange() {
        this.dispatchEvent(new CustomEvent("change"));
    }
}
