// Unit tests for the pure dashboard helpers (channel label + tally mapping).
// Run with Node's built-in test runner (see the web:test task): no browser, no
// DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import type { AvailableDevice } from "../src/lib/types.ts";
import { fileURLToPath } from "node:url";

import {
  availableCardKey,
  availableGoneMessage,
  availablePlan,
  bannerIsError,
  captureFormatLabel,
  channelHiddenMessage,
  channelLabel,
  clientSummary,
  controlGoneMessage,
  downCauseTitle,
  focusFallbackRow,
  footerMetrics,
  hiddenRows,
  needsNotificationsFallback,
  streamSummary,
  tallyStates,
  tokenHiddenMessage,
} from "../src/lib/dashboard-core.ts";
import { hideInactiveKey, hideInactivePrefDevice, parseBoolPref } from "../src/lib/prefs.ts";

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
const APPLIANCE_GO = fileURLToPath(new URL("../../cmd/remotemic/appliance.go", import.meta.url));

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

test("hiddenRows hides the rows no stream carries, only with the preference on", () => {
  assert.deepEqual(hiddenRows([true, false, true, false], true), [false, true, false, true]);
  assert.deepEqual(hiddenRows([true, false], false), [false, false]);
  // Never every row: a selection that matches no row shows them all.
  assert.deepEqual(hiddenRows([false, false], true), [false, false]);
  assert.deepEqual(hiddenRows([], true), []);
});

test("focusFallbackRow picks the first visible row, or -1 when none is", () => {
  assert.equal(focusFallbackRow([true, false, false]), 1);
  assert.equal(focusFallbackRow([false, true]), 0);
  assert.equal(focusFallbackRow([true, true]), -1);
  // Chained with hiddenRows, a stranded focus always has a row to land on.
  assert.notEqual(focusFallbackRow(hiddenRows([false, true], true)), -1);
});

test("channelHiddenMessage names the hidden channel and where focus landed", () => {
  assert.equal(channelHiddenMessage(3, 1), "Channel 3 is hidden. Focus moved to channel 1.");
  assert.equal(channelHiddenMessage(2, null), "Channel 2 is hidden. Focus moved to the device settings.");
});

test("hideInactivePrefDevice reads back the device id of a hide-inactive key", () => {
  assert.equal(hideInactivePrefDevice(hideInactiveKey("usb-Foo_Mic-00")), "usb-Foo_Mic-00");
  assert.equal(hideInactivePrefDevice("remote-mic-theme"), null);
  assert.equal(hideInactivePrefDevice(null), null);
  // The prefix must start the key, not merely appear in it.
  assert.equal(hideInactivePrefDevice(`x-${hideInactiveKey("dev")}`), null);
});

test("parseBoolPref reads 1 and 0, and falls back for anything else", () => {
  assert.equal(parseBoolPref("1", false), true);
  assert.equal(parseBoolPref("0", true), false);
  assert.equal(parseBoolPref(null, true), true);
  assert.equal(parseBoolPref("yes", false), false);
  // An unrecognized value reads as the default, not as off.
  assert.equal(parseBoolPref("yes", true), true);
  assert.equal(parseBoolPref("", true), true);
});

test("streamSummary lists every configured stream, even while the device is down", () => {
  const flat = { path: "/a", mode: "opus" as const, clientConnected: false };
  const cfg = { streams: [
    { path: "/a", mode: "opus" as const, channels: [1] },
    { path: "/b", mode: "pcm" as const, channels: [1, 2] },
    { path: "/c", mode: "opus" as const, channels: [2] },
  ] };
  const down = streamSummary(flat, cfg);
  assert.deepEqual(down, { paths: ["/a", "/b", "/c"], modes: ["opus", "pcm"], connected: 0 });
  assert.equal(clientSummary(down), "0 of 3 connected");

  const serving = streamSummary({ ...flat, clientConnected: true, streams: [
    { path: "/a", clientConnected: true, droppedFrames: 0 },
    { path: "/b", clientConnected: true, droppedFrames: 0 },
    { path: "/c", clientConnected: false, droppedFrames: 0 },
  ] }, cfg);
  assert.equal(clientSummary(serving), "2 of 3 connected");
});

