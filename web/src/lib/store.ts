import { api, ApiError, apiErrorMessage, isRefusal, TOKEN_NOT_ACCEPTED, UnreadableResponseError, type ApiClient } from "./api.ts";
import { sentence } from "./text.ts";
import { Emitter } from "./emitter.ts";
import { sse, type SSEClient } from "./sse.ts";
import { getToken, setToken } from "./auth.ts";
import { LatestGate } from "./latest-core.ts";
import { ChangeTracker, gatedRefresh } from "./store-core.ts";
import type {
  ApplianceStatus,
  AvailableDevice,
  Config,
  Device,
  DeviceLevels,
  LevelsEvent,
  LoadError,
  SystemInfo,
  UpdateStatus,
} from "./types.ts";

// How often the REST resources are polled while the page is visible.
const POLL_INTERVAL_MS = 3000;
// How long the page may stay hidden before the event stream is stopped. A
// quick tab switch keeps the stream (and its toasts); a tab left in the
// background stops holding the appliance's levels feed.
export const HIDDEN_STREAM_GRACE_MS = 60_000;
// How long levels keep streaming after the dashboard stops showing. A quick
// look at another view keeps the stream as it is; a longer stay drops levels
// from it, so the appliance marshals and sends none for this page (the SSE
// handler skips a source the filter excludes, internal/sse/sse.go:130).
export const LEVELS_GRACE_MS = 30_000;
// NON_LEVEL_EVENTS is every event type the UI consumes except levels, the
// filter the stream uses while levels are dropped. An event type the UI
// starts consuming must be added here too, or it stops arriving while no
// dashboard shows.
export const NON_LEVEL_EVENTS: readonly string[] = ["notification"];

// Timers is the timer API the stores schedule with: the globals in the app, a
// fake a test fires by hand.
export interface Timers {
  setTimeout(fn: () => void, ms: number): ReturnType<typeof setTimeout>;
  clearTimeout(handle: ReturnType<typeof setTimeout>): void;
  setInterval(fn: () => void, ms: number): ReturnType<typeof setInterval>;
  clearInterval(handle: ReturnType<typeof setInterval>): void;
}

export interface AppState {
  status: ApplianceStatus | null;
  devices: Device[];
  available: AvailableDevice[];
  levels: Map<string, DeviceLevels>;
  system: SystemInfo | null;
  config: Config | null;
  connected: boolean;
}

// StoreDeps is the slice of the API and SSE clients the store drives. The app
// uses the shared singletons; a test passes fakes, so the store's wiring (what
// it announces, and when) is exercised without a network or a browser.
export interface StoreDeps {
  api: Pick<
    ApiClient,
    "onUnauthorized" | "getHealth" | "getStatus" | "getDevices" | "getSystem" | "getConfig" | "getAvailableDevices"
  >;
  sse: Pick<SSEClient, "subscribe" | "start" | "stop" | "setEvents">;
  timers?: Timers;
}

// StoreEvents is what AppStore announces: each event's name and payload.
// devices, status, system and available fire only when their data changed;
// devices, status and system also fire again on the first read after a failed
// one (available does not: no view shows a load error for it). config fires
// every poll.
export interface StoreEvents {
  status: ApplianceStatus;
  devices: Device[];
  available: AvailableDevice[];
  levels: Map<string, DeviceLevels>;
  system: SystemInfo;
  config: Config;
  connection: boolean;
  loaderror: LoadError;
  authrequired: undefined;
  authok: undefined;
  // levelsdropped: the stream stopped carrying levels (see setLevelsWanted),
  // so the levels the meters show are no longer current.
  levelsdropped: undefined;
}

export class AppStore extends Emitter<StoreEvents> {
  private readonly api: StoreDeps["api"];
  private readonly sse: StoreDeps["sse"];
  private readonly timers: Timers;
  private state: AppState = {
    status: null,
    devices: [],
    available: [],
    levels: new Map(),
    system: null,
    config: null,
    connected: false,
  };

