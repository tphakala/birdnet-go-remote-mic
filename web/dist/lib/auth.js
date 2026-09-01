// Access-token persistence for the web UI. The appliance's shared token gates
// every API call and the SSE stream; the UI keeps it in localStorage so the
// dashboard does not prompt on every tab, and pushes it into the API and SSE
// clients whenever it changes. The appliance is LAN-only and the token is
// already shown on the Access Control card, so per-device persistence is an
// acceptable trade for not re-typing it.
import { api } from "./api.js";
import { sse } from "./sse.js";
const STORAGE_KEY = "remote-mic-token";
// getToken returns the stored token, or null when none is stored or storage is
// unavailable (a private window, a blocked origin).
export function getToken() {
    try {
        return localStorage.getItem(STORAGE_KEY);
    }
    catch {
        return null;
    }
}
// setToken stores (or clears, with null) the token and applies it to the API
// and SSE clients synchronously, so the very next request carries it. Callers
// rotating the token must call this the moment the PATCH resolves, before any
// follow-up refresh, or a poll racing the swap would be rejected.
export function setToken(token) {
    try {
        if (token)
            localStorage.setItem(STORAGE_KEY, token);
        else
            localStorage.removeItem(STORAGE_KEY);
    }
    catch {
        // Storage unavailable: the token still applies for this page's lifetime.
    }
    api.setToken(token);
    sse.setToken(token);
}
// applyStoredToken pushes a previously stored token into the clients at boot.
export function applyStoredToken() {
    const token = getToken();
    api.setToken(token);
    sse.setToken(token);
}
// generateToken returns 16 random bytes as 32 hex characters, which satisfies
// the appliance's token rule (12..128 URL-unreserved characters).
export function generateToken() {
    const bytes = new Uint8Array(16);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}
