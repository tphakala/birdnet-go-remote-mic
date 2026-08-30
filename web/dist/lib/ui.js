// Shared UI helpers used across views and components. Extracted so the DOM
// builder, the uptime formatter (which had diverged between the dashboard and
// the system view), the load-error/retry pattern, and the per-mode/per-state
// label maps live in exactly one place.
// elem creates an element with an optional class and text content.
export function elem(tag, className, text) {
    const e = document.createElement(tag);
    if (className)
        e.className = className;
    if (text !== undefined)
        e.textContent = text;
    return e;
}
// formatUptime renders a seconds count as a compact human string. The dashboard
// top ribbon passes { seconds: true } so the live counter visibly ticks (a cue
// the appliance is alive); the system info list omits seconds because it is a
// static row that should not churn on every render.
export function formatUptime(totalSeconds, opts = {}) {
    const d = Math.floor(totalSeconds / 86400);
    const h = Math.floor((totalSeconds % 86400) / 3600);
    const m = Math.floor((totalSeconds % 3600) / 60);
    if (d > 0)
        return `${d}d ${h}h ${String(m).padStart(2, "0")}m`;
    if (h > 0)
        return `${h}h ${String(m).padStart(2, "0")}m`;
    if (opts.seconds)
        return `${m}m ${String(Math.floor(totalSeconds % 60)).padStart(2, "0")}s`;
    return `${m}m`;
}
// modeLabel maps a stream mode to its display label.
export function modeLabel(mode) {
    return mode === "pcm" ? "PCM L16" : "OPUS";
}
// deviceStateBadge maps a device runtime state to the status-badge class and
// label shown in both the dashboard cards and the system device table.
export function deviceStateBadge(state) {
    if (state === "skipped")
        return { cls: "status-badge crit", label: "Skipped" };
    if (state === "failed")
        return { cls: "status-badge crit", label: "Failed" };
    return { cls: "status-badge ok", label: "Serving" };
}
// renderLoadError replaces a container's contents with a failure message and a
// Retry button, and marks the container as an assertive live region (role=alert)
// so the cold load error is announced to screen readers rather than appearing
// silently. loadingText is shown in place while the retry runs; onRetry performs
// the reload. The caller owns which element is the container (an existing
// placeholder for the dashboard, a dedicated paragraph for the system view) so
// this never disturbs the surrounding layout.
export function renderLoadError(container, message, loadingText, onRetry) {
    container.hidden = false;
    container.setAttribute("role", "alert");
    container.textContent = `${message} `;
    const retry = elem("button", "btn btn-secondary", "Retry");
    retry.setAttribute("type", "button");
    retry.addEventListener("click", () => {
        container.textContent = loadingText;
        onRetry();
    });
    container.appendChild(retry);
}
