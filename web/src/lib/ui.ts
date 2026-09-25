// Shared UI helpers used across views and components. Extracted so the DOM
// builder, the uptime formatter (which had diverged between the dashboard and
// the system view), the load-error/retry pattern, and the per-mode/per-state
// label maps live in exactly one place.
import { ApiError } from "./api.js";
import { showToast } from "../components/toast.js";

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
// ICON_COPY is the shared copy glyph for every copy-to-clipboard control, so the
// affordance reads the same on the dashboard, the system view, and the device
// settings form. It lived in dashboard.ts, where only the dashboard's own copy
// buttons could reach it; the other copy buttons shipped without an icon as a
// result. Keep new copy controls pointed here.
export const ICON_COPY =
  '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"></rect><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"></path></svg>';

// iconSpan wraps trusted, static icon markup in a decorative (aria-hidden) span.
// The control's own text or aria-label carries the meaning, so the graphic is
// hidden from assistive tech rather than announced unlabeled.
export function iconSpan(markup: string, className?: string): HTMLElement {
  const s = document.createElement("span");
  if (className) s.className = className;
  s.setAttribute("aria-hidden", "true");
  // Static trusted markup only; never runtime/user data.
  s.innerHTML = markup;
  return s;
}

// button builds a .btn control: variant fill, an optional leading icon, a label,
// and the attributes every button repeats (type=button, aria-label, id, click).
// It replaces the elem("button", "btn btn-…") + setAttribute("type","button")
// pattern that was copied at each call site, so a button cannot drift from the
// shared shape. It covers the button-shaped variants only; the inline text link
// (.btn-link) and the chip controls (.copy-btn, .icon-btn, .clip-latch-btn) keep
// their own construction.
export interface ButtonOptions {
  variant?: "primary" | "secondary" | "danger";
  label?: string;
  icon?: string;
  ariaLabel?: string;
  id?: string;
  title?: string;
  extraClass?: string;
  type?: "button" | "submit" | "reset";
  onClick?: (ev: MouseEvent) => void;
}

export function button(opts: ButtonOptions = {}): HTMLButtonElement {
  const b = document.createElement("button");
  b.type = opts.type ?? "button";
  const classes = ["btn", `btn-${opts.variant ?? "secondary"}`];
  if (opts.extraClass) classes.push(opts.extraClass);
  b.className = classes.join(" ");
  if (opts.icon) b.appendChild(iconSpan(opts.icon, "btn-icon"));
  // Label lives in its own span so setButtonLabel/setBusy can rewrite the text
  // without wiping a leading icon.
  if (opts.label !== undefined) b.appendChild(elem("span", "btn-label", opts.label));
  if (opts.ariaLabel) b.setAttribute("aria-label", opts.ariaLabel);
  if (opts.id) b.id = opts.id;
  if (opts.title) b.title = opts.title;
  if (opts.onClick) b.addEventListener("click", opts.onClick);
  return b;
}

// switchControl builds the shared on/off switch: a <label class="switch-control">
// wrapping a visually hidden checkbox with role="switch" (announced as "switch,
// on/off" rather than "checkbox, checked") followed by the drawn track. The
// input must stay directly before the track for the `input + .switch-track`
// state selectors. label is a visible caption after the track that also names
// the input; caption is a short visible caption placed before the input and
// hidden from assistive tech, for a switch whose accessible name comes from an
// aria-label instead. The caller wires any change listener on the returned
// input.
export interface SwitchOptions {
  label?: string;
  caption?: string;
  extraClass?: string;
  id?: string;
  checked?: boolean;
  describedBy?: string;
  title?: string;
}

export function switchControl(opts: SwitchOptions = {}): { el: HTMLLabelElement; input: HTMLInputElement } {
  const el = document.createElement("label");
  el.className = opts.extraClass ? `switch-control ${opts.extraClass}` : "switch-control";
  if (opts.title) el.title = opts.title;
  const input = document.createElement("input");
  input.type = "checkbox";
  input.className = "visually-hidden";
  input.setAttribute("role", "switch");
  if (opts.id) input.id = opts.id;
  input.checked = opts.checked ?? false;
  if (opts.describedBy) input.setAttribute("aria-describedby", opts.describedBy);
  if (opts.caption !== undefined) {
    const caption = elem("span", "switch-caption", opts.caption);
    caption.setAttribute("aria-hidden", "true");
    el.append(caption);
  }
  const track = elem("span", "switch-track");
  track.append(elem("span", "switch-thumb"));
  el.append(input, track);
  if (opts.label !== undefined) el.append(elem("span", "switch-label", opts.label));
  return { el, input };
}

