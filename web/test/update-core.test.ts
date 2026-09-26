// Tests for the Software Update card's decisions and the install wait
// (web/src/lib/update-core.ts).

import test from "node:test";
import assert from "node:assert/strict";

import {
  describeUpdate,
  followEndText,
  formatElapsed,
  INSTALL_WAIT_TIMEOUT_MS,
  installMethodLabel,
  lastCheckText,
  safeNotesUrl,
  sentence,
  TICK_GAP_MS,
  TICK_HOLD_MS,
  TickGuard,
  UpdateFollow,
  updateUnderway,
  VERSION_SETTLE_S,
  VersionWatch,
} from "../src/lib/update-core.js";
import type { UpdatePhase, UpdateStatus } from "../src/lib/types.js";

// status builds an up-to-date, checked service install; a test overrides what
// it is about.
const status = (over: Partial<UpdateStatus> = {}): UpdateStatus => ({
  currentVersion: "v0.2.0",
  supported: true,
  checkEnabled: true,
  latestVersion: "v0.2.0",
  available: false,
  lastCheck: "2026-09-26T10:00:00Z",
  installMethod: "service",
  canApply: true,
  phase: "idle",
  ...over,
});

test("describeUpdate: up to date", () => {
  const v = describeUpdate(status());
  assert.equal(v.headline, "Up to date");
  assert.equal(v.tone, "ok");
  assert.equal(v.canCheck, true);
  assert.equal(v.applyVersion, "");
  assert.equal(v.busy, false);
});

test("describeUpdate: an update this installation can apply", () => {
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true }));
  assert.equal(v.headline, "v0.3.0 is available");
  assert.equal(v.tone, "accent");
  assert.equal(v.applyVersion, "v0.3.0");
  assert.equal(v.hint, "");
  assert.equal(v.note, "");
});

test("describeUpdate: an update a package manager installs gets the hint, not the button", () => {
  const hint = "Run brew upgrade birdnet-go-remote-mic";
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, installMethod: "homebrew", canApply: false, upgradeHint: hint }));
  assert.equal(v.applyVersion, "");
  assert.equal(v.hint, hint, "a hint is left as sent: it ends in a command");
  assert.equal(v.detail, "Running v0.2.0. This installation cannot update itself.");
});

test("describeUpdate: a failed check behind an offered update is a note", () => {
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, lastError: "timeout" }));
  assert.equal(v.applyVersion, "v0.3.0");
  assert.equal(v.note, "The latest check failed: Timeout.");
});

test("describeUpdate: a failed check with nothing on offer", () => {
  const v = describeUpdate(status({ lastError: "signature" }));
  assert.equal(v.headline, "Update check failed");
  assert.equal(v.tone, "warn");
  assert.equal(v.detail, "Signature.");
  assert.equal(v.note, "Running v0.2.0. The appliance tries again on its own; Check Now asks at once.");
});

test("describeUpdate: never checked", () => {
  const v = describeUpdate(status({ lastCheck: undefined, latestVersion: undefined }));
  assert.equal(v.headline, "Not checked yet");
  assert.equal(v.canCheck, true);
});

test("describeUpdate: checks off offer neither a check nor an update", () => {
  const v = describeUpdate(status({ checkEnabled: false, latestVersion: "v0.3.0", available: true }));
  assert.equal(v.headline, "Update checks are off");
  assert.equal(v.canCheck, false);
  assert.equal(v.applyVersion, "");
});

test("describeUpdate: a development build never checks", () => {
  const v = describeUpdate(status({ supported: false, currentVersion: "dev" }));
  assert.equal(v.headline, "Development build");
  assert.equal(v.canCheck, false);
  assert.equal(v.applyVersion, "");
  assert.ok(v.detail.includes("(dev)"), v.detail);
});

test("describeUpdate: an update in progress is busy and offers no check", () => {
  for (const phase of ["downloading", "installing"] as const) {
    const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase }));
    assert.equal(v.busy, true, phase);
    assert.equal(v.canCheck, false, phase);
    assert.equal(v.applyVersion, "", phase);
    assert.ok(v.headline.endsWith("v0.3.0"), v.headline);
  }
});

