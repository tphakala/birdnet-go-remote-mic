import { api } from "../lib/api.js";
import { showToast } from "./toast.js";

// restarting guards against a double click starting two restart flows (and thus
// two countdown/health-poll intervals).
let restarting = false;

const FOCUSABLE = 'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// trapFocus keeps Tab and Shift+Tab cycling within container until released.
function trapFocus(container: HTMLElement): () => void {
  const handler = (e: KeyboardEvent) => {
    if (e.key !== "Tab") return;
    const items = Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
      (el) => !el.hidden && el.offsetParent !== null
    );
    if (items.length === 0) {
      e.preventDefault();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement as HTMLElement | null;
    if (!active || items.indexOf(active) === -1) {
      // Focus is on the dialog container (or has otherwise escaped the focusable
      // set): pull it back to the appropriate end rather than letting Tab leave.
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
    } else if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  };
  container.addEventListener("keydown", handler);
  return () => container.removeEventListener("keydown", handler);
}

// confirmRestart shows a modal asking to confirm the disruptive restart. It
// defaults to Cancel (focused), closes on Escape or a backdrop click, traps
// focus, and restores focus to the trigger. Resolves true only if confirmed.
function confirmRestart(): Promise<boolean> {
  return new Promise((resolve) => {
    const prevFocus = document.activeElement as HTMLElement | null;

    const overlay = document.createElement("div");
    overlay.className = "modal-overlay open";
    overlay.setAttribute("role", "dialog");
    overlay.setAttribute("aria-modal", "true");
    overlay.setAttribute("aria-labelledby", "confirm-restart-title");
    overlay.setAttribute("aria-describedby", "confirm-restart-desc");

    const card = document.createElement("div");
    card.className = "modal-card";

    const title = document.createElement("h3");
    title.id = "confirm-restart-title";
    title.style.cssText = "font-size:16px;font-weight:700;color:var(--text-primary);margin-bottom:6px;";
    title.textContent = "Restart appliance?";

    const body = document.createElement("p");
    body.id = "confirm-restart-desc";
    body.style.cssText = "font-size:12px;color:var(--text-secondary);line-height:1.5;";
    body.textContent = "This closes every active RTSP client session while the service restarts.";

    const actions = document.createElement("div");
    actions.className = "settings-actions";

    const cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "btn btn-secondary";
    cancel.textContent = "Cancel";

    const confirm = document.createElement("button");
    confirm.type = "button";
    confirm.className = "btn btn-danger";
    confirm.textContent = "Restart";

    actions.append(cancel, confirm);
    card.append(title, body, actions);
    overlay.appendChild(card);

    const release = trapFocus(overlay);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close(false);
    };

    function close(result: boolean): void {
      release();
      document.removeEventListener("keydown", onKey);
      overlay.remove();
      prevFocus?.focus();
      resolve(result);
    }

    cancel.addEventListener("click", () => close(false));
    confirm.addEventListener("click", () => close(true));
    overlay.addEventListener("click", (e) => {
      if (e.target === overlay) close(false);
    });
    document.addEventListener("keydown", onKey);

    document.body.appendChild(overlay);
    cancel.focus();
  });
}

export async function triggerApplianceRestart(): Promise<void> {
  if (restarting) return;
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
  } catch (err: unknown) {
    const errorMsg = err instanceof Error ? err.message : String(err);
    showToast(`Restart request failed: ${errorMsg}`, "error");
    restarting = false;
    return;
  }

  modal.classList.add("open");
  trapFocus(modal);
  modal.querySelector<HTMLElement>(".modal-card")?.focus();

  let seconds = 5;
  if (timerEl) timerEl.textContent = `Reconnecting in ${seconds}s...`;

  const countdown = window.setInterval(() => {
    seconds -= 1;
    if (seconds > 0) {
      if (timerEl) timerEl.textContent = `Reconnecting in ${seconds}s...`;
    } else {
      clearInterval(countdown);
      if (timerEl) timerEl.textContent = "Probing /healthz...";
      startHealthPolling();
    }
  }, 1000);
}

function startHealthPolling(): void {
  const timerEl = document.getElementById("reconnect-timer");
  let attempts = 0;
  const maxAttempts = 30;

  const interval = window.setInterval(async () => {
    attempts += 1;
    if (timerEl) timerEl.textContent = `Probing /healthz (${attempts}/${maxAttempts})...`;

    try {
      const res = await fetch("/api/v1/healthz", { cache: "no-store" });
      if (res.ok) {
        clearInterval(interval);
        if (timerEl) timerEl.textContent = "Appliance online! Reloading...";
        window.setTimeout(() => {
          window.location.reload();
        }, 600);
      }
    } catch {
      // Still rebooting / down
    }

    if (attempts >= maxAttempts) {
      clearInterval(interval);
      if (timerEl) timerEl.textContent = "Restart timed out.";
      showRetry();
    }
  }, 1000);
}

// showRetry reveals the real Retry button (replacing the old "click anywhere"
// affordance) and focuses it so a keyboard user can reload.
function showRetry(): void {
  const retry = document.getElementById("restart-retry") as HTMLButtonElement | null;
  if (!retry) return;
  retry.hidden = false;
  retry.addEventListener("click", () => window.location.reload(), { once: true });
  retry.focus();
}