  private pollIntervalTimer: ReturnType<typeof setInterval> | null = null;
  // polling is true from startPolling until stopPolling. The timer runs only
  // while polling and the page is visible (see setPageHidden).
  private polling = false;
  private pollIntervalMs = POLL_INTERVAL_MS;
  private pageHidden = false;
  // streamStopTimer is armed while the page is hidden and polling; when it
  // fires the event stream is stopped and streamPaused set, until the page
  // shows again (see setPageHidden).
  private streamStopTimer: ReturnType<typeof setTimeout> | null = null;
  private streamPaused = false;
  // Levels: whether a view wants them, whether the stream dropped them, and
  // the grace timer between the two (see setLevelsWanted).
  private levelsWanted = true;
  private levelsDropped = false;
  // Whether a view has said yet whether it wants levels (the first route).
  private levelsDecided = false;
  private levelsTimer: ReturnType<typeof setTimeout> | null = null;
  // One ordering gate per polled resource (see LatestGate). The poll timer does
  // not wait for a tick to finish, and a provision, removal, or save triggers an
  // extra refresh, so reads of one resource overlap and can resolve out of
  // order. The gate keeps an older body from overwriting a newer one (a stale
  // device state, a stale Enable card, a stale config base for the next queued
  // mutation) without dropping a response just because a newer read started,
  // which would starve every view on a link slower than the poll interval. The
  // poll deliberately does not skip ticks instead: the API client sets no fetch
  // timeout, so a hung request would stop polling for good.
  private statusGate = new LatestGate();
  private devicesGate = new LatestGate();
  private systemGate = new LatestGate();
  private availableGate = new LatestGate();
  // applyConfig invalidates this one, so a GET /config already in flight cannot
  // overwrite the authoritative PATCH result when it resolves later.
  private configGate = new LatestGate();
  // Change detection for the resources announced only on change (see the
  // refresh methods below).
  private statusChange = new ChangeTracker();
  private devicesChange = new ChangeTracker();
  private systemChange = new ChangeTracker();
  private availableChange = new ChangeTracker();
  // loginPending is set from the first 401 until a token is accepted, so a
  // burst of rejected requests (the initial load fires five) opens one prompt
  // and the generic load-error state is suppressed in favor of it.
  private loginPending = false;
  // swapDepth is non-zero while a deliberate token rotation is in progress. The
  // appliance enforces the new token before it finishes writing the PATCH
  // response, so an in-flight poll can be rejected with a 401 that carries
  // EITHER the old or the new token while the swap is landing. During that
  // window such a 401 is expected and must not stop polling or pop the login
  // prompt; the rotation caller reloads once setToken has run.
  private swapDepth = 0;

  constructor(deps: StoreDeps = { api, sse }) {
    super();
    this.api = deps.api;
    this.sse = deps.sse;
    this.timers = deps.timers ?? globalThis;
    this.api.onUnauthorized = () => this.onUnauthorized();
    this.initSSE();
  }

  // beginTokenSwap opens the rotation window; endTokenSwap closes it. The pair
  // is depth-counted (floored at 0) so overlapping rotations do not close the
  // window early.
  public beginTokenSwap(): void {
    this.swapDepth++;
  }

  public endTokenSwap(): void {
    this.swapDepth = Math.max(0, this.swapDepth - 1);
    // The event stream stops itself on a 401, and inside the swap window that
    // 401 is expected rather than a credential failure, so nothing else would
    // bring it back until the next startPolling. Restart it under the token now
    // in force; start() is a no-op while the stream is already running.
    // polling is the invariant for "polling is active" (the timer itself is
    // paused while the page is hidden), and the SSE stream runs while polling
    // does (startPolling starts both, stopPolling stops both), except while
    // streamPaused, when a page hidden past the grace stopped it and showing
    // the page restarts it under the token then in force. So polling and not
    // paused means the stream is meant to be up and safe to (re)start here.
    if (this.swapDepth === 0 && this.polling && !this.streamPaused) this.sse.start();
  }

  // onUnauthorized reacts to the appliance rejecting the UI's credentials:
  // polling and the SSE stream stop (they would only be rejected again) and the
  // login prompt is asked for, once per outage.
  private onUnauthorized(): void {
    // A 401 during a deliberate rotation is expected under either token and must
    // not interrupt polling; the rotation caller resumes once setToken has run.
    if (this.swapDepth > 0) return;
    if (this.loginPending) return;
    this.loginPending = true;
    this.stopPolling();
    this.emit("authrequired");
  }

