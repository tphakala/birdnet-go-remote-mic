// Pure, DOM-free logic for the System view's Software Update card and the wait
// for an update to install, split out so node:test covers it
// (web/test/update-core.test.ts).

import type { InstallMethod, UpdateStatus } from "./types.js";

// UpdateTone colours the card's headline: ok for up to date, info for a neutral
// state, accent for an update on offer, warn for a failed check, and error for
// a failed update.
export type UpdateTone = "ok" | "info" | "accent" | "warn" | "error";

// UpdateView is what the card shows for one UpdateStatus.
export interface UpdateView {
  headline: string;
  tone: UpdateTone;
  detail: string;
  // A second sentence, such as a failed check behind an update still on offer.
  note: string;
  // Whether Check Now can run: the build is a release, checks are on, and no
  // update is in progress.
  canCheck: boolean;
  // The version the Update button installs, or "" when there is none to offer.
  applyVersion: string;
  // How to update by hand when this installation cannot update itself.
  hint: string;
  // An update in progress, which the card shows as busy.
  busy: boolean;
}

// INSTALL_METHOD_LABELS names how the running binary was installed.
const INSTALL_METHOD_LABELS: Record<InstallMethod, string> = {
  service: "remote-mic service install",
  deb: "Debian package",
  homebrew: "Homebrew",
  manual: "Unpacked by hand",
};

// installMethodLabel names an install method; an unknown value from a later
// appliance shows as sent.
export function installMethodLabel(method: string): string {
  return (INSTALL_METHOD_LABELS as Record<string, string>)[method] ?? method;
}

const RESTART_NOTE =
  "The appliance restarts to finish, which drops connected streams for a moment, and goes back to the running version on its own if the new one does not start.";

// describeUpdate decides the card's text and actions. The order matters: an
// update in progress outranks everything, a failed update outranks the check
// result it came from, and a build that names no release never checks at all.
export function describeUpdate(u: UpdateStatus): UpdateView {
  const latest = u.latestVersion ?? "";
  const view: UpdateView = {
    headline: "",
    tone: "info",
    detail: "",
    note: "",
    canCheck: u.supported && u.checkEnabled,
    applyVersion: "",
    hint: "",
    busy: false,
  };
  const offer = (): void => {
    if (!u.available || !latest) return;
    if (u.canApply) view.applyVersion = latest;
    else view.hint = u.upgradeHint ?? "";
  };

  if (!u.supported) {
    view.headline = "Development build";
    view.detail = `This build (${u.currentVersion || "unknown"}) names no release, so it never checks for updates.`;
    return view;
  }
  if (u.phase === "downloading" || u.phase === "installing") {
    const target = latest || "the update";
    view.busy = true;
    view.canCheck = false;
    view.headline = u.phase === "downloading" ? `Downloading ${target}` : `Installing ${target}`;
    view.detail = u.phase === "downloading"
      ? "The release is downloaded and checked against its signature before anything is installed."
      : RESTART_NOTE;
    return view;
  }
  if (u.phase === "failed") {
    view.tone = "error";
    view.headline = "Update failed";
    view.detail = u.phaseMessage || "The last update attempt did not finish.";
    view.note = `Still running ${u.currentVersion}.`;
    if (u.checkEnabled) offer();
    return view;
  }
  if (!u.checkEnabled) {
    view.headline = "Update checks are off";
    view.detail = `Running ${u.currentVersion}. Turn on the daily check to hear about new releases; while it is off the appliance makes no update requests.`;
    return view;
  }
  if (u.available && latest) {
    view.tone = "accent";
    view.headline = `${latest} is available`;
    view.detail = u.canApply
      ? `Running ${u.currentVersion}. ${RESTART_NOTE}`
      : `Running ${u.currentVersion}. This installation cannot update itself:`;
    if (u.lastError) view.note = `The latest check failed: ${u.lastError}`;
    offer();
    return view;
  }
  if (u.lastError) {
    view.tone = "warn";
    view.headline = "Update check failed";
    view.detail = u.lastError;
    view.note = `Running ${u.currentVersion}. The appliance tries again on its own.`;
    return view;
  }
  if (!u.lastCheck) {
    view.headline = "Not checked yet";
    view.detail = `Running ${u.currentVersion}. The first check runs a few minutes after the appliance starts; Check Now asks at once.`;
    return view;
  }
  view.tone = "ok";
  view.headline = "Up to date";
  view.detail = `Running ${u.currentVersion}, the newest release.`;
  return view;
}

