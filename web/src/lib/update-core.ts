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

// sentence turns a backend message (a Go error string: lowercase, often no
// final stop) into a sentence for the card: first letter capitalised, and a
// period added unless it already ends in one (a trailing colon becomes the
// period). Empty stays empty. Hints are left as sent: they end in commands
// and paths an operator copies.
export function sentence(msg: string | undefined): string {
  const t = (msg ?? "").trim().replace(/:$/, "");
  if (!t) return "";
  const s = t[0].toUpperCase() + t.slice(1);
  return /[.!?]$/.test(s) ? s : `${s}.`;
}

const RESTART_NOTE =
  "The appliance restarts to finish, which drops connected streams for a moment, and goes back to the running version on its own if the new one does not start.";

// describeUpdate decides the card's text and actions. The order matters: an
// update in progress (any phase but idle or failed) outranks everything, a
// failed update outranks the check result it came from, and a build that names
// no release never checks at all. Backend messages go through sentence().
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
    else view.hint = u.upgradeHint?.trim() || "Update it the way it was installed.";
  };

  if (!u.supported) {
    view.headline = "Development build";
    view.detail = `This build (${u.currentVersion || "unknown"}) names no release, so it never checks for updates.`;
    return view;
  }
  if (updateUnderway(u.phase)) {
    const target = latest || "the update";
    view.busy = true;
    view.canCheck = false;
    if (u.phase === "downloading") {
      view.headline = `Downloading ${target}`;
      view.detail = "The release is downloaded and checked against its signature before anything is installed.";
    } else if (u.phase === "installing") {
      view.headline = `Installing ${target}`;
      view.detail = RESTART_NOTE;
    } else {
      view.headline = `Updating to ${target}`;
      view.detail = "An update is under way.";
    }
    return view;
  }
  if (u.phase === "failed") {
    view.tone = "error";
    view.headline = "Update failed";
    view.detail = sentence(u.phaseMessage) || "The last update attempt did not finish.";
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
      : `Running ${u.currentVersion}. This installation cannot update itself.`;
    if (u.lastError) view.note = `The latest check failed: ${sentence(u.lastError)}`;
    offer();
    return view;
  }
  if (u.lastError) {
    view.tone = "warn";
    view.headline = "Update check failed";
    view.detail = sentence(u.lastError);
    view.note = `Running ${u.currentVersion}. The appliance tries again on its own; Check Now asks at once.`;
    return view;
  }
  if (!u.lastCheck) {
    view.headline = "Not checked yet";
    view.detail = `Running ${u.currentVersion}. The first check runs a few minutes after the appliance starts or checks are turned on; Check Now asks at once.`;
    return view;
  }
  // Up to date needs a known newest release: turning checks off forgets it
  // while keeping the time of the last check.
  if (!latest) {
    view.headline = "Checking soon";
    view.detail = `Running ${u.currentVersion}. The newest release is looked up again a few minutes after checks are turned on; Check Now asks at once.`;
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
// is only as right as the two clocks agree; formatRelative, the one caller's
// relative, reads a future time as "just now".
export function lastCheckText(lastCheck: string | undefined, nowMs: number, relative: (fromMs: number, toMs: number) => string): string {
  if (!lastCheck) return "Never";
  const t = Date.parse(lastCheck);
  return Number.isFinite(t) ? relative(t, nowMs) : "Unknown";
}

// INSTALL_WAIT_TIMEOUT_MS bounds the wait once the release is handed to the
// root updater. The appliance reports a missing updater result after four
// minutes (a minute for the updater to start, two for the new version to come
// up healthy, and a minute of slack; awaitUpdater in manager.go), so this waits
// longer.
export const INSTALL_WAIT_TIMEOUT_MS = 6 * 60_000;

// TICK_GAP_MS is a gap between two ticks of a one-second timer that means the
// page was asleep (a suspended machine, a throttled background tab), and
// TICK_HOLD_MS how long a deadline then waits: long enough for the store's
// own poll, which runs when the page shows again, to bring a fresh status.
export const TICK_GAP_MS = 5000;
export const TICK_HOLD_MS = 10_000;

// TickGuard decides whether a deadline may be checked on a timer tick. After
// a tick while hidden, or one that follows a gap, it holds for TICK_HOLD_MS, so
// a wait never times out on a clock that ran while nobody was looking before a
// status could arrive; it waits on nothing else, so an appliance that never
// answers still times out once the hold ends.
export class TickGuard {
  private last: number;
  private holdUntil = 0;

  constructor(nowMs: number) {
    this.last = nowMs;
  }

  // mayCheck reports whether the deadline may be checked on this tick.
  mayCheck(nowMs: number, hidden: boolean): boolean {
    if (hidden || nowMs - this.last > TICK_GAP_MS) this.holdUntil = nowMs + TICK_HOLD_MS;
    this.last = nowMs;
    return nowMs >= this.holdUntil;
  }
}

// VERSION_SETTLE_S is how long a new version must have been up before a page
// reloads onto it. The root updater confirms a new version only after it has
// stayed up for its settle period (DefaultHealthSettle, 10 s in
// internal/update/apply.go), which starts once the new process writes its
// health file after its devices open; this allows up to about 9 s for that.
// A version that dies within the settle is rolled back and never reloaded
// onto. A startup slower than that could see a page reload onto a version
// that is then rolled back; the page then says the appliance runs the old one
// again. Only the updater knows exactly; a status field for its confirmation
// would replace this margin.
export const VERSION_SETTLE_S = 20;

// VersionChange is what a status read says about the running version: the one
// this page was loaded against, another one not yet settled, or another one
// that has been up for VERSION_SETTLE_S.
export type VersionChange = "same" | "changed" | "confirmed";

// VersionWatch notices the appliance running another version than the one this
// page's scripts came from: after an update, from any tab, or after a rollback.
// The first version read is taken as the page's own, and a tab starting an
// update rebases on the version it updates from. The view acts on it: a tab
// following its own update reloads on a confirmed version, any other tab only
// offers the reload, since it may hold unsaved input. It keeps no count: a
// version's own uptime says whether it has settled.
export class VersionWatch {
  private base: string | null = null;

  // rebase makes version the page's own: a tab that updates from a version it
  // was told about must not take that version for its update landing.
  rebase(version: string): void {
    this.base = version;
  }

  seen(version: string | undefined, uptimeSeconds: number | undefined): VersionChange {
    if (!version) return "same";
    if (this.base === null) this.base = version;
    if (version === this.base) return "same";
    return (uptimeSeconds ?? 0) >= VERSION_SETTLE_S ? "confirmed" : "changed";
  }
}

// updateUnderway reports an update in progress: any phase but idle or failed,
// a phase a later appliance adds included, since the server refuses a second
// update while one runs.
export function updateUnderway(phase: string): boolean {
  return phase !== "idle" && phase !== "failed";
}

// FollowStep is what the card does with a status while following its update:
// nothing, show the install modal, end the follow, or report the install as
// overdue.
export type FollowStep = "none" | "show" | "end" | "timeout";

// UpdateFollow follows one update this tab started, from statuses read after
// the request (the store drops older reads). The page stays usable while the
// release downloads; the modal shows once the root updater has it, and a
// deadline runs only from then. idle or failed on the version the update started
// from means the attempt ended without a restart. A status on another version
// is left to VersionWatch, and the view reloads the page on its answer.
export class UpdateFollow {
  private modalShown = false;
  private ended = false;
  private installSeen = false;
  private deadline = 0;

  constructor(private readonly fromVersion: string) {}

  // reachedInstall reports whether the root updater was seen with the release,
  // after which turning checks off no longer stops the update.
  get reachedInstall(): boolean {
    return this.installSeen;
  }

  status(u: UpdateStatus): FollowStep {
    if (this.ended || u.currentVersion !== this.fromVersion) return "none";
    switch (u.phase) {
      case "idle":
      case "failed":
        this.ended = true;
        return "end";
      case "installing":
        this.installSeen = true;
        // Asked again on every installing read until the view has a modal: a
        // restart can hold it for a while.
        return this.modalShown ? "none" : "show";
      default:
        // downloading, or a phase a later appliance adds: still under way.
        return "none";
    }
  }

  // installReached records the install as reached when the new version is
  // seen answering, for a follow that never read the short installing phase.
  installReached(): void {
    this.installSeen = true;
  }

  // shown records that the view now shows the modal, which starts the install
  // deadline.
  shown(nowMs: number): void {
    if (this.modalShown) return;
    this.modalShown = true;
    this.deadline = nowMs + INSTALL_WAIT_TIMEOUT_MS;
  }

  // tick reports the install as overdue, once, when the deadline passes while
  // the modal shows.
  tick(nowMs: number): FollowStep {
    if (!this.modalShown || this.ended || nowMs < this.deadline) return "none";
    this.ended = true;
    return "timeout";
  }
}

// followEndText says why a follow ended without a restart, from the status and
// whether the install was reached: a failure carries its reason (as a
// sentence); checks turned off (here or in another tab) stop a download but
// not an install already handed over; anything else ended before installing
// (a restart during the download, say), which not every path notifies about.
export function followEndText(u: UpdateStatus, reachedInstall: boolean): { text: string; tone: "warn" | "error" } {
  if (u.phase === "failed") return { text: `Update failed: ${sentence(u.phaseMessage) || "The attempt did not finish."}`, tone: "error" };
  if (!u.checkEnabled && !reachedInstall) return { text: "The update stopped because update checks were turned off.", tone: "warn" };
  return { text: `The update did not install; still running ${u.currentVersion}.`, tone: "warn" };
}

// formatElapsed renders a wait as m:ss.
export function formatElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}