  // login stores token, verifies it against /status, and on success reloads
  // everything and resumes polling. A rejected token is not kept.
  public async login(token: string): Promise<{ ok: boolean; message: string }> {
    setToken(token);
    let status: ApplianceStatus;
    try {
      status = await this.api.getStatus();
    } catch (err: unknown) {
      setToken(null);
      if (err instanceof ApiError && err.status === 401) {
        return { ok: false, message: `${sentence(TOKEN_NOT_ACCEPTED)} Check it and try again.` };
      }
      if (err instanceof UnreadableResponseError) return { ok: false, message: "The appliance answered, but its reply could not be read. Try again." };
      // A problem body is the appliance's own answer; any other failure (a
      // proxy's page, no answer) did not reach it.
      if (isRefusal(err)) return { ok: false, message: `The appliance refused the sign-in: ${apiErrorMessage(err)}.` };
      // A dropped connection's error text is the browser's and differs by
      // engine, so it is not quoted.
      return { ok: false, message: "Could not reach the appliance. Check the connection and try again." };
    }
    this.loginPending = false;
    this.emit("authok");
    await this.loadInitial(status);
    // loadInitial may have hit a fresh 401 (the token was revoked between the
    // verifying getStatus and the bulk load), which re-arms loginPending via
    // onUnauthorized. Resuming polling would only be rejected again, so report
    // failure and leave the prompt up instead. Drop the token as well: it was
    // just rejected, so keeping it would contradict this method's contract ("a
    // rejected token is not kept") and leave a dead credential in storage.
    if (this.loginPending) {
      setToken(null);
      return { ok: false, message: `${sentence(`${TOKEN_NOT_ACCEPTED} while loading`)} Try again.` };
    }
    this.startPolling();
    return { ok: true, message: "" };
  }

  public getState(): Readonly<AppState> {
    return this.state;
  }

  private initSSE(): void {
    this.sse.subscribe((eventName: string, data: unknown) => {
      if (eventName === "unauthorized") {
        this.onUnauthorized();
      } else if (eventName === "connected") {
        this.state.connected = true;
        this.emit("connection", true);
      } else if (eventName === "disconnected") {
        // During a deliberate token rotation the SSE connection carrying the old
        // token is dropped and reconnects under the new one within a backoff
        // interval. That blip is expected, so do not blink the connection
        // indicator to "Reconnecting" for it; a genuine drop (swapDepth 0) still
        // shows. endTokenSwap restarts the stream so the recovery is not skipped.
        if (this.swapDepth > 0) return;
        this.state.connected = false;
        this.emit("connection", false);
      } else if (eventName === "levels") {
        const payload = data as LevelsEvent;
        for (const dl of payload.devices) {
          this.state.levels.set(dl.name, dl);
        }
        this.emit("levels", this.state.levels);
      }
    });
  }

  // status, when given, is the /status body the caller already fetched to verify
  // the token (start and login), so it is not requested twice.
  public async loadInitial(status?: ApplianceStatus): Promise<void> {
    // Name every result rather than destructuring a prefix positionally: the
    // Promise.all order and the assignment order must agree, and a silent
    // misassignment (adding or reordering a refresh) is exactly the bug this
    // avoids. results[i] pairs 1:1 with the refresh at the same index below.
    const results = await Promise.all([
      this.refreshStatus(status),
      this.refreshDevices(),
      this.refreshSystem(),
      this.refreshConfig(),
      this.refreshAvailable(),
    ]);
    const [statusOk, devicesOk, systemOk, configOk, availableOk] = results;
    // Surface a per-resource load error so each view can offer a retry for its
    // own data instead of a "Loading..." placeholder that never resolves, and
    // so one failing endpoint does not blank another view that loaded fine.
    const coreFailed = !statusOk && !devicesOk;
    const systemFailed = !systemOk;
    // A config-only failure leaves the System view's network/access/notification
    // cards hidden (they unhide on the "config" event). Surface it so the miss is
    // not silent; the System view warns and polling recovers it on a later tick.
    const configFailed = !configOk;
    // The unconfigured-hardware list is advisory: a failure leaves it stale until
    // the next poll rather than blanking a view, so availableFailed never triggers
    // the load error on its own, but it is carried in the detail for completeness.
    const availableFailed = !availableOk;
    // A rejected token is handled by the login prompt, not the retry state.
    if (this.loginPending) return;
    if (coreFailed || systemFailed || configFailed) {
      this.emit("loaderror", { coreFailed, systemFailed, configFailed, availableFailed, message: "Could not reach the appliance." });
    }
  }

