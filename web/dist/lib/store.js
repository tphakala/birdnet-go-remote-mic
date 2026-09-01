import { api, ApiError } from "./api.js";
import { sse } from "./sse.js";
import { setToken } from "./auth.js";
export class AppStore extends EventTarget {
    state = {
        status: null,
        devices: [],
        available: [],
        levels: new Map(),
        system: null,
        config: null,
        connected: false,
    };
    pollIntervalTimer = null;
    // Monotonic generation for config writes. A GET /config that was already in
    // flight when a newer applyConfig (or a newer refreshConfig) landed must not
    // overwrite the fresher value with its stale body, which would resurrect a
    // stale base for the next queued mutation. Both writers bump it; a refresh
    // commits only while its captured generation is still current.
    configEpoch = 0;
    // Monotonic generation for available-device refreshes. Overlapping polls can
    // resolve out of order (a provision/removal triggers an extra refresh that can
    // race the 3s poll), so an older response must not overwrite a newer one and
    // restore a stale Enable card.
    availableEpoch = 0;
    // loginPending is set from the first 401 until a token is accepted, so a
    // burst of rejected requests (the initial load fires five) opens one prompt
    // and the generic load-error state is suppressed in favor of it.
    loginPending = false;
    constructor() {
        super();
        api.onUnauthorized = () => this.onUnauthorized();
        this.initSSE();
    }
    // onUnauthorized reacts to the appliance rejecting the UI's credentials:
    // polling and the SSE stream stop (they would only be rejected again) and the
    // login prompt is asked for, once per outage.
    onUnauthorized() {
        if (this.loginPending)
            return;
        this.loginPending = true;
        this.stopPolling();
        this.dispatchEvent(new CustomEvent("authrequired"));
    }
    // login stores token, verifies it against /status, and on success reloads
    // everything and resumes polling. A rejected token is not kept.
    async login(token) {
        setToken(token);
        try {
            await api.getStatus();
        }
        catch (err) {
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
        this.startPolling();
        return { ok: true, message: "" };
    }
    getState() {
        return this.state;
    }
    initSSE() {
        sse.subscribe((eventName, data) => {
            if (eventName === "unauthorized") {
                this.onUnauthorized();
            }
            else if (eventName === "connected") {
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
            this.refreshAvailable(),
        ]);
        // Surface a per-resource load error so each view can offer a retry for its
        // own data instead of a "Loading..." placeholder that never resolves, and
        // so one failing endpoint does not blank another view that loaded fine.
        const coreFailed = !statusOk && !devicesOk;
        const systemFailed = !systemOk;
        // A rejected token is handled by the login prompt, not the retry state.
        if (this.loginPending)
            return;
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
                this.refreshAvailable(),
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
    async refreshAvailable() {
        const epoch = ++this.availableEpoch;
        try {
            const available = await api.getAvailableDevices();
            // A newer refreshAvailable started while this GET was in flight; its result
            // is fresher, so drop this stale body rather than restoring a stale list.
            if (epoch !== this.availableEpoch)
                return true;
            this.state.available = available;
            this.dispatchEvent(new CustomEvent("available", { detail: this.state.available }));
            return true;
        }
        catch (err) {
            console.warn("Failed to refresh available devices:", err);
            return false;
        }
    }
    async refreshDevices() {
        try {
            const devices = await api.getDevices();
            // Defensive normalization at the store boundary: the contract guarantees
            // channels is an array, but every consumer indexes it, so a malformed
            // payload becomes an empty selection rather than a runtime error.
            for (const d of devices) {
                if (!Array.isArray(d.channels))
                    d.channels = [];
            }
            this.state.devices = devices;
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
        const epoch = ++this.configEpoch;
        try {
            const config = await api.getConfig();
            // A newer applyConfig or refreshConfig ran while this GET was in flight;
            // its result is fresher, so drop this stale body rather than clobbering it.
            if (epoch !== this.configEpoch)
                return true;
            this.state.config = config;
            this.dispatchEvent(new CustomEvent("config", { detail: this.state.config }));
            return true;
        }
        catch (err) {
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
    applyConfig(config) {
        // Bump the generation so a GET /config already in flight (from an overlapping
        // refreshConfig) cannot overwrite this authoritative PATCH result when it
        // resolves later.
        ++this.configEpoch;
        this.state.config = config;
        this.dispatchEvent(new CustomEvent("config", { detail: config }));
    }
}
export const store = new AppStore();
