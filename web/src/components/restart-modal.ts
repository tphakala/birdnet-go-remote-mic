import { api, ApiError } from "../lib/api.ts";
import { showToast } from "./toast.ts";
import { closeTransientDialogs, confirmDialog, setAppInert, trapFocus } from "../lib/modal.ts";
import { announce, apiErrorMessage } from "../lib/ui.ts";
import { formatElapsed } from "../lib/update-core.ts";

// restarting guards against a double click starting two restart flows (and thus
// two countdown/health-poll intervals), and against the update's install modal
// taking the modal while a restart holds it (or the reverse).
let restarting = false;

// say writes to the polite live region that carries only phase changes, so a
// screen reader is not spammed by the per-second visual countdown. The shared
// announce clears the region first, so a repeated phase is read again.
function say(text: string): void {
  announce(document.getElementById("restart-announce"), text);
}

// sayNow writes the last phase before a reload at once. announce waits for the
// next animation frame, which a background tab does not run before the reload.
function sayNow(text: string): void {
  const region = document.getElementById("restart-announce");
  if (region) region.textContent = text;
}

// confirmRestart asks the user to confirm the disruptive restart before it runs.
function confirmRestart(): Promise<boolean> {
  return confirmDialog({
    title: "Restart appliance?",
    body: "This closes every active RTSP client session while the service restarts.",
    confirmLabel: "Restart",
    danger: true,
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
    // 501: the server has no restart control wired
    // (internal/mgmtserver/system.go:86), so say what to do instead.
    const why = err instanceof ApiError && err.status === 501
      ? "this appliance cannot restart itself. Restart the remote-mic service on its host instead."
      : apiErrorMessage(err);
    showToast(`Restart request failed: ${why}`, "error");
    restarting = false;
    return;
  }

  modal.classList.add("open");
  setAppInert(true);
  trapFocus(modal);
  modal.querySelector<HTMLElement>(".modal-card")?.focus();

  // Announce the phase once; the per-second countdown below updates only the
  // aria-hidden visual element, so it is not read out on every tick.
  say("Restarting the appliance. Reconnecting shortly.");

  let seconds = 5;
  if (timerEl) timerEl.textContent = `Reconnecting in ${seconds}s...`;

  const countdown = window.setInterval(() => {
    seconds -= 1;
    if (seconds > 0) {
      if (timerEl) timerEl.textContent = `Reconnecting in ${seconds}s...`;
    } else {
      clearInterval(countdown);
      if (timerEl) timerEl.textContent = "Waiting for the appliance to come back...";
      say("Checking whether the appliance is back online.");
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
    if (timerEl) timerEl.textContent = `Waiting for the appliance to come back (${attempts}/${maxAttempts})...`;

    try {
      const res = await fetch("/api/v1/healthz", { cache: "no-store" });
      if (res.ok) {
        clearInterval(interval);
        if (timerEl) timerEl.textContent = "Appliance online! Reloading...";
        sayNow("Appliance is back online. Reloading.");
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
      say("Restart timed out. Use the reload button to try again.");
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

// UpdateModal is the install modal for an update this tab started. It shows
// once the root updater has the release, or when the new version answers
// first; the view drives it from the store's post-request statuses (see
// UpdateFollow and VersionWatch).
export interface UpdateModal {
  // elapsed refreshes the visual timer line.
  elapsed(ms: number): void;
  // overdue says the install has not finished in time and offers Reload.
  overdue(): void;
  // reloading says the new version answers, just before the page reloads.
  reloading(): void;
  // hide closes the modal when the attempt ended without a restart. It puts
  // the modal back as the restart flow expects it; the caller places focus.
  hide(): void;
}

// showUpdateModal shows the restart modal for installing target, or returns
// null when a restart already holds it. Transient dialogs (a confirm) close
// first, as the login prompt does, so the modal is never stacked on one.
export function showUpdateModal(target: string): UpdateModal | null {
  const modal = document.getElementById("restart-modal");
  const titleEl = document.getElementById("modal-title");
  const textEl = document.getElementById("modal-text");
  const timerEl = document.getElementById("reconnect-timer");
  if (!modal || restarting) return null;
  restarting = true;
  closeTransientDialogs();

  const oldTitle = titleEl?.textContent ?? "";
  // The restart text holds markup (a code span), so keep its nodes, not its text.
  const oldText = textEl ? Array.from(textEl.childNodes) : [];
  if (titleEl) titleEl.textContent = "Installing Update";
  if (textEl) {
    textEl.textContent = `Installing ${target}. The appliance restarts to finish, which drops connected streams for a moment, and goes back to the running version on its own if the new one does not start. This page reloads once the new version is up.`;
  }
  modal.classList.add("open");
  setAppInert(true);
  const release = trapFocus(modal);
  modal.querySelector<HTMLElement>(".modal-card")?.focus();
  say(`Installing ${target}. This page reloads when the new version is up.`);
  if (timerEl) timerEl.textContent = `Installing (${formatElapsed(0)})`;

  let settled = false;
  let hidden = false;
  return {
    elapsed(ms: number): void {
      if (!settled && timerEl) timerEl.textContent = `Installing (${formatElapsed(ms)})`;
    },
    overdue(): void {
      settled = true;
      if (timerEl) timerEl.textContent = "The update has not finished yet.";
      say("The update has not finished yet. Use the reload button to see where it stands.");
      showRetry();
    },
    reloading(): void {
      settled = true;
      if (timerEl) timerEl.textContent = "New version online. Reloading...";
      sayNow("The new version answers. Reloading.");
    },
    hide(): void {
      if (hidden) return;
      hidden = true;
      settled = true;
      modal.classList.remove("open");
      release();
      setAppInert(false);
      if (titleEl) titleEl.textContent = oldTitle;
      textEl?.replaceChildren(...oldText);
      // An overdue wait revealed Reload; a later restart must not open with it.
      const retry = document.getElementById("restart-retry");
      if (retry) retry.hidden = true;
      restarting = false;
      // announce writes on the next frame; a clear queued after it runs after
      // it in that frame, so a hidden modal leaves nothing to be read out.
      const region = document.getElementById("restart-announce");
      requestAnimationFrame(() => {
        if (region) region.textContent = "";
      });
    },
  };
}
