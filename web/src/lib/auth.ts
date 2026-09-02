// Access-token persistence for the web UI. The appliance's shared token gates
// every API call and the SSE stream; the UI keeps it in localStorage so the
// dashboard does not prompt on every tab, and pushes it into the API and SSE
// clients whenever it changes. The appliance is LAN-only and the token is
// already shown on the Access Control card, so per-device persistence is an
// acceptable trade for not re-typing it.
import { api } from "./api.js";
import { sse } from "./sse.js";

const STORAGE_KEY = "remote-mic-token";

// current mirrors the in-force token in memory. It is the source of truth for
// getToken; localStorage is persistence only. A storage-blocked browser (a
// private window, a blocked origin) cannot read the token back, so without this
// getToken would return null right after a successful login and the credentialed
// copy-URL affordance would silently drop the token. Written by setToken and
// applyStoredToken.
let current: string | null = null;

// readStored returns the persisted token, or null when none is stored or storage
// is unavailable.
function readStored(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

// getToken returns the in-force token from memory, so it works even when storage
// is unavailable.
export function getToken(): string | null {
  return current;
}

// setToken stores (or clears, with null) the token and applies it to the API
// and SSE clients synchronously, so the very next request carries it. Callers
// rotating the token must call this the moment the PATCH resolves, before any
// follow-up refresh, or a poll racing the swap would be rejected.
export function setToken(token: string | null): void {
  current = token;
  try {
    if (token) localStorage.setItem(STORAGE_KEY, token);
    else localStorage.removeItem(STORAGE_KEY);
  } catch {
    // Storage unavailable: the token still applies for this page's lifetime via
    // the in-memory `current`.
  }
  api.setToken(token);
  sse.setToken(token);
}

// applyStoredToken pushes a previously stored token into the clients at boot and
// seeds the in-memory copy.
export function applyStoredToken(): void {
  current = readStored();
  api.setToken(current);
  sse.setToken(current);
}

// generateToken returns 16 random bytes as 32 hex characters, which satisfies
// the appliance's token rule (12..128 URL-unreserved characters).
export function generateToken(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}
