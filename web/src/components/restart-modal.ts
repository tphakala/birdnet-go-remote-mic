import { api, ApiError, apiErrorMessage } from "../lib/api.ts";
import { withDeadline } from "../lib/deadline.ts";
import { showToast } from "./toast.ts";
import { closeTransientDialogs, confirmDialog, setAppInert, trapFocus } from "../lib/modal.ts";
import { announce } from "../lib/ui.ts";
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

// UNCONFIRMED_TEXT is the dialog's text when the appliance did not confirm
// the restart request (no answer, or an answer that could not be read).
const UNCONFIRMED_TEXT = "The appliance did not confirm the restart. This page waits to see whether it restarts.";
// HEALTH_ATTEMPTS and HEALTH_PROBE_MS bound the wait for the appliance.
const HEALTH_ATTEMPTS = 30;
// A probe gets 4 s: the first one after a restart may need a fresh TLS
// handshake with a busy appliance (its duration NOT MEASURED).
const HEALTH_PROBE_MS = 4_000;

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

  let confirmed = true;
  try {
    await api.postSystemRestart();
  } catch (err: unknown) {
    // An ApiError is a refusal. Anything else (no answer, or an accepted
    // request whose body could not be read) leaves the outcome unknown.
    if (err instanceof ApiError) {
      // 501: the server has no restart control wired
      // (internal/mgmtserver/system.go:86), so say what to do instead.
      const why = err.status === 501
        ? "this appliance cannot restart itself. Restart the remote-mic service on its host instead."
        : apiErrorMessage(err);
      showToast(`Restart request failed: ${why}`, "error");
      restarting = false;
      return;
    }
    // The restart may have started, so wait for the appliance as after a
    // confirmed one rather than invite a second restart.
    confirmed = false;
  }

  if (!confirmed) {
    const titleEl = document.getElementById("modal-title");
    const textEl = document.getElementById("modal-text");
    if (titleEl) titleEl.textContent = "Restart Not Confirmed";
    if (textEl) textEl.textContent = UNCONFIRMED_TEXT;
  }

  modal.classList.add("open");
  setAppInert(true);
  trapFocus(modal);
  modal.querySelector<HTMLElement>(".modal-card")?.focus();

  // The per-second countdown below updates only the aria-hidden visual
  // element, so it is not read out on every tick. The dialog's title and
  // description are read as focus enters it, so the phase line adds only
  // what they lack: when it reconnects (the unconfirmed description already
  // says it waits).
  if (confirmed) say("Reconnecting in 5 seconds.");

  let seconds = 5;
  const countdownText = (s: number) => (confirmed ? `Reconnecting in ${s}s...` : `Checking in ${s}s...`);
  if (timerEl) timerEl.textContent = countdownText(seconds);

  const countdown = window.setInterval(() => {
    seconds -= 1;
    if (seconds > 0) {
      if (timerEl) timerEl.textContent = countdownText(seconds);
    } else {
      clearInterval(countdown);
      const phase = confirmed ? "Waiting for the appliance to come back" : "Checking whether the appliance answers";
      if (timerEl) timerEl.textContent = `${phase}...`;
      say(`${phase}.`);
      void pollHealth(confirmed, phase);
    }
  }, 1000);
}

// pollHealth waits for the appliance to answer: up to HEALTH_ATTEMPTS
// probes, one at a time with a second between them, each bounded by
// HEALTH_PROBE_MS. After a confirmed restart an answer reloads the page.
// After an unconfirmed one the appliance may never have restarted, so the
// dialog says so and offers Reload now instead of reloading under the
// operator, who would not learn it. phase is the waiting text.
async function pollHealth(confirmed: boolean, phase: string): Promise<void> {
  const timerEl = document.getElementById("reconnect-timer");
  for (let attempt = 1; attempt <= HEALTH_ATTEMPTS; attempt++) {
    if (timerEl) timerEl.textContent = `${phase} (${attempt}/${HEALTH_ATTEMPTS})...`;
    let up = false;
    try {
      up = await withDeadline(HEALTH_PROBE_MS, async (signal) => (await fetch("/api/v1/healthz", { cache: "no-store", signal })).ok);
    } catch {
      // Still restarting, or the probe timed out.
    }
    if (up && confirmed) {
      if (timerEl) timerEl.textContent = "Appliance online! Reloading...";
      sayNow("Appliance is back online. Reloading.");
      window.setTimeout(() => window.location.reload(), 600);
      return;
    }
    if (up) {
      endWait("The appliance answers, but did not confirm the restart. Check its uptime after you reload.");
      return;
    }
    await new Promise<void>((resolve) => window.setTimeout(resolve, 1000));
  }
  if (timerEl) timerEl.textContent = "Restart timed out.";
  endWait("The appliance did not answer in time. Use the Reload now button to check on it.");
}

// endWait ends the dialog's wait without a reload: the description says why
// (it said the page was waiting), the spinner stops, and Reload now takes
// focus.
function endWait(text: string): void {
  const textEl = document.getElementById("modal-text");
  if (textEl) textEl.textContent = text;
  const spinner = document.querySelector<HTMLElement>("#restart-modal .spinner-ring");
  if (spinner) spinner.hidden = true;
  say(text);
  showRetry();
}

// showRetry reveals the Reload now button and focuses it so a keyboard user
// can reload.
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
  // Keep the restart text's nodes, so hide() puts them back as they were.
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
      say("The update has not finished yet. Use the Reload now button to see where it stands.");
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