  // start is the boot sequence. It settles access before any gated request, so
  // a browser without a valid token sees the login prompt instead of a burst of
  // rejected loads (and their console errors). The open /healthz says whether a
  // token is required; with one required, a stored token is checked with a
  // single request first. It resolves true once loading and polling are
  // running, false when the login prompt is up instead (a successful login then
  // loads and starts polling itself).
  public async start(): Promise<boolean> {
    let authRequired = false;
    try {
      authRequired = (await this.api.getHealth()).authRequired === true;
    } catch {
      // Unreachable or an older appliance without the field: fall through to
      // the normal load, which surfaces the failure per view (or a 401 prompts).
    }
    // The token check's /status body seeds the load below, so a token-gated
    // boot fetches it once.
    let status: ApplianceStatus | undefined;
    if (authRequired) {
      if (!getToken()) {
        this.onUnauthorized();
        return false;
      }
      try {
        status = await this.api.getStatus();
      } catch {
        // A 401 has already raised the prompt through api.onUnauthorized; drop
        // the rejected token too, so the next page load goes straight to the
        // prompt instead of repeating the failed request. Any other failure is
        // left to the full load below to report per view.
        if (this.loginPending) {
          setToken(null);
          return false;
        }
      }
    }
    void this.loadInitial(status);
    this.startPolling();
    return true;
  }

  // retry re-runs the initial load; views call it from their error state.
  public retry(): Promise<void> {
    return this.loadInitial();
  }

  public startPolling(intervalMs: number = POLL_INTERVAL_MS): void {
    // Start the event stream before the early return: if polling is already on
    // while a prior stop()/start() left SSE stopped, returning early here would
    // leave the stream down. sse.start() is idempotent. Starting it ends any
    // hidden-page pause; a page still hidden gets a fresh grace period.
    this.sse.start();
    this.streamPaused = false;
    if (this.polling) {
      if (this.pageHidden) this.armStreamStop();
      return;
    }
    this.polling = true;
    this.pollIntervalMs = intervalMs;
    if (this.pageHidden) this.armStreamStop();
    else this.armPollTimer();
  }

  // setLevelsWanted says whether a view is showing levels (the dashboard).
  // Levels stop LEVELS_GRACE_MS after the dashboard leaves, by changing the
  // stream's event filter, and come back at once when one returns; a return
  // within the grace changes nothing. Dropping them announces levelsdropped.
  // Each filter change reconnects the stream once, and the connect re-sync
  // reloads the notifications snapshot, as on any reconnect.
  // The filter outlives a stopped stream (a 401, a hidden page), so a restart
  // keeps it.
  public setLevelsWanted(wanted: boolean): void {
    // The first call comes from the start-up route, before the stream opens:
    // a page that starts on another view never shows levels, so it opens the
    // stream without them rather than streaming them for the grace and then
    // reconnecting.
    if (!this.levelsDecided) {
      this.levelsDecided = true;
      this.levelsWanted = wanted;
      if (!wanted) {
        this.levelsDropped = true;
        this.sse.setEvents(NON_LEVEL_EVENTS);
      }
      return;
    }
    if (wanted === this.levelsWanted) return;
    this.levelsWanted = wanted;
    if (wanted) {
      if (this.levelsTimer !== null) {
        this.timers.clearTimeout(this.levelsTimer);
        this.levelsTimer = null;
      }
      if (this.levelsDropped) {
        this.levelsDropped = false;
        this.sse.setEvents(null);
      }
      return;
    }
    this.levelsTimer = this.timers.setTimeout(() => {
      this.levelsTimer = null;
      this.levelsDropped = true;
      this.sse.setEvents(NON_LEVEL_EVENTS);
      this.state.levels.clear();
      this.emit("levelsdropped");
    }, LEVELS_GRACE_MS);
  }

  public stopPolling(): void {
    this.polling = false;
    this.clearPollTimer();
    this.clearStreamStop();
    this.streamPaused = false;
    this.sse.stop();
    // stop() is silent, so say the stream is down, as the hidden-page stop
    // does: otherwise connected stays true through a login prompt, and the
    // notifications fallback after the login would take a stream that never
    // came back for a live one.
    if (this.state.connected) {
      this.state.connected = false;
      this.emit("connection", false);
    }
  }

