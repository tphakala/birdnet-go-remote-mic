import { api, ApiError } from "./api.js";
import { sse } from "./sse.js";
import { getToken, setToken } from "./auth.js";
import { LatestGate } from "./latest-core.js";
import type {
  ApplianceStatus,
  AvailableDevice,
  Config,
  Device,
  DeviceLevels,
  LevelsEvent,
  SystemInfo,
} from "./types.js";

export interface AppState {
  status: ApplianceStatus | null;
  devices: Device[];
  available: AvailableDevice[];
  levels: Map<string, DeviceLevels>;
  system: SystemInfo | null;
  config: Config | null;
  connected: boolean;
}

export class AppStore extends EventTarget {
  private state: AppState = {
    status: null,
    devices: [],
    available: [],
    levels: new Map(),
    system: null,
    config: null,
    connected: false,
  };

  private pollIntervalTimer: number | null = null;
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

  constructor() {
    super();
    api.onUnauthorized = () => this.onUnauthorized();
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
    // pollIntervalTimer !== null is the invariant for "polling is active", and the
    // SSE stream runs exactly while polling does (startPolling starts both,
    // stopPolling stops both), so a non-null timer means the stream is meant to be
    // up and safe to (re)start here; a null timer means we are not polling and must
    // not resurrect the stream.
    if (this.swapDepth === 0 && this.pollIntervalTimer !== null) sse.start();
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
    this.dispatchEvent(new CustomEvent("authrequired"));
  }

  // login stores token, verifies it against /status, and on success reloads
  // everything and resumes polling. A rejected token is not kept.
  public async login(token: string): Promise<{ ok: boolean; message: string }> {
    setToken(token);
    try {
      await api.getStatus();
    } catch (err: unknown) {
      setToken(null);
      if (err instanceof ApiError && err.status === 401) {
        return { ok: false, message: "That token was rejected. Check it and try again." };
      }
      const msg = err instanceof Error ? err.message : String(err);
      return { ok: false, message: `Could not reach the appliance: ${msg}` };
    }
    this.loginPending = false;
    this.dispatchEvent(new CustomEvent("authok"));
    await this.loadInitial();
    // loadInitial may have hit a fresh 401 (the token was revoked between the
    // verifying getStatus and the bulk load), which re-arms loginPending via
    // onUnauthorized. Resuming polling would only be rejected again, so report
    // failure and leave the prompt up instead. Drop the token as well: it was
    // just rejected, so keeping it would contradict this method's contract ("a
    // rejected token is not kept") and leave a dead credential in storage.
    if (this.loginPending) {
      setToken(null);
      return { ok: false, message: "The appliance rejected the token during load. Try again." };
    }
    this.startPolling();
    return { ok: true, message: "" };
  }

  public getState(): Readonly<AppState> {
    return this.state;
  }

  private initSSE(): void {
    sse.subscribe((eventName: string, data: unknown) => {
      if (eventName === "unauthorized") {
        this.onUnauthorized();
      } else if (eventName === "connected") {
        this.state.connected = true;
        this.dispatchEvent(new CustomEvent("connection", { detail: true }));
      } else if (eventName === "disconnected") {
        // During a deliberate token rotation the SSE connection carrying the old
        // token is dropped and reconnects under the new one within a backoff
        // interval. That blip is expected, so do not blink the connection
        // indicator to "Reconnecting" for it; a genuine drop (swapDepth 0) still
        // shows. endTokenSwap restarts the stream so the recovery is not skipped.
        if (this.swapDepth > 0) return;
        this.state.connected = false;
        this.dispatchEvent(new CustomEvent("connection", { detail: false }));
      } else if (eventName === "levels") {
        const payload = data as LevelsEvent;
        for (const dl of payload.devices) {
          this.state.levels.set(dl.name, dl);
        }
        this.dispatchEvent(new CustomEvent("levels", { detail: this.state.levels }));
      }
    });
  }

