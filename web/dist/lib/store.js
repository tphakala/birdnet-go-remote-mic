import { api } from "./api.js";
import { sse } from "./sse.js";
export class AppStore extends EventTarget {
    state = {
        status: null,
        devices: [],
        levels: new Map(),
        system: null,
        config: null,
        connected: false,
    };
    pollIntervalTimer = null;
    constructor() {
        super();
        this.initSSE();
    }
    getState() {
        return this.state;
    }
    initSSE() {
        sse.subscribe((eventName, data) => {
            if (eventName === "connected") {
                this.state.connected = true;
                this.dispatchEvent(new CustomEvent("connection", { detail: true }));
            }
            else if (eventName === "disconnected") {
                this.state.connected = false;
                this.dispatchEvent(new CustomEvent("connection", { detail: false }));
            }
            else if (eventName === "levels") {
                const payload = data;
                for (const dl of payload.devices) {
                    this.state.levels.set(dl.name, dl);
                }
                this.dispatchEvent(new CustomEvent("levels", { detail: this.state.levels }));
            }
        });
    }
    async loadInitial() {
        const [statusOk, devicesOk, systemOk] = await Promise.all([
            this.refreshStatus(),
            this.refreshDevices(),
            this.refreshSystem(),
            this.refreshConfig(),
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
    retry() {
        return this.loadInitial();
    }
    startPolling(intervalMs = 3000) {
        if (this.pollIntervalTimer !== null)
            return;
        sse.start();
        this.pollIntervalTimer = window.setInterval(async () => {
            await Promise.allSettled([
                this.refreshStatus(),
                this.refreshDevices(),
                this.refreshSystem(),
            ]);
        }, intervalMs);
    }
    stopPolling() {
        if (this.pollIntervalTimer !== null) {
            clearInterval(this.pollIntervalTimer);
            this.pollIntervalTimer = null;
        }
        sse.stop();
    }
    async refreshStatus() {
        try {
            this.state.status = await api.getStatus();
            this.dispatchEvent(new CustomEvent("status", { detail: this.state.status }));
            return true;
        }
        catch (err) {
            console.warn("Failed to refresh status:", err);
            return false;
        }
    }
    async refreshDevices() {
        try {
            this.state.devices = await api.getDevices();
            // Drop level entries for devices that are no longer present so the map
            // does not grow without bound as devices are added or removed.
            const present = new Set(this.state.devices.map((d) => d.name));
            for (const name of this.state.levels.keys()) {
                if (!present.has(name))
                    this.state.levels.delete(name);
            }
            this.dispatchEvent(new CustomEvent("devices", { detail: this.state.devices }));
            return true;
        }
        catch (err) {
            console.warn("Failed to refresh devices:", err);
            return false;
        }
    }
    async refreshSystem() {
        try {
            this.state.system = await api.getSystem();
            this.dispatchEvent(new CustomEvent("system", { detail: this.state.system }));
            return true;
        }
        catch {
            // System info is optional, non-fatal.
            return false;
        }
    }
    async refreshConfig() {
        try {
            this.state.config = await api.getConfig();
            this.dispatchEvent(new CustomEvent("config", { detail: this.state.config }));
            return true;
        }
        catch (err) {
            console.warn("Failed to refresh config:", err);
            return false;
        }
    }
}
export const store = new AppStore();
