// Shared UI helpers used across views and components. Extracted so the DOM
// builder, the uptime formatter (which had diverged between the dashboard and
// the system view), the load-error/retry pattern, and the per-mode/per-state
// label maps live in exactly one place.
import { ApiError } from "./api.js";

// elem creates an element with an optional class and text content.
export function elem(tag: string, className?: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

// setBusy marks a control in-progress WITHOUT removing it from the tab order:
// aria-disabled (not the disabled property) keeps it focusable, so a keyboard
// user is not dumped to <body> when the focused control goes busy. Because
// aria-disabled does not block activation, the caller must guard re-entry (a
// boolean flag, or checking aria-disabled). aria-busy announces the state and
// the .is-busy class dims the control and shows a progress cursor. This is the
// canonical busy affordance; prefer it over toggling `disabled` on a focused
// control, which steals focus.
export function setBusy(el: HTMLElement, label?: string): void {
  if (label !== undefined) el.textContent = label;
  el.setAttribute("aria-disabled", "true");
  el.setAttribute("aria-busy", "true");
  el.classList.add("is-busy");
}

// clearBusy reverses setBusy, restoring the control's label when one is given.
export function clearBusy(el: HTMLElement, label?: string): void {
  if (label !== undefined) el.textContent = label;
  el.removeAttribute("aria-disabled");
  el.removeAttribute("aria-busy");
  el.classList.remove("is-busy");
}

// apiErrorMessage reduces any thrown value to a short human string: an ApiError
// shows its problem title, any other Error its message, and anything else its
// string form. Shared so the several save/PATCH catch blocks map failures the
// same way instead of re-inlining the ternary.
export function apiErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.title;
  if (err instanceof Error) return err.message;
  return String(err);
}

// setFieldError marks (or clears) a form field's invalid state uniformly: the
// red border/background via .invalid on the wrapper, aria-invalid on the input
// so a screen reader announces it, and the rule text in the error element. An
// empty message clears all three. Shared by the hand-rolled per-field error
// setters across the views.
export function setFieldError(
  field: HTMLElement | null,
  input: HTMLElement | null | undefined,
  errorEl: HTMLElement | null,
  message: string,
): void {
  field?.classList.toggle("invalid", !!message);
  input?.setAttribute("aria-invalid", message ? "true" : "false");
  if (errorEl) errorEl.textContent = message;
}

// setText and setHidden write only when the value actually changes. A card is
// re-synced on every 3 s poll, so an unconditional write would dirty the DOM and,
// for a role=status live region, re-announce an unchanged message to a screen
// reader on every tick. These make the diffed-write convention uniform rather
// than repeating a hand-written guard at each call site.
export function setText(el: HTMLElement, text: string): void {
  if (el.textContent !== text) el.textContent = text;
}
export function setHidden(el: HTMLElement, hidden: boolean): void {
  if (el.hidden !== hidden) el.hidden = hidden;
}

// formatUptime renders a seconds count as a compact human string. Seconds are
// shown only below one hour, and only when the caller opts in: the dashboard top
// ribbon passes { seconds: true } so a freshly (re)started appliance shows its
// uptime advancing, while a long-running appliance settles to minute resolution
// rather than churning a seconds field forever. This matches the pre-existing
// dashboard formatter byte-for-byte. The system info list omits seconds entirely
// because it is a static row that should not churn on every render.
export function formatUptime(totalSeconds: number, opts: { seconds?: boolean } = {}): string {
  const d = Math.floor(totalSeconds / 86400);
  const h = Math.floor((totalSeconds % 86400) / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h ${String(m).padStart(2, "0")}m`;
  if (h > 0) return `${h}h ${String(m).padStart(2, "0")}m`;
  if (opts.seconds) return `${m}m ${String(Math.floor(totalSeconds % 60)).padStart(2, "0")}s`;
  return `${m}m`;
}

// formatRelative renders the age of an event as a compact "N ago" string, given
// two epoch-millisecond instants (the event and now). Callers correct the event
// time for server clock skew before calling, so this stays a pure, browser-free
// formatter. A future-dated instant (small forward skew) clamps to "just now".
export function formatRelative(fromMs: number, toMs: number): string {
  // An unparseable timestamp (Date.parse -> NaN) would otherwise fall through
  // every threshold to "NaN ago"; render nothing instead.
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs)) return "";
  const s = Math.floor(Math.max(0, toMs - fromMs) / 1000);
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// modeLabel maps a stream mode to its display label.
export function modeLabel(mode: string): string {
  return mode === "pcm" ? "PCM L16" : "OPUS";
}

// deviceStateBadge maps a device runtime state to the status-badge class and
// label shown in both the dashboard cards and the system device table.
export function deviceStateBadge(state: string): { cls: string; label: string } {
  if (state === "skipped") return { cls: "status-badge crit", label: "Skipped" };
  if (state === "failed") return { cls: "status-badge crit", label: "Failed" };
  if (state === "disabled") return { cls: "status-badge idle", label: "Disabled" };
  return { cls: "status-badge ok", label: "Serving" };
}

// renderLoadError replaces a container's contents with a failure message and a
// Retry button, and marks the container as an assertive live region (role=alert)
// so the cold load error is announced to screen readers rather than appearing
// silently. loadingText is shown in place while the retry runs; onRetry performs
// the reload. The caller owns which element is the container (an existing
// placeholder for the dashboard, a dedicated paragraph for the system view) so
// this never disturbs the surrounding layout.
export function renderLoadError(
  container: HTMLElement,
  message: string,
  loadingText: string,
  onRetry: () => void,
): void {
  container.hidden = false;
  container.setAttribute("role", "alert");
  container.textContent = `${message} `;
  const retry = elem("button", "btn btn-secondary", "Retry");
  retry.setAttribute("type", "button");
  retry.addEventListener("click", () => {
    // Drop the assertive alert role before showing the benign loading text so
    // the screen reader does not read "Loading..." as an alert. A later failure
    // re-adds it when renderLoadError runs again.
    container.removeAttribute("role");
    container.textContent = loadingText;
    onRetry();
  });
  container.appendChild(retry);
}
