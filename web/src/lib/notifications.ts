// The stateful shell around the pure notification core. It owns all the I/O the
// core deliberately avoids: fetching the snapshot, subscribing to the SSE
// stream and the app store's connection events, the debounced re-sync on a
// detected gap, the error toast, and localStorage persistence. The component
// renders from the "change" event this dispatches.

import { api, ApiError, type ApiClient } from "./api.js";
import { sse, type SSEClient } from "./sse.js";
import { store, type Timers } from "./store.js";
import { showToast } from "../components/toast.js";
import {
  LoadTracker,
  applyLive,
  applySnapshot,
  clearAll as coreClearAll,
  clockStepped,
  deserialize,
  initialState,
  isNotification,
  isSnapshot,
  markAllRead as coreMarkAllRead,
  requestMidpoint,
  resyncDelay,
  serialize,
  type ClockReference,
  type CoreState,
} from "./notifications-core.js";

// Persisted per browser. Only the read state is stored (see serialize); the
// items are always rebuilt from the server, so the key stays small.
const STORAGE_KEY = "remote-mic-notifications";
// Coalesce the burst of refetches a gap can trigger into one snapshot request.
const GAP_RELOAD_DELAY_MS = 400;

// NotificationDeps is what the notification store drives: the snapshot
// endpoint, the event stream, the source of "connection" events (the app
// store), and the timers. The app uses the shared singletons; a test passes
// fakes, so the re-sync wiring runs without a network or a browser.
export interface NotificationDeps {
  api: Pick<ApiClient, "getNotifications">;
  sse: Pick<SSEClient, "subscribe">;
  connection: EventTarget;
  timers: Pick<Timers, "setTimeout" | "clearTimeout">;
}

export class NotificationStore extends EventTarget {
  private readonly api: NotificationDeps["api"];
  private readonly timers: NotificationDeps["timers"];
  private state: CoreState;
  private reloadTimer: ReturnType<typeof setTimeout> | null = null;
  // Whether the pending reloadTimer is only a backoff retry (see scheduleReload).
  private reloadIsBackoff = false;
  // Whether the event stream is up, as the last "connection" event said. While
  // it is down no re-sync timer is armed (see scheduleReload): the next
  // connect re-syncs anyway.
  private connected = false;
  // Load ordering and outcome (see LoadTracker): the Events page reads
  // hasLoaded to tell an empty log from an unfetched one, and hasFailed to
  // offer Retry instead of waiting on "Loading" forever.
  private readonly loads = new LoadTracker();
  // Consecutive failed re-syncs, which set the backoff before the next one.
  private resyncAttempt = 0;
  // Set when the latest failed load was a 401: the login flow reconnects and
  // re-syncs on its own, so a re-sync retry would only be rejected again.
  private lastFailureUnauthorized = false;
  // The wall and monotonic clocks read together when the anchor was last taken,
  // for spotting a browser clock step (see checkClock). Null until an anchor
  // exists, and after a detected step until the re-sync lands.
  private clockRef: ClockReference | null = null;
  // The persisted read state as last written (or read), so a live event that
  // did not change it does not rewrite localStorage.
  private persisted: string;

  constructor(deps: NotificationDeps = { api, sse, connection: store, timers: globalThis }) {
    super();
    this.api = deps.api;
    this.timers = deps.timers;
    this.state = this.readPersisted();
    this.persisted = serialize(this.state);

    deps.sse.subscribe((name: string, data: unknown) => this.onSSE(name, data));

    // Re-sync on every (re)connect: the stream is best effort and may have
    // dropped events while down, so the snapshot is the source of truth. This
    // also performs the first load, since startPolling fires a "connected" event.
    // It goes through resync, so a failed connect-time load retries with the
    // same backoff as any other (a 401 still defers to the login flow). While
    // the stream is down a pending retry is dropped and no new one is armed,
    // not even by a load already in flight when it went down that then fails:
    // the next connect re-syncs anyway, and a stream stopped for a hidden page
    // must not keep loading.
    deps.connection.addEventListener("connection", (e: Event) => {
      this.connected = (e as CustomEvent<boolean>).detail;
      if (this.connected) void this.resync();
      else this.clearReload();
    });
  }

  public getState(): CoreState {
    return this.state;
  }

  // hasLoaded reports whether a snapshot has been applied yet, so a consumer can
  // distinguish a genuinely empty log from one not fetched (or failed to fetch).
  public hasLoaded(): boolean {
    return this.loads.hasLoaded();
  }

  // hasFailed reports whether the most recent load failed with no success since.
  public hasFailed(): boolean {
    return this.loads.hasFailed();
  }

