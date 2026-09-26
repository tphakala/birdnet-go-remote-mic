import { api } from "../lib/api.js";
import { showToast } from "./toast.js";
import { confirmDialog, setAppInert, trapFocus } from "../lib/modal.js";
import { announce } from "../lib/ui.js";
import { formatElapsed, InstallWait, STAGE_LABELS } from "../lib/update-core.js";

// restarting guards against a double click starting two restart flows (and thus
// two countdown/health-poll intervals).
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
    const errorMsg = err instanceof Error ? err.message : String(err);
    showToast(`Restart request failed: ${errorMsg}`, "error");
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
      if (timerEl) timerEl.textContent = "Probing /healthz...";
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
    if (timerEl) timerEl.textContent = `Probing /healthz (${attempts}/${maxAttempts})...`;

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

// How often the install wait probes /healthz, and how long one probe may take.
const INSTALL_PROBE_MS = 2000;
const INSTALL_PROBE_TIMEOUT_MS = 4000;

// InstallWaitHandle is how the caller feeds the install wait the phases the
// status poll reports.
export interface InstallWaitHandle {
  // installing reports the installing phase (the release is with the root
  // updater), which starts the install allowance.
  installing(): void;
  // close ends the wait and hides the modal when the attempt failed or was
  // abandoned without a restart. Once the appliance has gone down for the
  // restart it does nothing, since only a probe can end the wait then. It
  // returns whether it closed.
  close(): boolean;
}

// probeVersion asks the open /healthz for the running version, or null when
// the appliance does not answer (restarting). Like the restart probe it is a
// deliberate raw fetch: api.ts would treat a failure as an error to report.
async function probeVersion(): Promise<string | null> {
  try {
    const res = await fetch("/api/v1/healthz", { cache: "no-store", signal: AbortSignal.timeout(INSTALL_PROBE_TIMEOUT_MS) });
    if (!res.ok) return null;
    const body = (await res.json()) as { version?: unknown };
    return typeof body.version === "string" ? body.version : null;
  } catch {
    return null;
  }
}

// awaitUpdateInstall shows the restart modal from the moment an update to
// target starts until the appliance answers again, then reloads the page (see
// InstallWait). It returns null when a restart or another wait already holds
// the modal.
export function awaitUpdateInstall(target: string): InstallWaitHandle | null {
  const modal = document.getElementById("restart-modal");
  const titleEl = document.getElementById("modal-title");
  const textEl = document.getElementById("modal-text");
  const timerEl = document.getElementById("reconnect-timer");
  if (!modal || restarting) return null;
  restarting = true;

  const oldTitle = titleEl?.textContent ?? "";
  const oldText = textEl?.textContent ?? "";
  if (titleEl) titleEl.textContent = "Updating Appliance";
  if (textEl) {
    textEl.textContent = `Updating to ${target}. The appliance downloads and verifies the release, then restarts to install it, and goes back to the running version on its own if the new one does not start. This page reloads once it answers again.`;
  }
  const prevFocus = document.activeElement as HTMLElement | null;
  modal.classList.add("open");
  setAppInert(true);
  const release = trapFocus(modal);
  modal.querySelector<HTMLElement>(".modal-card")?.focus();
  say(`Updating to ${target}. This page reloads when the appliance is back.`);

  const started = Date.now();
  const wait = new InstallWait(target, started);
  let lastStage = wait.stage;
  let done = false;
  let timer = 0;
  // The progress line is visual only (aria-hidden); a stage change is announced.
  const tick = (): void => {
    if (timerEl) timerEl.textContent = `${STAGE_LABELS[wait.stage]} (${formatElapsed(Date.now() - started)})`;
    if (wait.stage !== lastStage) {
      lastStage = wait.stage;
      say(`${STAGE_LABELS[lastStage]}.`);
    }
  };
  tick();
  const finish = (): void => {
    done = true;
    window.clearTimeout(timer);
  };
  const step = async (): Promise<void> => {
    const version = await probeVersion();
    if (done) return;
    const next = wait.probe(version, Date.now());
    tick();
    if (next === "reload") {
      finish();
      if (timerEl) timerEl.textContent = "Appliance online. Reloading...";
      sayNow("The appliance is back. Reloading.");
      window.setTimeout(() => window.location.reload(), 600);
      return;
    }
    if (next === "timeout") {
      finish();
      if (timerEl) timerEl.textContent = "The appliance has not come back yet.";
      say("The appliance has not come back yet. Use the reload button to try again.");
      showRetry();
      return;
    }
    timer = window.setTimeout(() => void step(), INSTALL_PROBE_MS);
  };
  timer = window.setTimeout(() => void step(), INSTALL_PROBE_MS);

  return {
    installing(): void {
      if (done) return;
      wait.installing(Date.now());
      tick();
    },
    close(): boolean {
      if (done || wait.wentDown) return false;
      finish();
      modal.classList.remove("open");
      release();
      setAppInert(false);
      if (titleEl) titleEl.textContent = oldTitle;
      if (textEl) textEl.textContent = oldText;
      restarting = false;
      prevFocus?.focus({ preventScroll: true });
      return true;
    },
  };
}
