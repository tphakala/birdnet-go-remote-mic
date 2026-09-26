// Tests for the Software Update card's decisions and the install wait
// (web/src/lib/update-core.ts).

import test from "node:test";
import assert from "node:assert/strict";

import {
  DOWNLOAD_WAIT_TIMEOUT_MS,
  describeUpdate,
  formatElapsed,
  INSTALL_WAIT_TIMEOUT_MS,
  InstallWait,
  installMethodLabel,
  lastCheckText,
  safeNotesUrl,
} from "../src/lib/update-core.js";
import type { UpdateStatus } from "../src/lib/types.js";

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

test("InstallWait reloads when the new version answers", () => {
  const w = new InstallWait("v0.3.0", 0);
  assert.equal(w.stage, "downloading");
  assert.equal(w.probe("v0.2.0", 1000), "wait");
  w.installing(2000);
  assert.equal(w.stage, "installing");
  assert.equal(w.probe(null, 3000), "wait");
  assert.equal(w.stage, "restarting");
  assert.equal(w.probe("v0.3.0", 4000), "reload");
});

test("InstallWait reloads onto the old version after a restart (a rollback)", () => {
  const w = new InstallWait("v0.3.0", 0);
  assert.equal(w.wentDown, false);
  assert.equal(w.probe(null, 1000), "wait");
  assert.equal(w.wentDown, true);
  assert.equal(w.probe("v0.2.0", 2000), "reload");
});

test("InstallWait gives up after the download and install allowances", () => {
  const w = new InstallWait("v0.3.0", 0);
  const all = DOWNLOAD_WAIT_TIMEOUT_MS + INSTALL_WAIT_TIMEOUT_MS;
  assert.equal(w.probe("v0.2.0", all - 1), "wait");
  assert.equal(w.probe("v0.2.0", all), "timeout");
});

test("InstallWait restarts its deadline once, when installing is first seen", () => {
  const w = new InstallWait("v0.3.0", 0);
  w.installing(1000);
  // A later report does not push the deadline out again.
  w.installing(5000);
  assert.equal(w.probe("v0.2.0", 1000 + INSTALL_WAIT_TIMEOUT_MS - 1), "wait");
  assert.equal(w.probe(null, 1000 + INSTALL_WAIT_TIMEOUT_MS), "timeout");
});

test("InstallWait reloads when the new version answers without a missed probe", () => {
  // A restart quicker than the probe interval is never seen down.
  const w = new InstallWait("v0.3.0", 0);
  assert.equal(w.probe("v0.2.0", 1000), "wait");
  assert.equal(w.probe("v0.3.0", 3000), "reload");
  assert.equal(w.wentDown, false);
});