test("describeUpdate: a failed update says why and offers a retry", () => {
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase: "failed", phaseMessage: "the root updater did not start" }));
  assert.equal(v.headline, "Update failed");
  assert.equal(v.tone, "error");
  assert.equal(v.detail, "The root updater did not start.");
  assert.equal(v.note, "Still running v0.2.0.");
  assert.equal(v.applyVersion, "v0.3.0");
  // No retry while checks are off: the appliance would refuse it.
  assert.equal(describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase: "failed", checkEnabled: false })).applyVersion, "");
});

test("describeUpdate: each phase under way names its own stage", () => {
  const dl = describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase: "downloading" }));
  assert.equal(dl.headline, "Downloading v0.3.0");
  assert.ok(dl.detail.includes("signature"), dl.detail);
  const inst = describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase: "installing" }));
  assert.equal(inst.headline, "Installing v0.3.0");
  assert.ok(inst.detail.includes("restarts"), inst.detail);
  assert.equal(describeUpdate(status({ latestVersion: undefined, phase: "downloading" })).headline, "Downloading the update");
});

test("describeUpdate: a phase a later appliance adds counts as under way", () => {
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase: "verifying" as UpdatePhase }));
  assert.equal(v.headline, "Updating to v0.3.0", "not named as a stage it may not be");
  assert.equal(v.busy, true);
  assert.equal(v.canCheck, false);
  assert.equal(v.applyVersion, "");
});

test("describeUpdate: a failed update with nothing newer offers no update", () => {
  const v = describeUpdate(status({ phase: "failed", available: false }));
  assert.equal(v.applyVersion, "");
  assert.equal(v.detail, "The last update attempt did not finish.");
  const hinted = describeUpdate(status({ phase: "failed", latestVersion: "v0.3.0", available: true, canApply: false, upgradeHint: "run it by hand" }));
  assert.equal(hinted.applyVersion, "");
  assert.equal(hinted.hint, "run it by hand");
});

test("describeUpdate: up to date only when the newest release is known", () => {
  // Checks turned off and on again forget the latest release but keep lastCheck.
  const v = describeUpdate(status({ latestVersion: undefined }));
  assert.equal(v.headline, "Checking soon");
  assert.ok(v.detail.includes("checks are turned on"), v.detail);
  const never = describeUpdate(status({ latestVersion: undefined, lastCheck: undefined }));
  assert.equal(never.headline, "Not checked yet");
  assert.ok(never.detail.includes("or checks are turned on"), never.detail);
});

test("describeUpdate: no upgrade hint still says how to update, in a sentence", () => {
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, canApply: false, upgradeHint: undefined }));
  assert.equal(v.hint, "Update it the way it was installed.");
  const blank = describeUpdate(status({ latestVersion: "v0.3.0", available: true, canApply: false, upgradeHint: "   " }));
  assert.equal(blank.hint, "Update it the way it was installed.", "a blank hint counts as none");
});

test("describeUpdate: neutral states are info toned", () => {
  assert.equal(describeUpdate(status({ supported: false })).tone, "info");
  assert.equal(describeUpdate(status({ checkEnabled: false })).tone, "info");
  assert.equal(describeUpdate(status({ lastCheck: undefined })).tone, "info");
});

test("sentence capitalises and ends a backend message once", () => {
  assert.equal(sentence("timeout"), "Timeout.");
  assert.equal(sentence("already ends."), "Already ends.");
  assert.equal(sentence("asks?"), "Asks?");
  assert.equal(sentence("dangles:"), "Dangles.");
  assert.equal(sentence("  "), "");
  assert.equal(sentence(undefined), "");
});

test("installMethodLabel names each method and passes an unknown one through", () => {
  assert.equal(installMethodLabel("deb"), "Debian package");
  assert.equal(installMethodLabel("snap"), "snap");
});