// safeNotesUrl returns the release notes address when it is an https URL, and
// "" otherwise, so a link never carries another scheme.
export function safeNotesUrl(url: string | undefined): string {
  if (!url) return "";
  try {
    return new URL(url).protocol === "https:" ? url : "";
  } catch {
    return "";
  }
}

// lastCheckText renders when the last check finished. The appliance stamps it
// with its own wall clock, which can be off (a Pi without an RTC), so the age
// is computed against the browser clock and a future time reads "just now".
export function lastCheckText(lastCheck: string | undefined, nowMs: number, relative: (fromMs: number, toMs: number) => string): string {
  if (!lastCheck) return "Never";
  const t = Date.parse(lastCheck);
  return Number.isFinite(t) ? relative(t, nowMs) : "Unknown";
}

// DOWNLOAD_WAIT_TIMEOUT_MS is how long the appliance may take to download and
// verify a release (stageTimeout in internal/update manager.go).
export const DOWNLOAD_WAIT_TIMEOUT_MS = 15 * 60_000;

// INSTALL_WAIT_TIMEOUT_MS bounds the wait once the release is handed to the
// root updater. The appliance reports a missing updater result after four
// minutes (a minute for the updater to start, two for the new version to come
// up healthy, and a minute of slack; awaitUpdater in manager.go), so this waits
// longer.
export const INSTALL_WAIT_TIMEOUT_MS = 6 * 60_000;

// InstallWaitStep is what the wait does after a probe: keep waiting, reload
// the page onto whatever runs now, or give up and offer a manual reload.
export type InstallWaitStep = "wait" | "reload" | "timeout";

// InstallStage is what the wait shows the operator.
export type InstallStage = "downloading" | "installing" | "restarting";

// InstallWait follows the appliance through an update from a stream of
// /healthz probes (null for a probe that failed) and the phases the status
// poll reports. The page reloads as soon as the target version answers. Once
// the appliance has been seen down, any version answering means the attempt
// ended (a rollback brings the old one back, and its notification says why),
// so that reloads too. While the old version keeps answering without going
// down, the download or the updater is still at work, so the wait goes on
// until its deadline: the download allowance plus the install allowance, cut
// to the install allowance once the installing phase is seen.
export class InstallWait {
  private seenDown = false;
  private installSeen = false;
  private deadline: number;

  constructor(private readonly target: string, nowMs: number) {
    this.deadline = nowMs + DOWNLOAD_WAIT_TIMEOUT_MS + INSTALL_WAIT_TIMEOUT_MS;
  }

  // wentDown reports whether a probe has failed since the wait began. Before
  // that, a failed or abandoned attempt can still close the wait; after it the
  // restart is under way and only a probe ends it.
  get wentDown(): boolean {
    return this.seenDown;
  }

  get stage(): InstallStage {
    if (this.seenDown) return "restarting";
    return this.installSeen ? "installing" : "downloading";
  }

  // installing records the status poll reporting the installing phase; the
  // first report starts the install allowance.
  installing(nowMs: number): void {
    if (this.installSeen) return;
    this.installSeen = true;
    this.deadline = nowMs + INSTALL_WAIT_TIMEOUT_MS;
  }

  probe(version: string | null, nowMs: number): InstallWaitStep {
    if (version === null) this.seenDown = true;
    else if (version === this.target || this.seenDown) return "reload";
    return nowMs >= this.deadline ? "timeout" : "wait";
  }
}

// STAGE_LABELS names each stage in the wait's progress line.
export const STAGE_LABELS: Record<InstallStage, string> = {
  downloading: "Downloading and verifying",
  installing: "Installing",
  restarting: "Restarting",
};

// formatElapsed renders a wait as m:ss.
export function formatElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}
