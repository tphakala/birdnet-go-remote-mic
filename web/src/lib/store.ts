import { api } from "./api.js";
import { sse } from "./sse.js";
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
  // Monotonic generation for config writes. A GET /config that was already in
  // flight when a newer applyConfig (or a newer refreshConfig) landed must not
  // overwrite the fresher value with its stale body, which would resurrect a
  // stale base for the next queued mutation. Both writers bump it; a refresh
  // commits only while its captured generation is still current.
  private configEpoch = 0;

  constructor() {
    super();
    this.initSSE();
  }

  public getState(): Readonly<AppState> {
    return this.state;
  }

  private initSSE(): void {
    sse.subscribe((eventName: string, data: unknown) => {
      if (eventName === "connected") {
        this.state.connected = true;
        this.dispatchEvent(new CustomEvent("connection", { detail: true }));
      } else if (eventName === "disconnected") {
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
    const [statusOk, devicesOk, systemOk] = await Promise.all([
      this.refreshStatus(),
      this.refreshDevices(),
      this.refreshSystem(),
      this.refreshConfig(),
      this.refreshAvailable(),
    ]);
    // Surface a per-resource load error so each view can offer a retry for its
    // own data instead of a "Loading..." placeholder that never resolves, and
    // so one failing endpoint does not blank another view that loaded fine.
    const coreFailed = !statusOk && !devicesOk;
    const systemFailed = !systemOk;
    if (coreFailed || systemFailed) {
      this.dispatchEvent(new CustomEvent("loaderror", {
        detail: { coreFailed, systemFailed, message: "Could not reach the appliance." },
      }));
    }
  }

  // retry re-runs the initial load; views call it from their error state.
  public retry(): Promise<void> {
    return this.loadInitial();
  }

  public startPolling(intervalMs: number = 3000): void {
    if (this.pollIntervalTimer !== null) return;
    sse.start();
    this.pollIntervalTimer = window.setInterval(async () => {
      await Promise.allSettled([
        this.refreshStatus(),
        this.refreshDevices(),
        this.refreshSystem(),
        this.refreshAvailable(),
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

  public async refreshStatus(): Promise<boolean> {
    try {
      this.state.status = await api.getStatus();
      this.dispatchEvent(new CustomEvent("status", { detail: this.state.status }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh status:", err);
      return false;
    }
  }

  public async refreshAvailable(): Promise<boolean> {
    try {
      this.state.available = await api.getAvailableDevices();
      this.dispatchEvent(new CustomEvent("available", { detail: this.state.available }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh available devices:", err);
      return false;
    }
  }

  public async refreshDevices(): Promise<boolean> {
    try {
      this.state.devices = await api.getDevices();
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
      return false;
    }
  }

  public async refreshSystem(): Promise<boolean> {
    try {
      this.state.system = await api.getSystem();
      this.dispatchEvent(new CustomEvent("system", { detail: this.state.system }));
      return true;
    } catch {
      // System info is optional, non-fatal.
      return false;
    }
  }

  public async refreshConfig(): Promise<boolean> {
    const epoch = ++this.configEpoch;
    try {
      const config = await api.getConfig();
      // A newer applyConfig or refreshConfig ran while this GET was in flight;
      // its result is fresher, so drop this stale body rather than clobbering it.
      if (epoch !== this.configEpoch) return true;
      this.state.config = config;
      this.dispatchEvent(new CustomEvent("config", { detail: this.state.config }));
      return true;
    } catch (err) {
      console.warn("Failed to refresh config:", err);
      return false;
    }
  }

  // applyConfig records the authoritative config the server returned from a
  // successful PATCH /config, so the cached config reflects the change even if
  // the follow-up GET refresh fails. That matters because refreshConfig swallows
  // its error and leaves config stale on failure, and the 3s poll never refreshes
  // config; without this, a later queued mutation would rebuild its full-array
  // PATCH from the stale base and silently clobber this change. Using the PATCH
  // response (not the request body) also seeds a config that was never loaded
  // (initial GET failed) and picks up any server-side normalization.
  public applyConfig(config: Config): void {
    // Bump the generation so a GET /config already in flight (from an overlapping
    // refreshConfig) cannot overwrite this authoritative PATCH result when it
    // resolves later.
    ++this.configEpoch;
    this.state.config = config;
    this.dispatchEvent(new CustomEvent("config", { detail: config }));
  }
}

export const store = new AppStore();
