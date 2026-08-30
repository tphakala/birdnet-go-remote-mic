import { api } from "../lib/api.js";
import { showToast } from "./toast.js";
import { confirmDialog, setAppInert, trapFocus } from "../lib/modal.js";
// restarting guards against a double click starting two restart flows (and thus
// two countdown/health-poll intervals).
let restarting = false;
// announce writes to the polite live region that carries only phase changes, so
// a screen reader is not spammed by the per-second visual countdown.
function announce(text) {
    const el = document.getElementById("restart-announce");
    if (el)
        el.textContent = text;
}
// confirmRestart asks the user to confirm the disruptive restart before it runs.
function confirmRestart() {
    return confirmDialog({
        title: "Restart appliance?",
        body: "This closes every active RTSP client session while the service restarts.",
        confirmLabel: "Restart",
        danger: true,
    });
}
export async function triggerApplianceRestart() {
    if (restarting)
        return;
    // Arm the guard before the confirm dialog so a second trigger while the
    // confirm is open cannot stack a second dialog or a second restart flow.
    restarting = true;
    if (!(await confirmRestart())) {
        restarting = false;
        return;
    }
    const modal = document.getElementById("restart-modal");
    const timerEl = document.getElementById("reconnect-timer");
    if (!modal) {
        restarting = false;
        return;
    }
    try {
        await api.postSystemRestart();
    }
    catch (err) {
        const errorMsg = err instanceof Error ? err.message : String(err);
        showToast(`Restart request failed: ${errorMsg}`, "error");
        restarting = false;
        return;
    }
    modal.classList.add("open");
    setAppInert(true);
    trapFocus(modal);
    modal.querySelector(".modal-card")?.focus();
    // Announce the phase once; the per-second countdown below updates only the
    // aria-hidden visual element, so it is not read out on every tick.
    announce("Restarting the appliance. Reconnecting shortly.");
    let seconds = 5;
    if (timerEl)
        timerEl.textContent = `Reconnecting in ${seconds}s...`;
    const countdown = window.setInterval(() => {
        seconds -= 1;
        if (seconds > 0) {
            if (timerEl)
                timerEl.textContent = `Reconnecting in ${seconds}s...`;
        }
        else {
            clearInterval(countdown);
            if (timerEl)
                timerEl.textContent = "Probing /healthz...";
            announce("Checking whether the appliance is back online.");
            startHealthPolling();
        }
    }, 1000);
}
function startHealthPolling() {
    const timerEl = document.getElementById("reconnect-timer");
    let attempts = 0;
    const maxAttempts = 30;
    const interval = window.setInterval(async () => {
        attempts += 1;
        if (timerEl)
            timerEl.textContent = `Probing /healthz (${attempts}/${maxAttempts})...`;
        try {
            const res = await fetch("/api/v1/healthz", { cache: "no-store" });
            if (res.ok) {
                clearInterval(interval);
                if (timerEl)
                    timerEl.textContent = "Appliance online! Reloading...";
                announce("Appliance is back online. Reloading.");
                window.setTimeout(() => {
                    window.location.reload();
                }, 600);
            }
        }
        catch {
            // Still rebooting / down
        }
        if (attempts >= maxAttempts) {
            clearInterval(interval);
            if (timerEl)
                timerEl.textContent = "Restart timed out.";
            announce("Restart timed out. Use the reload button to try again.");
            showRetry();
        }
    }, 1000);
}
// showRetry reveals the real Retry button (replacing the old "click anywhere"
// affordance) and focuses it so a keyboard user can reload.
function showRetry() {
    const retry = document.getElementById("restart-retry");
    if (!retry)
        return;
    retry.hidden = false;
    retry.addEventListener("click", () => window.location.reload(), { once: true });
    retry.focus();
}
