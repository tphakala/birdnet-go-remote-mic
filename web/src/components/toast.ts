import { h } from "../lib/h.ts";
import { svgIcon } from "../lib/svg.ts";
import { ERROR_TTL_MS, toastView, type ToastType } from "../lib/toast-core.ts";

export type { ToastType };
export { ERROR_TTL_MS };

// Trusted static icon markup (never interpolated with runtime data).
const ICON_WARN = svgIcon('<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line>', 15);
const ICON_ERROR = svgIcon('<circle cx="12" cy="12" r="10"></circle><line x1="15" y1="9" x2="9" y2="15"></line><line x1="9" y1="9" x2="15" y2="15"></line>', 15);
const ICON_OK = svgIcon('<path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"></path><polyline points="22 4 12 14.01 9 11.01"></polyline>', 15);
const ICON_INFO = svgIcon('<circle cx="12" cy="12" r="10"></circle><line x1="12" y1="16" x2="12" y2="12"></line><line x1="12" y1="8" x2="12.01" y2="8"></line>', 15);
// Exported so the notification center reuses the same close glyph on its panel.
export const ICON_CLOSE = svgIcon('<line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line>', 13);

// The leading icon per toast type. A lookup keyed by type replaces a nested
// ternary so a new type is one entry, not another branch. Exported so the
// notification center renders the same severity glyphs as the toasts. ok is a
// completed action (the green check); info is neutral (an i).
export const TOAST_ICONS: Record<ToastType, string> = {
  ok: ICON_OK,
  info: ICON_INFO,
  warn: ICON_WARN,
  error: ICON_ERROR,
};

export function showToast(message: string, type?: ToastType, durationMs?: number): void {
  const v = toastView(type, durationMs);
  const container = document.getElementById(v.alert ? "toast-alerts" : "toast-root");
  if (!container) return;

  const icon = h("span", { class: "toast-icon sev-badge" });
  icon.innerHTML = TOAST_ICONS[v.type]; // static, trusted markup
  // sev-* drives the shared severity surface (badge fill, border and wash).
  const toast = h("div", { class: `toast sev-${v.type}` }, icon, h("span", { class: "toast-msg" }, message));

  let timer = 0;
  let removed = false;
  const remove = (): void => {
    if (removed) return;
    removed = true;
    window.clearTimeout(timer);
    toast.style.animation = "toast-out 0.25s cubic-bezier(0.16, 1, 0.3, 1) forwards";
    window.setTimeout(() => toast.remove(), 250);
  };
  // Pause the auto-dismiss while the pointer is over the toast OR focus is inside
  // it, so a keyboard user tabbing to the dismiss button (or anyone reading a
  // hovered toast) is not raced by the timer. The two holds are tracked
  // independently and the timer only restarts once BOTH are released: dropping one
  // (the mouse leaves) while the other still holds (focus is inside) must not
  // re-arm the dismiss under the user.
  let hovered = false;
  let focused = false;
  const disarm = (): void => window.clearTimeout(timer);
  const rearm = (): void => {
    if (!removed && !hovered && !focused) timer = window.setTimeout(remove, v.ttlMs);
  };
  toast.addEventListener("mouseenter", () => { hovered = true; disarm(); });
  toast.addEventListener("mouseleave", () => { hovered = false; rearm(); });
  toast.addEventListener("focusin", () => { focused = true; disarm(); });
  toast.addEventListener("focusout", () => { focused = false; rearm(); });

  // The dismiss button lets anyone close the toast by hand, and a keyboard user
  // can focus it to hold the toast open.
  if (v.dismissible) {
    const dismiss = h("button", { type: "button", class: "toast-dismiss", "aria-label": "Dismiss notification" });
    dismiss.innerHTML = ICON_CLOSE; // static, trusted markup
    dismiss.addEventListener("click", remove);
    toast.appendChild(dismiss);
  }

  container.appendChild(toast);
  rearm();
}
