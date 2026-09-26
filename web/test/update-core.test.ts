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
  UpdateFollow,
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
  assert.equal(v.hint, hint);
  assert.ok(v.detail.includes("cannot update itself"), v.detail);
});

test("describeUpdate: a failed check behind an offered update is a note", () => {
  const v = describeUpdate(status({ latestVersion: "v0.3.0", available: true, lastError: "timeout" }));
  assert.equal(v.applyVersion, "v0.3.0");
  assert.equal(v.note, "The latest check failed: timeout");
});

test("describeUpdate: a failed check with nothing on offer", () => {
  const v = describeUpdate(status({ lastError: "signature" }));
  assert.equal(v.headline, "Update check failed");
  assert.equal(v.tone, "warn");
  assert.equal(v.detail, "signature");
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
  assert.equal(v.detail, "the root updater did not start");
  assert.equal(v.applyVersion, "v0.3.0");
  // No retry while checks are off: the appliance would refuse it.
  assert.equal(describeUpdate(status({ latestVersion: "v0.3.0", available: true, phase: "failed", checkEnabled: false })).applyVersion, "");
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
  assert.equal(w.seen("v0.2.0"), "same");
  assert.equal(w.seen("v0.2.0"), "same");
  assert.equal(w.seen(undefined), "same");
  assert.equal(w.seen(""), "same");
});

test("VersionWatch confirms a new version on the second read in a row", () => {
  const w = new VersionWatch();
  w.seen("v0.2.0");
  assert.equal(w.seen("v0.3.0"), "changed");
  assert.equal(w.seen("v0.3.0"), "confirmed");
  assert.equal(w.seen("v0.3.0"), "confirmed");
});

test("VersionWatch starts over when the page's own version answers again (a rollback)", () => {
  const w = new VersionWatch();
  w.seen("v0.2.0");
  assert.equal(w.seen("v0.3.0"), "changed");
  assert.equal(w.seen("v0.2.0"), "same");
  assert.equal(w.seen("v0.3.0"), "changed");
});

test("VersionWatch rebase forgets a change read before an update starts", () => {
  // A tab loaded on v0.2.0 was told about v0.3.0, then updates from v0.3.0.
  const w = new VersionWatch();
  w.seen("v0.2.0");
  w.seen("v0.3.0");
  assert.equal(w.seen("v0.3.0"), "confirmed");
  w.rebase("v0.3.0");
  assert.equal(w.seen("v0.3.0"), "same", "the version updated from is not the update landing");
  assert.equal(w.seen("v0.4.0"), "changed");
  assert.equal(w.seen("v0.4.0"), "confirmed");
  // A rebase starts the count again, even straight into another version.
  w.rebase("v0.3.0");
  assert.equal(w.seen("v0.4.0"), "changed");
});

test("UpdateFollow keeps following while downloading or in an unknown phase", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.status(status({ phase: "downloading" }), 0), "none");
  assert.equal(f.status(status({ phase: "verifying" as UpdatePhase }), 0), "none");
  assert.equal(f.isShown, false);
  assert.equal(f.tick(INSTALL_WAIT_TIMEOUT_MS * 10), "none", "no deadline before the modal shows");
});

test("UpdateFollow shows the modal when installing is first seen", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.status(status({ phase: "installing" }), 1000), "show");
  assert.equal(f.isShown, true);
  assert.equal(f.status(status({ phase: "installing" }), 5000), "none", "shown once");
});

test("UpdateFollow ends on idle or failed on the version it started from", () => {
  for (const phase of ["idle", "failed"] as const) {
    const f = new UpdateFollow("v0.2.0");
    f.status(status({ phase: "installing" }), 0);
    assert.equal(f.status(status({ phase }), 1000), "end", phase);
    assert.equal(f.isShown, false, phase);
  }
});

test("UpdateFollow ignores a status from another version", () => {
  const f = new UpdateFollow("v0.2.0");
  // The restarted appliance starts idle; that is VersionWatch's to handle.
  assert.equal(f.status(status({ currentVersion: "v0.3.0", phase: "idle" }), 0), "none");
  assert.equal(f.status(status({ phase: "installing" }), 0), "show", "still following");
});

test("UpdateFollow ignores statuses after it ended", () => {
  const f = new UpdateFollow("v0.2.0");
  assert.equal(f.status(status({ phase: "failed" }), 0), "end");
  assert.equal(f.status(status({ phase: "installing" }), 0), "none");
  assert.equal(f.status(status({ phase: "idle" }), 0), "none");
});

test("UpdateFollow times out once, only after showing, at the install deadline", () => {
  const f = new UpdateFollow("v0.2.0");
  f.status(status({ phase: "installing" }), 1000);
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS - 1), "none");
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS), "timeout");
  assert.equal(f.tick(1000 + INSTALL_WAIT_TIMEOUT_MS + 1), "none");
  assert.equal(f.status(status({ phase: "idle" }), 0), "none", "a timed-out follow has ended");
});

test("followEndText says why from the status", () => {
  assert.deepEqual(followEndText(status({ phase: "failed", phaseMessage: "signature" })), { text: "Update failed: signature", tone: "error" });
  assert.equal(followEndText(status({ phase: "failed" })).text, "Update failed: the attempt did not finish");
  assert.deepEqual(followEndText(status({ phase: "idle", checkEnabled: false })), { text: "The update stopped because update checks were turned off.", tone: "warn" });
  assert.equal(followEndText(status({ phase: "idle" })).text, "The update did not install; still running v0.2.0. The notifications say why.");
});
