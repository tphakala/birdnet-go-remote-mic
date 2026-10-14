// Pure, DOM-free toast decisions, split out so they can be unit tested with
// node:test (see web/test/toast-core.test.ts) without a DOM.

export type ToastType = "ok" | "info" | "warn" | "error";

export const TOAST_TYPES: readonly ToastType[] = ["ok", "info", "warn", "error"];

export const INFO_TTL_MS = 3200;
// A failed provisioning, removal or save deserves more than a glance: errors
// live in an assertive region, stay longer, and can be dismissed by hand (as
// can any other toast given this long a duration).
export const ERROR_TTL_MS = 8000;

export interface ToastView {
  type: ToastType;
  // alert puts the toast in the role=alert container, so a screen reader
  // announces it at once; notices stay in the polite region.
  alert: boolean;
  ttlMs: number;
  dismissible: boolean;
}

// toastView decides how a toast renders. An omitted type is a completed action
// (ok). A type outside the contract (a severity from a newer appliance) becomes
// the neutral info, so it cannot render "undefined", pass for a success, or pick
// an Object.prototype member; it is checked with includes, never an object
// lookup.
export function toastView(type?: string, durationMs?: number): ToastView {
  const known = type === undefined ? "ok" : (TOAST_TYPES as readonly string[]).includes(type) ? (type as ToastType) : "info";
  const alert = known === "error";
  const ttlMs = durationMs ?? (alert ? ERROR_TTL_MS : INFO_TTL_MS);
  // Errors, and any toast held as long as one, get a dismiss button.
  return { type: known, alert, ttlMs, dismissible: alert || ttlMs >= ERROR_TTL_MS };
}
