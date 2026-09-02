// Trusted static icon markup (never interpolated with runtime data).
const ICON_WARN = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>`;
const ICON_ERROR = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="15" y1="9" x2="9" y2="15"></line><line x1="9" y1="9" x2="15" y2="15"></line></svg>`;
const ICON_OK = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"></path><polyline points="22 4 12 14.01 9 11.01"></polyline></svg>`;
const ICON_CLOSE = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>`;
// The leading icon per toast type. A lookup keyed by type replaces a nested
// ternary so a new type is one entry, not another branch.
const TOAST_ICONS = {
    info: ICON_OK,
    warn: ICON_WARN,
    error: ICON_ERROR,
};
const INFO_TTL_MS = 3200;
// A failed provisioning, removal or save deserves more than a glance: errors
// live in an assertive region, stay longer, and can be dismissed by hand.
const ERROR_TTL_MS = 8000;
export function showToast(message, type = "info", durationMs) {
    const isError = type === "error";
    // Errors go to the role=alert container so a screen reader announces them
    // at once; notices stay in the polite region.
    const container = document.getElementById(isError ? "toast-alerts" : "toast-root");
    if (!container)
        return;
    const ttl = durationMs ?? (isError ? ERROR_TTL_MS : INFO_TTL_MS);
    const toast = document.createElement("div");
    toast.className = `toast toast-${type}`;
    const icon = document.createElement("span");
    icon.className = "toast-icon";
    // Fall back to ICON_OK so an off-contract type cannot render "undefined"
    // (the replaced ternary had a default; the lookup must keep one).
    icon.innerHTML = TOAST_ICONS[type] ?? ICON_OK; // static, trusted markup
    const msg = document.createElement("span");
    msg.className = "toast-msg";
    msg.textContent = message;
    toast.append(icon, msg);
    let timer = 0;
    const remove = () => {
        window.clearTimeout(timer);
        toast.style.animation = "toast-out 0.25s cubic-bezier(0.16, 1, 0.3, 1) forwards";
        window.setTimeout(() => toast.remove(), 250);
    };
    if (isError) {
        const dismiss = document.createElement("button");
        dismiss.type = "button";
        dismiss.className = "toast-dismiss";
        dismiss.setAttribute("aria-label", "Dismiss notification");
        dismiss.innerHTML = ICON_CLOSE; // static, trusted markup
        dismiss.addEventListener("click", remove);
        toast.appendChild(dismiss);
    }
    container.appendChild(toast);
    timer = window.setTimeout(remove, ttl);
}