  public async loadInitial(): Promise<void> {
    // Name every result rather than destructuring a prefix positionally: the
    // Promise.all order and the assignment order must agree, and a silent
    // misassignment (adding or reordering a refresh) is exactly the bug this
    // avoids. results[i] pairs 1:1 with the refresh at the same index below.
    const results = await Promise.all([
      this.refreshStatus(),
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
      this.dispatchEvent(new CustomEvent("loaderror", {
        detail: { coreFailed, systemFailed, configFailed, availableFailed, message: "Could not reach the appliance." },
      }));
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
      authRequired = (await api.getHealth()).authRequired === true;
    } catch {
      // Unreachable or an older appliance without the field: fall through to
      // the normal load, which surfaces the failure per view (or a 401 prompts).
    }
    if (authRequired) {
      if (!getToken()) {
        this.onUnauthorized();
        return false;
      }
      try {
        await api.getStatus();
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
    void this.loadInitial();
    this.startPolling();
    return true;
  }

  // retry re-runs the initial load; views call it from their error state.
  public retry(): Promise<void> {
    return this.loadInitial();
  }

  public startPolling(intervalMs: number = 3000): void {
    // Start the event stream before the early return: if the poll timer is
    // already running while a prior stop()/start() left SSE stopped, returning
    // early here would leave the stream down. sse.start() is idempotent.
    sse.start();
    if (this.pollIntervalTimer !== null) return;
    this.pollIntervalTimer = window.setInterval(async () => {
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
    }, intervalMs);
  }

  public stopPolling(): void {
    if (this.pollIntervalTimer !== null) {
      clearInterval(this.pollIntervalTimer);
      this.pollIntervalTimer = null;
    }
    sse.stop();
  }

  // Each refresh below follows the same shape: take a gate token, fetch, and
  // apply only if the gate accepts it. A refresh whose body is dropped, or that
  // fails after a newer body was applied, still reports success: fresher data is
  // in place, so loadInitial must not raise a load error for it.

  public async refreshStatus(): Promise<boolean> {
    const token = this.statusGate.begin();
    try {
      const status = await api.getStatus();
      if (!this.statusGate.accept(token)) return true;
      this.state.status = status;
      this.dispatchEvent(new CustomEvent("status", { detail: this.state.status }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh status:", err);
      return this.statusGate.superseded(token);
    }
  }

  public async refreshAvailable(): Promise<boolean> {
    const token = this.availableGate.begin();
    try {
      const available = await api.getAvailableDevices();
      if (!this.availableGate.accept(token)) return true;
      this.state.available = available;
      this.dispatchEvent(new CustomEvent("available", { detail: this.state.available }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh available devices:", err);
      return this.availableGate.superseded(token);
    }
  }

  public async refreshDevices(): Promise<boolean> {
    const token = this.devicesGate.begin();
    try {
      const devices = await api.getDevices();
      // Defensive normalization at the store boundary: the contract guarantees
      // channels is an array, but every consumer indexes it, so a malformed
      // payload becomes an empty selection rather than a runtime error. It runs
      // before accept so a payload that throws here (not an array at all) never
      // marks this token applied and so never drops an older valid response.
      for (const d of devices) {
        if (!Array.isArray(d.channels)) d.channels = [];
      }
      if (!this.devicesGate.accept(token)) return true;
      this.state.devices = devices;
      // Drop level entries for devices that are no longer present so the map
      // does not grow without bound as devices are added or removed.
      const present = new Set(this.state.devices.map((d) => d.name));
      for (const name of this.state.levels.keys()) {
        if (!present.has(name)) this.state.levels.delete(name);
      }
      this.dispatchEvent(new CustomEvent("devices", { detail: this.state.devices }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh devices:", err);
      return this.devicesGate.superseded(token);
    }
  }

  public async refreshSystem(): Promise<boolean> {
    const token = this.systemGate.begin();
    try {
      const system = await api.getSystem();
      if (!this.systemGate.accept(token)) return true;
      this.state.system = system;
      this.dispatchEvent(new CustomEvent("system", { detail: this.state.system }));
      return true;
    } catch {
      // System info is optional, non-fatal.
      return this.systemGate.superseded(token);
    }
  }

  public async refreshConfig(): Promise<boolean> {
    const token = this.configGate.begin();
    try {
      const config = await api.getConfig();
      // A newer applyConfig or applied refreshConfig landed while this GET was in
      // flight; its result is fresher, so drop this stale body.
      if (!this.configGate.accept(token)) return true;
      this.state.config = config;
      this.dispatchEvent(new CustomEvent("config", { detail: this.state.config }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh config:", err);
      return this.configGate.superseded(token);
    }
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
    this.dispatchEvent(new CustomEvent("config", { detail: config }));
  }
}

export const store = new AppStore();
