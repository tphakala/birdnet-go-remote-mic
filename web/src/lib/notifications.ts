// The stateful shell around the pure notification core. It owns all the I/O the
// core deliberately avoids: fetching the snapshot, subscribing to the SSE
// stream and the app store's connection events, the debounced re-sync on a
// detected gap, the error toast, and localStorage persistence. The component
// renders from the "change" event this dispatches.

import { api } from "./api.js";
import { sse } from "./sse.js";
import { store } from "./store.js";
import { showToast } from "../components/toast.js";
import { LatestGate } from "./latest-core.js";
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

// isSnapshot rejects a response that is not a notification snapshot (the uptime
// anchor included, since every entry is placed in time through it). A proxy or
// captive portal can answer a 200 with a non-JSON body, which api.request
// surfaces as a string; folding that into applySnapshot would reset the read
// state on a bogus bootId and then throw iterating a missing notifications array.
function isSnapshot(v: unknown): v is NotificationSnapshot {
  if (typeof v !== "object" || v === null) return false;
  const s = v as Partial<NotificationSnapshot>;
  return (
    typeof s.bootId === "string" &&
    typeof s.serverTime === "string" &&
    typeof s.uptimeMs === "number" &&
    Number.isFinite(s.uptimeMs) &&
    Number.isFinite(s.nextId) &&
    Array.isArray(s.notifications) &&
    // Every entry must be well-formed AND belong to the snapshot's own boot, so
    // a snapshot cannot label itself one boot while carrying another boot's
    // entries (which would persist as stale history or active conditions).
    s.notifications.every((n) => isNotification(n) && n.bootId === s.bootId)
  );
}

export class NotificationStore extends EventTarget {
  private state: CoreState;
  private gapReloadTimer: number | null = null;
  // load() ordering (see LatestGate). Each call takes a token; a snapshot
  // applies only when no newer load has applied yet, and the token is recorded
  // as applied only after a successful apply, so a newer load that FAILS cannot
  // discard an older load's valid snapshot (while a newer load that SUCCEEDS
  // still wins over an older, slower one).
  private loadGate = new LatestGate();
  // Latched true once a snapshot has been applied. Until then a consumer cannot
  // tell an empty log from an unfetched one; the Events page uses this to show a
  // loading state rather than asserting "no events".
  private loadedOnce = false;
  // True while the latest load attempt failed and none has succeeded since, so
  // the Events page can offer Retry instead of waiting on "Loading" forever.
  private loadFailed = false;

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

  // hasLoaded reports whether a snapshot has been applied yet, so a consumer can
  // distinguish a genuinely empty log from one not fetched (or failed to fetch).
  public hasLoaded(): boolean {
    return this.loadedOnce;
  }

  // hasFailed reports whether the most recent load failed with no success since.
  public hasFailed(): boolean {
    return this.loadFailed;
  }

  // load fetches the authoritative snapshot and folds it in. A failure (a 501
  // when no source is mounted, a 401 handled by the shared auth flow, or a
  // transient error) keeps the bell working from its last state; it only sets
  // the failure flag and emits a change so a page can show it.
  public async load(): Promise<void> {
    const token = this.loadGate.begin();
    let snap: unknown;
    try {
      snap = await api.getNotifications();
    } catch (err) {
      console.warn("Failed to load notifications:", err);
      this.markFailed(token);
      return;
    }
    // Skip only if a strictly newer load has ALREADY applied its snapshot; a
    // newer load that merely started (and may still fail) must not discard this
    // valid result.
    if (this.loadGate.superseded(token)) return;
    if (!isSnapshot(snap)) {
      console.warn("Ignoring malformed notifications snapshot");
      this.markFailed(token);
      return;
    }
    applySnapshot(this.state, snap, Date.now());
    this.loadedOnce = true;
    this.loadFailed = false;
    // Recorded only after a successful apply (as before), so a snapshot that
    // throws while folding in never marks this token applied. Nothing awaits
    // between the superseded check above and here, so accept cannot refuse.
    this.loadGate.accept(token);
    this.persist();
    this.emitChange();
  }

  // markFailed records a failed load, unless a newer load has already applied a
  // snapshot (that success is the fresher truth). The change event fires on every
  // failure, so a page showing a retry in progress learns that it failed again.
  private markFailed(token: number): void {
    if (this.loadGate.superseded(token)) return;
    this.loadFailed = true;
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
    const { gap, isNewError, resync } = applyLive(this.state, n, Date.now());
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
    // Reload on a detected gap to re-sync the dropped events. applyLive already
    // pruned to the server's ring depth, so a long-lived connection stays bounded.
    if (gap) this.scheduleReload();
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
