// Unit tests for the pure dashboard helpers (channel label + tally mapping). Run
// with Node's built-in test runner over the compiled output (see the web:test
// task): no browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { bannerIsError, captureFormatLabel, channelLabel, downCauseTitle, footerMetrics, needsNotificationsFallback, tallyStates } from "../src/lib/dashboard-core.js";

test("needsNotificationsFallback loads when nothing loaded or the stream is down", () => {
  assert.equal(needsNotificationsFallback(true, true), false); // healthy: the connect re-sync loaded it
  assert.equal(needsNotificationsFallback(false, true), true); // connected but the re-sync has not landed
  assert.equal(needsNotificationsFallback(true, false), true); // a previous session's snapshot, stream down
  assert.equal(needsNotificationsFallback(false, false), true);
});

test("bannerIsError: a failed device is an error unless it was unplugged", () => {
  assert.equal(bannerIsError("failed", "failed"), true);
  assert.equal(bannerIsError("failed", undefined), true);
  assert.equal(bannerIsError("failed", "disconnected"), false);
  assert.equal(bannerIsError("skipped", "not-connected"), false);
  assert.equal(bannerIsError("skipped", "open-failed"), false);
});

test("downCauseTitle names each cause as its notification does and falls back", () => {
  assert.equal(downCauseTitle("not-connected"), "Device not connected");
  assert.equal(downCauseTitle("ambiguous"), "Device ambiguous");
  assert.equal(downCauseTitle("malformed"), "Invalid device id");
  assert.equal(downCauseTitle("same-hardware"), "Device conflict");
  assert.equal(downCauseTitle("resolve-failed"), "Device unavailable");
  assert.equal(downCauseTitle("open-failed"), "Device unavailable");
  assert.equal(downCauseTitle("disconnected"), "Device disconnected");
  assert.equal(downCauseTitle("failed"), "Device failed");
  // An older appliance sends no cause, and a newer one may add a class.
  assert.equal(downCauseTitle(undefined), "Device excluded from streaming");
  assert.equal(downCauseTitle("some-future-cause"), "Device excluded from streaming");
});

// The banner titles promise to match the notification titles the appliance
// raises for the same cause; this reads the Go source so a title renamed on
// one side only fails here instead of silently drifting.
const APPLIANCE_GO = fileURLToPath(new URL("../../../cmd/remotemic/appliance.go", import.meta.url).href);

test("every downCauseTitle is a notification title in cmd/remotemic/appliance.go", () => {
  const src = readFileSync(APPLIANCE_GO, "utf8");
  const causes = [
    "not-connected", "ambiguous", "malformed", "resolve-failed",
    "same-hardware", "open-failed", "disconnected", "failed",
  ];
  for (const cause of causes) {
    const title = downCauseTitle(cause);
    assert.ok(src.includes(`"${title}"`), `${cause}: title "${title}" not found in appliance.go`);
  }
});

test("channelLabel renders mono, contiguous, and non-contiguous selections", () => {
  assert.equal(channelLabel([]), "");
  assert.equal(channelLabel([1]), "Ch 1");
  assert.equal(channelLabel([2]), "Ch 2");
  assert.equal(channelLabel([1, 2]), "Ch 1+2");
  assert.equal(channelLabel([1, 3]), "Ch 1+3");
  assert.equal(channelLabel([1, 2, 4]), "Ch 1+2+4");
});

test("tallyStates lights the streamed 1-based channels across the row count", () => {
  // Two captured channels, only channel 1 streamed.
  assert.deepEqual(tallyStates([1], 2), [true, false]);
  // Both streamed.
  assert.deepEqual(tallyStates([1, 2], 2), [true, true]);
  // Non-contiguous selection over four captured channels.
  assert.deepEqual(tallyStates([1, 3], 4), [true, false, true, false]);
  // No streamed channels leaves every row dim.
  assert.deepEqual(tallyStates([], 3), [false, false, false]);
  // A streamed channel beyond the row count does not overflow the output.
  assert.deepEqual(tallyStates([5], 2), [false, false]);
});

test("captureFormatLabel renders bit depth for negotiated formats and falls back", () => {
  assert.equal(captureFormatLabel("s16"), "16-bit");
  assert.equal(captureFormatLabel("s24_le"), "24-bit");
  assert.equal(captureFormatLabel("s24_3le"), "24-bit");
  assert.equal(captureFormatLabel("s32"), "32-bit");
  assert.equal(captureFormatLabel("f32"), "32-bit float");
  // An unrecognised token (a future capture format) shows its uppercased form.
  assert.equal(captureFormatLabel("s20_3le"), "S20_3LE");
});

test("footerMetrics formats the card footer counters", () => {
  assert.deepEqual(footerMetrics({ clientConnected: true, droppedFrames: 12, overruns: 3 }), {
    clients: "1 connected",
    dropped: "12",
    overruns: "3",
  });
  assert.deepEqual(footerMetrics({ clientConnected: false, droppedFrames: 0, overruns: 0 }), {
    clients: "0 connected",
    dropped: "0",
    overruns: "0",
  });
  // An appliance that predates the overrun counter omits it: zero, not "undefined".
  assert.equal(footerMetrics({ clientConnected: false, droppedFrames: 5 }).overruns, "0");
});
