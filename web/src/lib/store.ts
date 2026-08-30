import { api } from "./api.js";
import { sse } from "./sse.js";
import type {
  ApplianceStatus,
  Config,
  Device,
  DeviceLevels,
  LevelsEvent,
  SystemInfo,
} from "./types.js";

export interface AppState {
  status: ApplianceStatus | null;
  devices: Device[];
  levels: Map<string, DeviceLevels>;
  system: SystemInfo | null;
  config: Config | null;
  connected: boolean;
}

export class AppStore extends EventTarget {
  private state: AppState = {
    status: null,
    devices: [],
    levels: new Map(),
    system: null,
    config: null,
    connected: false,
  };

  private pollIntervalTimer: number | null = null;

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
    await Promise.allSettled([
      this.refreshStatus(),
      this.refreshDevices(),
      this.refreshSystem(),
      this.refreshConfig(),
    ]);
  }

  public startPolling(intervalMs: number = 3000): void {
    if (this.pollIntervalTimer !== null) return;
    sse.start();
    this.pollIntervalTimer = window.setInterval(async () => {
      await Promise.allSettled([
        this.refreshStatus(),
        this.refreshDevices(),
        this.refreshSystem(),
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

  public async refreshStatus(): Promise<void> {
    try {
      this.state.status = await api.getStatus();
      this.dispatchEvent(new CustomEvent("status", { detail: this.state.status }));
    } catch (err) {
      console.warn("Failed to refresh status:", err);
    }
  }

  public async refreshDevices(): Promise<void> {
    try {
      this.state.devices = await api.getDevices();
      // Drop level entries for devices that are no longer present so the map
      // does not grow without bound as devices are added or removed.
      const present = new Set(this.state.devices.map((d) => d.name));
      for (const name of this.state.levels.keys()) {
        if (!present.has(name)) this.state.levels.delete(name);
      }
      this.dispatchEvent(new CustomEvent("devices", { detail: this.state.devices }));
    } catch (err) {
      console.warn("Failed to refresh devices:", err);
    }
  }

  public async refreshSystem(): Promise<void> {
    try {
      this.state.system = await api.getSystem();
      this.dispatchEvent(new CustomEvent("system", { detail: this.state.system }));
    } catch {
      // System info is optional, non-fatal
    }
  }

  public async refreshConfig(): Promise<void> {
    try {
      this.state.config = await api.getConfig();
      this.dispatchEvent(new CustomEvent("config", { detail: this.state.config }));
    } catch (err) {
      console.warn("Failed to refresh config:", err);
    }
  }
}

export const store = new AppStore();