// setButtonLabel updates a button's visible text without disturbing a leading
// icon: it writes into the .btn-label span the factory adds, and falls back to
// the element's own text for a plain button that has no such span.
export function setButtonLabel(el: HTMLElement, text: string): void {
  let label = el.querySelector<HTMLElement>(".btn-label");
  // An icon-only button has a .btn-icon but no label span; add one rather than
  // wiping the icon via textContent.
  if (!label && el.querySelector(".btn-icon")) {
    label = elem("span", "btn-label");
    el.appendChild(label);
  }
  if (label) label.textContent = text;
  else el.textContent = text;
}

export function setBusy(el: HTMLElement, label?: string): void {
  if (label !== undefined) setButtonLabel(el, label);
  el.setAttribute("aria-disabled", "true");
  el.setAttribute("aria-busy", "true");
  el.classList.add("is-busy");
}

// clearBusy reverses setBusy, restoring the control's label when one is given.
export function clearBusy(el: HTMLElement, label?: string): void {
  if (label !== undefined) setButtonLabel(el, label);
  el.removeAttribute("aria-disabled");
  el.removeAttribute("aria-busy");
  el.classList.remove("is-busy");
}

// Per-device meter display preference: "hide inactive channels". It is a per-
// viewer view option, not appliance config, so it lives in localStorage keyed by
// the stable device id rather than in the saved config. read/write are wrapped
// because localStorage can throw (private mode, disabled storage); a failure
// falls back to the default and simply does not persist.
export function hideInactiveKey(deviceId: string): string {
  return `remote-mic-hide-inactive:${deviceId}`;
}
export function readBoolPref(key: string, fallback: boolean): boolean {
  try {
    const v = localStorage.getItem(key);
    return v === null ? fallback : v === "1";
  } catch {
    return fallback;
  }
}
export function writeBoolPref(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? "1" : "0");
  } catch {
    /* storage unavailable: the preference just does not persist */
  }
}

// How long a download's object URL outlives the click. Some browsers start
// the download asynchronously and cancel it when the URL is revoked on the next
// tick; half a minute is ample, and holding the blob that long is harmless.
const DOWNLOAD_REVOKE_MS = 30_000;

// downloadBlob saves blob under filename through a temporary object URL and a
// synthetic link click, then revokes the URL once the download has had time to
// start.
export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), DOWNLOAD_REVOKE_MS);
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

// CLIPBOARD_UNAVAILABLE_MSG is shown when the Clipboard API is absent (an
// insecure plain-http origin, or an embedded browser). Defined once so every
// copy path reports it identically.
export const CLIPBOARD_UNAVAILABLE_MSG =
  "Copy is unavailable in this browser; it may need a secure (https) connection.";

// writeToClipboard is the shared clipboard primitive: it never throws, reporting
// the outcome instead. "unavailable" means the Clipboard API is absent; "failed"
// means the write was rejected; "ok" means it succeeded. The availability check
// and the rejection catch live here so a caller cannot silently no-op (the old
// Copy URL bug) or leak an unhandled rejection. Callers add their own feedback (a
// toast, a button pulse) on the result.
export async function writeToClipboard(value: string): Promise<"ok" | "unavailable" | "failed"> {
  // The Clipboard API is missing outright, or present but unusable outside a
  // secure context (plain http on a non-localhost origin, and some embedded
  // browsers). Treat both as "unavailable" so the caller shows the actionable
  // message instead of a bare "Copy failed".
  if (!navigator.clipboard || window.isSecureContext === false) return "unavailable";
  try {
    await navigator.clipboard.writeText(value);
    return "ok";
  } catch {
    return "failed";
  }
}

// reportClipboardFailure shows the standard toast for a non-ok writeToClipboard
// result, so the unavailable and failed cases read the same at every Copy button.
export function reportClipboardFailure(result: "unavailable" | "failed"): void {
  if (result === "unavailable") showToast(CLIPBOARD_UNAVAILABLE_MSG, "warn");
  else showToast("Copy failed", "error");
}

// copyText writes value to the clipboard and reports the outcome as a toast: the
// success message on success, and the shared unavailable/failed toast otherwise.
// Used by the plain Copy buttons; the dashboard's copy buttons call
// writeToClipboard directly so they can add their "copied" pulse.
export function copyText(value: string, successMessage: string): void {
  void writeToClipboard(value).then((result) => {
    if (result === "ok") showToast(successMessage);
    else reportClipboardFailure(result);
  });
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
// two epoch-millisecond instants (the event and now). Callers map the event onto
// the browser clock before calling, so this stays a pure, browser-free
// formatter. A future-dated instant (small forward skew) clamps to "just now".
export function formatRelative(fromMs: number, toMs: number): string {
  // An unknown instant (NaN) would otherwise fall through every threshold to
  // "NaN ago"; render nothing instead.
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
  const retry = button({ variant: "secondary", label: "Retry" });
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
