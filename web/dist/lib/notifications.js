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
export class NotificationStore extends EventTarget {
    state;
    gapReloadTimer = null;
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
        let snap;
        try {
            snap = await api.getNotifications();
        }
        catch (err) {
            console.warn("Failed to load notifications:", err);
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
        // a bad frame must not poison the state with a NaN id.
        if (!data || typeof data !== "object" || typeof data.id !== "number") {
            return;
        }
        const n = data;
        const { gap, isNewError } = applyLive(this.state, n);
        if (isNewError)
            showToast(n.message, "error");
        if (gap)
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