test("safeNotesUrl accepts only https", () => {
  assert.equal(safeNotesUrl("https://github.com/x/y/releases/tag/v1"), "https://github.com/x/y/releases/tag/v1");
  assert.equal(safeNotesUrl("javascript:alert(1)"), "");
  assert.equal(safeNotesUrl("http://example.com"), "");
  assert.equal(safeNotesUrl("not a url"), "");
  assert.equal(safeNotesUrl(undefined), "");
});

test("lastCheckText", () => {
  const rel = (from: number, to: number): string => `${Math.round((to - from) / 1000)}s`;
  assert.equal(lastCheckText(undefined, 0, rel), "Never");
  assert.equal(lastCheckText("garbage", 0, rel), "Unknown");
  assert.equal(lastCheckText("1970-01-01T00:00:10Z", 70_000, rel), "60s");
});

test("formatElapsed", () => {
  assert.equal(formatElapsed(0), "0:00");
  assert.equal(formatElapsed(65_400), "1:05");
  assert.equal(formatElapsed(-5), "0:00");
});

test("VersionWatch takes the first version as the page's own", () => {
  const w = new VersionWatch();
  assert.equal(w.seen("v0.2.0", 100), "same");
  assert.equal(w.seen("v0.2.0", 5), "same");
  assert.equal(w.seen(undefined, 100), "same");
  assert.equal(w.seen("", 100), "same");
});

test("VersionWatch waits for a new version to settle before confirming it", () => {
  const w = new VersionWatch();
  w.seen("v0.2.0", 100);
  assert.equal(w.seen("v0.3.0", 2), "changed");
  assert.equal(w.seen("v0.3.0", VERSION_SETTLE_S - 1), "changed");
  assert.equal(w.seen("v0.3.0", VERSION_SETTLE_S), "confirmed");
  assert.equal(w.seen("v0.3.0", undefined), "changed", "no uptime is not settled");
});

test("VersionWatch reads the page's own version again as same (a rollback)", () => {
  const w = new VersionWatch();
  w.seen("v0.2.0", 100);
  assert.equal(w.seen("v0.3.0", 3), "changed");
  assert.equal(w.seen("v0.2.0", 1), "same");
});

test("VersionWatch rebase makes the version updated from the page's own", () => {
  // A tab loaded on v0.2.0 was told about v0.3.0, then updates from v0.3.0.
  const w = new VersionWatch();
  w.seen("v0.2.0", 100);
  assert.equal(w.seen("v0.3.0", 100), "confirmed");
  w.rebase("v0.3.0");
  assert.equal(w.seen("v0.3.0", 100), "same", "the version updated from is not the update landing");
  assert.equal(w.seen("v0.4.0", 100), "confirmed");
});

test("TickGuard lets a steady visible timer check the deadline", () => {
  const g = new TickGuard(0);
  assert.equal(g.mayCheck(1000, false), true);
  assert.equal(g.mayCheck(2000, false), true);
});

// tickUntil runs a one-second timer up to and including endMs and returns
// the first time the guard let the deadline be checked, or null.
function tickUntil(g: TickGuard, fromMs: number, endMs: number): number | null {
  for (let t = fromMs; t <= endMs; t += 1000) if (g.mayCheck(t, false)) return t;
  return null;
}

test("TickGuard holds after a hidden tick until the hold has passed", () => {
  const g = new TickGuard(0);
  assert.equal(g.mayCheck(1000, true), false);
  assert.equal(g.mayCheck(2000, true), false, "hidden keeps holding");
  // Shown again at 3 s: steady ticks resume once the hold from the last
  // hidden tick ends; an appliance that never answers still times out then.
  assert.equal(tickUntil(g, 3000, 30_000), 2000 + TICK_HOLD_MS);
});

