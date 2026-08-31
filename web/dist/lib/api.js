export class ApiError extends Error {
    status;
    title;
    detail;
    errors;
    constructor(status, title, detail, errors) {
        super(detail || title);
        this.status = status;
        this.title = title;
        this.detail = detail;
        this.errors = errors;
        this.name = "ApiError";
    }
}
export class ApiClient {
    baseUrl;
    token = null;
    constructor(baseUrl = "/api/v1") {
        this.baseUrl = baseUrl;
    }
    setToken(token) {
        this.token = token;
    }
    async request(path, options = {}) {
        const headers = new Headers(options.headers || {});
        headers.set("Accept", "application/json, application/problem+json");
        if (this.token) {
            headers.set("Authorization", `Bearer ${this.token}`);
        }
        if (options.body && !headers.has("Content-Type")) {
            headers.set("Content-Type", "application/json");
        }
        const res = await fetch(`${this.baseUrl}${path}`, {
            ...options,
            headers,
        });
        if (res.status === 204) {
            return {};
        }
        const contentType = res.headers.get("Content-Type") || "";
        const isJson = contentType.includes("application/json") || contentType.includes("application/problem+json");
        if (!res.ok) {
            if (isJson) {
                const prob = (await res.json());
                throw new ApiError(prob.status || res.status, prob.title || res.statusText, prob.detail, prob.errors);
            }
            const text = await res.text();
            throw new ApiError(res.status, res.statusText, text);
        }
        if (isJson) {
            return (await res.json());
        }
        return (await res.text());
    }
    async getHealth() {
        return this.request("/healthz");
    }
    async getStatus() {
        return this.request("/status");
    }
    async getDevices() {
        return this.request("/devices");
    }
    async getDevice(name) {
        return this.request(`/devices/${encodeURIComponent(name)}`);
    }
    async getAvailableDevices() {
        return this.request("/devices/available");
    }
    async provisionDevice(req) {
        return this.request("/devices", {
            method: "POST",
            body: JSON.stringify(req),
        });
    }
    async deleteDevice(name) {
        await this.request(`/devices/${encodeURIComponent(name)}`, {
            method: "DELETE",
        });
    }
    async getConfig() {
        return this.request("/config");
    }
    async patchConfig(patch) {
        return this.request("/config", {
            method: "PATCH",
            body: JSON.stringify(patch),
        });
    }
    async getSystem() {
        return this.request("/system");
    }
    async postSystemRestart() {
        return this.request("/system/restart", {
            method: "POST",
        });
    }
}
export const api = new ApiClient();