  // setPageHidden pauses the poll while the page is hidden (a background tab or
  // a minimized window): nobody is looking, and every tick costs the appliance
  // five requests. The event stream stays up for HIDDEN_STREAM_GRACE_MS, so a
  // quick tab switch keeps notifications and their toasts arriving, and is then
  // stopped, so a forgotten tab does not keep the appliance sending it
  // levels. On showing again the page refreshes at once, so the views are
  // current without waiting an interval, and resumes the timer; a stopped
  // stream restarts, and its connect re-sync recovers any notification raised
  // meanwhile (toasts for those are not replayed).
  public setPageHidden(hidden: boolean): void {
    if (hidden === this.pageHidden) return;
    this.pageHidden = hidden;
    if (!this.polling) return;
    if (hidden) {
      this.clearPollTimer();
      this.armStreamStop();
      return;
    }
    this.clearStreamStop();
    if (this.streamPaused) {
      this.streamPaused = false;
      this.sse.start();
    }
    void this.pollOnce();
    this.armPollTimer();
  }

  // armStreamStop starts the hidden-page grace, keeping a deadline already set.
  private armStreamStop(): void {
    if (this.streamStopTimer !== null) return;
    this.streamStopTimer = this.timers.setTimeout(() => {
      this.streamStopTimer = null;
      // Every path that makes this moot also clears the timer; the check keeps
      // a stray fire from stopping a stream that is meant to be up.
      if (!this.polling || !this.pageHidden) return;
      this.streamPaused = true;
      this.sse.stop();
      // stop() is silent, so say the stream is down: the notification store
      // drops a pending re-sync retry (the restart's connect re-syncs), and the
      // indicator reads "Reconnecting" until the restarted stream connects.
      if (this.state.connected) {
        this.state.connected = false;
        this.emit("connection", false);
      }
    }, HIDDEN_STREAM_GRACE_MS);
  }

  private clearStreamStop(): void {
    if (this.streamStopTimer !== null) {
      this.timers.clearTimeout(this.streamStopTimer);
      this.streamStopTimer = null;
    }
  }

  private armPollTimer(): void {
    if (this.pollIntervalTimer !== null) return;
    this.pollIntervalTimer = this.timers.setInterval(() => void this.pollOnce(), this.pollIntervalMs);
  }

  private clearPollTimer(): void {
    if (this.pollIntervalTimer !== null) {
      this.timers.clearInterval(this.pollIntervalTimer);
      this.pollIntervalTimer = null;
    }
  }

  // pollOnce is one poll tick.
  private async pollOnce(): Promise<void> {
    await Promise.allSettled([
      this.refreshStatus(),
      this.refreshDevices(),
      this.refreshSystem(),
      this.refreshAvailable(),
      // Poll config too so a change made by another client (or another tab)
      // reflects here within one interval instead of only after a reload. The
      // configGate makes this safe against the lost-update race that
      // originally kept config out of the poll: an in-flight GET that resolves
      // after a newer applyConfig or a newer applied refresh is dropped.
      this.refreshConfig(),
    ]);
  }

  // Each refresh below is one gatedRefresh (lib/store-core.ts): take a gate
  // token, fetch, and apply only if the gate accepts it. A refresh whose body is
  // dropped, or that fails after a newer body was applied, still reports
  // success: fresher data is in place, so loadInitial must not raise a load
  // error for it.
  //
  // status, devices, system and available announce only when the applied data
  // changed (a ChangeTracker each). In practice this saves work on devices and
  // available: status carries uptimeSeconds and system carries live CPU, memory
  // and network counters, so both still announce nearly every tick (which the
  // System view's certificate refresh and the uptime displays rely on). config
  // announces every poll, because mutation flows repaint from it. The first
  // applied value always announces. A failed read of status, devices or system
  // resets its tracker, so the next successful read announces even when it
  // returns data a view showed before swapping in a load error; that is what
  // repairs the view. A failure that a newer applied read already superseded
  // resets nothing (the view kept fresh data), and available never resets,
  // since no view replaces the list with a load error, so there is nothing for
  // a re-announcement to repair.

