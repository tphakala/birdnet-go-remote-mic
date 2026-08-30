import { api } from "../lib/api.js";
import { showToast } from "./toast.js";
export async function triggerApplianceRestart() {
    const modal = document.getElementById("restart-modal");
    const timerEl = document.getElementById("reconnect-timer");
    if (!modal)
        return;
    try {
        await api.postSystemRestart();
    }
    catch (err) {
        const errorMsg = err instanceof Error ? err.message : String(err);
        showToast(`Restart request failed: ${errorMsg}`, "error");
        return;
    }
    modal.classList.add("open");
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
                timerEl.textContent = "Restart timeout. Click anywhere to retry reload.";
            const modal = document.getElementById("restart-modal");
            if (modal) {
                modal.addEventListener("click", () => window.location.reload(), { once: true });
            }
        }
    }, 1000);
}