test("TickGuard holds after a gap between ticks (a machine that slept)", () => {
  const g = new TickGuard(0);
  assert.equal(g.mayCheck(1000, false), true);
  assert.equal(g.mayCheck(1000 + TICK_GAP_MS, false), true, "a gap of exactly the limit is not a sleep");
  const woke = 1000 + 2 * TICK_GAP_MS + 1;
  assert.equal(g.mayCheck(woke, false), false);
  assert.equal(tickUntil(g, woke + 1000, woke + 30_000), woke + TICK_HOLD_MS);
});

test("updateUnderway is anything but idle or failed", () => {
  assert.equal(updateUnderway("downloading"), true);
  assert.equal(updateUnderway("installing"), true);
  assert.equal(updateUnderway("verifying"), true);
  assert.equal(updateUnderway("idle"), false);
  assert.equal(updateUnderway("failed"), false);
});

test("UpdateFollow keeps following while downloading or in an unknown phase", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.status(status({ phase: "downloading" })), "none");
  assert.equal(f.status(status({ phase: "verifying" as UpdatePhase })), "none");
  assert.equal(f.reachedInstall, false);
  assert.equal(f.tick(INSTALL_WAIT_TIMEOUT_MS * 10), "none", "no deadline before the modal shows");
});

test("UpdateFollow asks for the modal until one is shown", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.status(status({ phase: "installing" })), "show");
  assert.equal(f.reachedInstall, true);
  // A restart held the modal: the next installing read asks again.
  assert.equal(f.status(status({ phase: "installing" })), "show");
  f.shown(1000);
  assert.equal(f.status(status({ phase: "installing" })), "none");
});

test("UpdateFollow counts the install as reached once the new version is seen", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.reachedInstall, false);
  f.installReached();
  assert.equal(f.reachedInstall, true);
});

test("UpdateFollow ends on idle or failed on the version it started from", () => {
  for (const phase of ["idle", "failed"] as const) {
    const f = new UpdateFollow("v0.2.0");
    f.status(status({ phase: "installing" }));
    f.shown(0);
    assert.equal(f.status(status({ phase })), "end", phase);
  }
});

test("UpdateFollow ignores a status from another version", () => {
  const f = new UpdateFollow("v0.2.0");
  // The restarted appliance starts idle; that is VersionWatch's to handle.
  assert.equal(f.status(status({ currentVersion: "v0.3.0", phase: "idle" })), "none");
  assert.equal(f.status(status({ phase: "installing" })), "show", "still following");
});

test("UpdateFollow ignores statuses after it ended", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.status(status({ phase: "failed" })), "end");
  assert.equal(f.status(status({ phase: "installing" })), "none");
  assert.equal(f.status(status({ phase: "idle" })), "none");
});

test("UpdateFollow times out once, only after showing, at the install deadline", () => {
  const f = new UpdateFollow("v0.2.0");
  f.status(status({ phase: "installing" }));
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS), "none", "no deadline until shown");
  f.shown(1000);
  f.shown(5000);
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS - 1), "none");
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS), "timeout", "the deadline runs from the first shown");
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS + 1), "none");
  assert.equal(f.status(status({ phase: "idle" })), "none", "a timed-out follow has ended");
});

test("followEndText says why from the status and the stage reached", () => {
  assert.deepEqual(followEndText(status({ phase: "failed", phaseMessage: "signature" }), false), { text: "Update failed: Signature.", tone: "error" });
  assert.equal(followEndText(status({ phase: "failed" }), true).text, "Update failed: The attempt did not finish.");
  // A failure outranks checks being off.
  assert.equal(followEndText(status({ phase: "failed", checkEnabled: false, phaseMessage: "x" }), false).tone, "error");
  assert.deepEqual(followEndText(status({ phase: "idle", checkEnabled: false }), false), { text: "The update stopped because update checks were turned off.", tone: "warn" });
  // An install already handed over is not stopped by turning checks off.
  assert.equal(followEndText(status({ phase: "idle", checkEnabled: false }), true).text, "The update did not install; still running v0.2.0.");
  assert.equal(followEndText(status({ phase: "idle" }), false).text, "The update did not install; still running v0.2.0.");
});