  // prefetched, when given, is a status the caller just fetched (the boot and
  // login token check), applied through the same gate instead of a second GET.
  public refreshStatus(prefetched?: ApplianceStatus): Promise<boolean> {
    return gatedRefresh(
      this.statusGate,
      () => (prefetched ? Promise.resolve(prefetched) : this.api.getStatus()),
      (status) => {
        this.state.status = status;
        if (this.statusChange.changed(status)) {
          this.emit("status", status);
        }
      },
      (err, superseded) => {
        console.warn("Failed to refresh status:", err);
        if (!superseded) this.statusChange.reset();
      },
    );
  }

  public refreshAvailable(): Promise<boolean> {
    return gatedRefresh(
      this.availableGate,
      () => this.api.getAvailableDevices(),
      (available) => {
        this.state.available = available;
        if (this.availableChange.changed(available)) {
          this.emit("available", this.state.available);
        }
      },
      (err) => console.warn("Failed to refresh available devices:", err),
    );
  }

  public refreshDevices(): Promise<boolean> {
    return gatedRefresh(
      this.devicesGate,
      async () => {
        const devices = await this.api.getDevices();
        // Defensive normalization at the store boundary: the contract guarantees
        // channels is an array, but every consumer indexes it, so a malformed
        // payload becomes an empty selection rather than a runtime error. It runs
        // inside the fetch, before accept, so a payload that throws here (not an
        // array at all) never marks this token applied and so never drops an
        // older valid response.
        for (const d of devices) {
          if (!Array.isArray(d.channels)) d.channels = [];
        }
        return devices;
      },
      (devices) => {
        this.state.devices = devices;
        // Drop level entries for devices that are no longer present so the map
        // does not grow without bound as devices are added or removed.
        const present = new Set(this.state.devices.map((d) => d.name));
        for (const name of this.state.levels.keys()) {
          if (!present.has(name)) this.state.levels.delete(name);
        }
        if (this.devicesChange.changed(devices)) {
          this.emit("devices", this.state.devices);
        }
      },
      (err, superseded) => {
        console.warn("Failed to refresh devices:", err);
        if (!superseded) this.devicesChange.reset();
      },
    );
  }

  public refreshSystem(): Promise<boolean> {
    return gatedRefresh(
      this.systemGate,
      () => this.api.getSystem(),
      (system) => {
        this.state.system = system;
        if (this.systemChange.changed(system)) {
          this.emit("system", system);
        }
      },
      // System info is optional, non-fatal: no warning, just re-arm the tracker
      // unless a newer read already applied.
      (_err, superseded) => {
        if (!superseded) this.systemChange.reset();
      },
    );
  }

  public refreshConfig(): Promise<boolean> {
    // A newer applyConfig or applied refreshConfig that landed while this GET was
    // in flight is fresher, so the gate drops this stale body.
    return gatedRefresh(
      this.configGate,
      () => this.api.getConfig(),
      (config) => {
        this.state.config = config;
        this.emit("config", config);
      },
      (err) => console.warn("Failed to refresh config:", err),
    );
  }

  // applyConfig records the authoritative config the server returned from a
  // successful PATCH /config, so the cached config reflects the change even if
  // the follow-up GET refresh fails. That matters because refreshConfig swallows
  // its error and leaves config stale on failure; without this, a later queued
  // mutation would rebuild its full-array PATCH from the stale base and silently
  // clobber this change. It also seeds the cache ahead of the next poll rather
  // than waiting an interval for the fresh value. Using the PATCH response (not
  // the request body) seeds a config that was never loaded (initial GET failed)
  // and picks up any server-side normalization.
  public applyConfig(config: Config): void {
    // Invalidate the gate so a GET /config already in flight (from an overlapping
    // refreshConfig) cannot overwrite this authoritative PATCH result when it
    // resolves later.
    this.configGate.invalidate();
    this.state.config = config;
    this.emit("config", config);
  }

  // applyUpdateStatus records the update state an update check or an update
  // request returned, or the one the view derives after saving the
  // update-check switch, as applyConfig does for a PATCH. A GET /system already
  // in flight was read before that request, so the gate drops it: from here on
  // every "system" event carries a state read after the request, and a view
  // following an update can trust each one. Without a system snapshot yet
  // there is nothing to merge into; the next read is post-request anyway.
  public applyUpdateStatus(update: UpdateStatus): void {
    this.systemGate.invalidate();
    const system = this.state.system;
    if (!system) return;
    const next = { ...system, update };
    this.state.system = next;
    if (this.systemChange.changed(next)) this.emit("system", next);
  }
}

export const store = new AppStore();
