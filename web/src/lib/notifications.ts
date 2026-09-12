// The stateful shell around the pure notification core. It owns all the I/O the
// core deliberately avoids: fetching the snapshot, subscribing to the SSE
// stream and the app store's connection events, the debounced re-sync on a
// detected gap, the error toast, and localStorage persistence. The component
// renders from the "change" event this dispatches.

import { api } from "./api.js";
import { sse } from "./sse.js";
import { store } from "./store.js";
import { showToast } from "../components/toast.js";
import {
  applyLive,
  applySnapshot,
  clearAll as coreClearAll,
  deserialize,
  initialState,
  isNotification,
  markAllRead as coreMarkAllRead,
  serialize,
  type CoreState,
} from "./notifications-core.js";
import type { NotificationSnapshot } from "./types.js";

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
function isSnapshot(v: unknown): v is NotificationSnapshot {
  if (typeof v !== "object" || v === null) return false;
  const s = v as Partial<NotificationSnapshot>;
  return (
    typeof s.bootId === "string" &&
    typeof s.serverTime === "string" &&
    Number.isFinite(s.nextId) &&
    Array.isArray(s.notifications) &&
    s.notifications.every(isNotification)
  );
}

export class NotificationStore extends EventTarget {
  private state: CoreState;
  private gapReloadTimer: number | null = null;
  // load() sequencing. loadSeq tickets each call; appliedSeq records the highest
  // ticket whose snapshot was actually applied. A load applies only when no newer
  // load has applied yet, and appliedSeq advances only after a successful apply,
  // so a newer load that FAILS cannot discard an older load's valid snapshot
  // (while a newer load that SUCCEEDS still wins over an older, slower one).
  private loadSeq = 0;
  private appliedSeq = 0;

  constructor() {
    super();
    this.state = this.readPersisted();

    sse.subscribe((name: string, data: unknown) => this.onSSE(name, data));

    // Re-sync on every (re)connect: the stream is best effort and may have
    // dropped events while down, so the snapshot is the source of truth. This
    // also performs the first load, since startPolling fires a "connected" event.
    store.addEventListener("connection", (e: Event) => {
      if ((e as CustomEvent<boolean>).detail) void this.load();
    });
  }

  public getState(): CoreState {
    return this.state;
  }

  // load fetches the authoritative snapshot and folds it in. A failure (a 501
  // when no source is mounted, a 401 handled by the shared auth flow, or a
  // transient error) is swallowed so the bell keeps working from its last state.
  public async load(): Promise<void> {
    const seq = ++this.loadSeq;
    let snap: unknown;
    try {
      snap = await api.getNotifications();
    } catch (err) {
      console.warn("Failed to load notifications:", err);
      return;
    }
    // Skip only if a strictly newer load has ALREADY applied its snapshot; a
    // newer load that merely started (and may still fail) must not discard this
    // valid result.
    if (seq <= this.appliedSeq) return;
    if (!isSnapshot(snap)) {
      console.warn("Ignoring malformed notifications snapshot");
      return;
    }
    applySnapshot(this.state, snap, Date.now());
    this.appliedSeq = seq;
    this.persist();
    this.emitChange();
  }

  public markAllRead(): void {
    coreMarkAllRead(this.state);
    this.persist();
    this.emitChange();
  }

  public clearAll(): void {
    coreClearAll(this.state);
    this.persist();
    this.emitChange();
  }

  private onSSE(name: string, data: unknown): void {
    if (name !== "notification") return;
    // Reject a malformed frame outright: every field the UI renders or keys on
    // must be present and well-typed, or it would render "undefined" labels or
    // break the condition logic.
    if (!isNotification(data)) return;
    const n = data;
    const { gap, isNewError, resync } = applyLive(this.state, n);
    // A frame from a different boot means the appliance restarted; do not fold it
    // into the old boot's state, just reload a fresh snapshot (which resets on
    // the boot change) without persisting or emitting the mismatched frame.
    if (resync) {
      this.scheduleReload();
      return;
    }
    // Fall back to the title so an error that carries no message still toasts
    // readable text rather than a bare icon.
    if (isNewError) showToast(n.message || n.title, "error");
    // Reload on a detected gap (re-sync dropped events) or once the in-memory set
    // has grown past its bound on a long-lived connection (re-sync prunes it).
    if (gap || this.state.items.size > MAX_LIVE_ITEMS) this.scheduleReload();
    this.persist();
    this.emitChange();
  }

  // scheduleReload debounces the gap-driven re-sync so a burst of out-of-order
  // or dropped events costs one snapshot fetch, not one per event.
  private scheduleReload(): void {
    if (this.gapReloadTimer !== null) return;
    this.gapReloadTimer = window.setTimeout(() => {
      this.gapReloadTimer = null;
      void this.load();
    }, GAP_RELOAD_DELAY_MS);
  }

  private readPersisted(): CoreState {
    try {
      return deserialize(localStorage.getItem(STORAGE_KEY));
    } catch {
      // localStorage can throw in a private window or when storage is disabled.
      return initialState();
    }
  }

  private persist(): void {
    try {
      localStorage.setItem(STORAGE_KEY, serialize(this.state));
    } catch {
      // A quota or availability error must not break the UI; the read state is
      // a convenience, rebuilt from the server on the next load either way.
    }
  }

  private emitChange(): void {
    this.dispatchEvent(new CustomEvent("change"));
  }
}