test("streamSummary falls back to the record without a config", () => {
  const s = streamSummary({ path: "/a", mode: "pcm", clientConnected: true });
  assert.deepEqual(s, { paths: ["/a"], modes: ["pcm"], connected: 1 });
  assert.equal(clientSummary(s), "Connected");
  assert.equal(clientSummary(streamSummary({ path: "/a", mode: "pcm", clientConnected: false })), "-");
  // A serving record lists its runtime streams when the config has not loaded.
  const live = streamSummary({ path: "/a", mode: "pcm", clientConnected: false, streams: [
    { path: "/a", clientConnected: false, droppedFrames: 0 },
    { path: "/b", clientConnected: false, droppedFrames: 0 },
  ] }, { streams: [] });
  assert.deepEqual(live.paths, ["/a", "/b"]);
  assert.deepEqual(live.modes, ["pcm"], "runtime streams carry no mode; the record's is the first stream's");
  assert.equal(clientSummary(live), "0 of 2 connected");
});

test("focus messages name the device, the control and why it went", () => {
  assert.equal(
    controlGoneMessage("Garden", "clip-1", false),
    "Garden: the channel 2 clip button went away because the device stopped streaming. Focus moved to its device settings.",
  );
  assert.equal(controlGoneMessage("Garden", "copy", true), "Garden: the Copy URL button is no longer shown. Focus moved to its device settings.");
  assert.equal(
    controlGoneMessage("Garden", "token", false),
    "Garden: the Token tag went away because the device stopped streaming. Focus moved to its device settings.",
  );
  assert.equal(controlGoneMessage("Garden", "other", true), "Garden: the control is no longer shown. Focus moved to its device settings.");
  assert.equal(tokenHiddenMessage("Garden"), "Garden no longer needs the access token. Focus moved to its device settings.");
});

test("availablePlan keeps unchanged cards and rebuilds only changed ones", () => {
  const shown = new Map([
    ["a", "ka"],
    ["b", "kb"],
    ["c", "kc"],
  ]);
  const plan = availablePlan(shown, [
    { id: "a", key: "ka" },
    { id: "b", key: "kb2" },
    { id: "d", key: "kd" },
  ]);
  assert.deepEqual(plan.remove, ["c"]);
  assert.deepEqual(plan.build, ["b", "d"], "only the changed and the new card are built");
});

test("availablePlan orders cards like the list", () => {
  const shown = new Map([
    ["a", "ka"],
    ["b", "kb"],
  ]);
  const plan = availablePlan(shown, [
    { id: "b", key: "kb" },
    { id: "a", key: "ka" },
  ]);
  assert.deepEqual(plan.order, ["b", "a"]);
  assert.deepEqual(plan.build, [], "a reorder rebuilds nothing");
  assert.deepEqual(plan.remove, []);
});

test("availablePlan rebuilds a card whose busy state changed", () => {
  const d = { device: "hw:1,0", friendlyName: "USB mic" } as unknown as AvailableDevice;
  const shown = new Map([[d.device, availableCardKey(d, true)]]);
  // The Enable settled: the same device, no longer in flight.
  const plan = availablePlan(shown, [{ id: d.device, key: availableCardKey(d, false) }]);
  assert.deepEqual(plan.build, [d.device], "a card left busy must be rebuilt idle");
  assert.notEqual(availableCardKey(d, true), availableCardKey(d, false));
  assert.equal(availableCardKey(d, false), availableCardKey({ ...d }, false), "equal data gives equal keys");
});

test("availableGoneMessage names the device and says where focus went", () => {
  assert.equal(availableGoneMessage("USB mic (hw:1,0)", true), "USB mic (hw:1,0) is no longer available.");
  assert.equal(availableGoneMessage("hw:2,0", false), "hw:2,0 is no longer available. Focus moved to the dashboard.");
});