  // load fetches the authoritative snapshot and folds it in. A failure (a 501
  // when no source is mounted, a 401 handled by the shared auth flow, or a
  // transient error) keeps the bell working from its last state; it only sets
  // the failure flag and emits a change so a page can show it. It resolves true
  // when fresh data is in place (this snapshot applied, or a newer one already
  // had), so a caller such as the Events page's Retry can settle from its own
  // outcome rather than from whichever load reports next.
  public async load(): Promise<boolean> {
    const token = this.loads.begin();
    // Both clocks are read on each side of the request: the anchor sits at the
    // midpoint of the wall readings, the step reference at the midpoint of both.
    const beforeWall = Date.now();
    const beforeMono = performance.now();
    let snap: unknown;
    try {
      snap = await this.api.getNotifications();
    } catch (err) {
      console.warn("Failed to load notifications:", err);
      this.lastFailureUnauthorized = err instanceof ApiError && err.status === 401;
      this.markFailed(token);
      return false;
    }
    const wallMs = requestMidpoint(beforeWall, Date.now());
    const monoMs = requestMidpoint(beforeMono, performance.now());
    // Skip only if a strictly newer load has ALREADY applied its snapshot; a
    // newer load that merely started (and may still fail) must not discard this
    // valid result.
    if (!this.loads.canApply(token)) return true;
    if (!isSnapshot(snap)) {
      console.warn("Ignoring malformed notifications snapshot");
      this.lastFailureUnauthorized = false;
      this.markFailed(token);
      return false;
    }
    applySnapshot(this.state, snap, wallMs);
    this.clockRef = { wallMs, monoMs };
    // Recorded only after a successful apply, so a snapshot that throws while
    // folding in never marks this token applied. Nothing awaits between the
    // canApply check above and here, so the gate cannot refuse it.
    this.loads.applied(token);
    // Fresh data is in place, so a backoff retry still pending from an earlier
    // failure is moot. Clear it and the attempt count: left pending, it would
    // absorb (and so delay by up to a minute) the next gap's re-sync. A pending
    // plain re-sync is kept (see scheduleReload).
    if (this.reloadIsBackoff) this.clearReload();
    this.resyncAttempt = 0;
    this.persist();
    this.emitChange();
    return true;
  }

  // markFailed records a failed load and emits a change when it counts (see
  // LoadTracker.fail).
  private markFailed(token: number): void {
    if (this.loads.fail(token)) this.emitChange();
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
    // Every frame (the 15 s heartbeat included) is a cheap moment to check the
    // browser clock, so a step during a long connection is caught promptly.
    this.checkClock();
    if (name !== "notification") return;
    // Reject a malformed frame outright: every field the UI renders or keys on
    // must be present and well-typed, or it would render "undefined" labels or
    // break the condition logic.
    if (!isNotification(data)) return;
    const n = data;
    const hadAnchor = this.state.anchor !== null;
    const nowMs = Date.now();
    const { gap, isNewError, resync } = applyLive(this.state, n, nowMs);
    // applyLive anchors on the first frame when no snapshot has yet; take the
    // step reference at the same moment.
    if (!hadAnchor && this.state.anchor !== null) this.clockRef = { wallMs: nowMs, monoMs: performance.now() };
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

  // checkClock re-syncs when the browser wall clock stepped (a manual change,
  // an NTP step) since the anchor was taken: every mapped time rests on the
  // wall clock at the anchor, so they would all be shifted until the next
  // re-sync. A suspend does not move the anchor (the wall clock and the
  // appliance's uptime both run on through it), but performance.now can stall
  // during sleep, depending on browser and platform, so waking reads as a step
  // and costs one extra, harmless re-sync; so can slow drift between the two clocks on a tab
  // left open for days. The reference is dropped so one step costs one
  // re-sync; the re-sync's snapshot sets a fresh one.
  private checkClock(): void {
    if (this.clockRef === null) return;
    if (!clockStepped(this.clockRef, Date.now(), performance.now())) return;
    this.clockRef = null;
    this.scheduleReload();
  }

  // scheduleReload debounces a re-sync (a detected gap, a boot change, a clock
  // step) so a burst of out-of-order or dropped events costs one snapshot
  // fetch, not one per event. A pending re-sync absorbs any later request.
  // backoff marks resync's retry of a failed load, which an applied load makes
  // moot; a re-sync request absorbed into a pending retry turns it into a
  // plain re-sync, which must still run even if some other load applies first
  // (that load may predate the events the request is about). Nothing is armed
  // while the stream is down: the connect-time re-sync covers what was missed.
  private scheduleReload(delayMs = GAP_RELOAD_DELAY_MS, backoff = false): void {
    if (!this.connected) return;
    if (this.reloadTimer !== null) {
      if (!backoff) this.reloadIsBackoff = false;
      return;
    }
    this.reloadIsBackoff = backoff;
    this.reloadTimer = this.timers.setTimeout(() => {
      this.reloadTimer = null;
      void this.resync();
    }, delayMs);
  }

  // clearReload cancels a pending re-sync, if any.
  private clearReload(): void {
    if (this.reloadTimer === null) return;
    this.timers.clearTimeout(this.reloadTimer);
    this.reloadTimer = null;
  }

  // resync loads the snapshot and, when that fails with no newer success in
  // place, schedules another attempt with a bounded backoff (see resyncDelay),
  // so a failing re-sync is not left waiting for the next gap, reconnect or
  // Retry. A 401 is left to the login flow, which re-syncs on reconnect.
  private async resync(): Promise<void> {
    const ok = await this.load();
    if (ok || !this.loads.hasFailed()) {
      this.resyncAttempt = 0;
      return;
    }
    if (this.lastFailureUnauthorized) return;
    this.resyncAttempt++;
    this.scheduleReload(resyncDelay(this.resyncAttempt), true);
  }

  private readPersisted(): CoreState {
    try {
      return deserialize(localStorage.getItem(STORAGE_KEY));
    } catch {
      // localStorage can throw in a private window or when storage is disabled.
      return initialState();
    }
  }

  // persist writes the read state, but only when it changed since the last
  // write: most live events leave it untouched, and a write per event would
  // cost a synchronous storage write for nothing.
  private persist(): void {
    const next = serialize(this.state);
    if (next === this.persisted) return;
    // Recorded before the write, so a storage that throws is not retried on
    // every event; the next real change tries again.
    this.persisted = next;
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      // A quota or availability error must not break the UI; the read state is
      // a convenience, rebuilt from the server on the next load either way.
    }
  }

  private emitChange(): void {
    this.dispatchEvent(new CustomEvent("change"));
  }
}
