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
        await Promise.allSettled([
            this.refreshStatus(),
            this.refreshDevices(),
            this.refreshSystem(),
            this.refreshConfig(),
        ]);
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
        }
        catch (err) {
            console.warn("Failed to refresh status:", err);
        }
    }
    async refreshDevices() {
        try {
            this.state.devices = await api.getDevices();
            this.dispatchEvent(new CustomEvent("devices", { detail: this.state.devices }));
        }
        catch (err) {
            console.warn("Failed to refresh devices:", err);
        }
    }
    async refreshSystem() {
        try {
            this.state.system = await api.getSystem();
            this.dispatchEvent(new CustomEvent("system", { detail: this.state.system }));
        }
        catch {
            // System info is optional, non-fatal
        }
    }
    async refreshConfig() {
        try {
            this.state.config = await api.getConfig();
            this.dispatchEvent(new CustomEvent("config", { detail: this.state.config }));
        }
        catch (err) {
            console.warn("Failed to refresh config:", err);
        }
    }
}
export const store = new AppStore();
