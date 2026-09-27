// Unit tests for the pure dashboard helpers (channel label + tally mapping).
// Run with Node's built-in test runner (see the web:test task): no browser, no
// DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import type { AvailableDevice, DeviceConfig } from "../src/lib/types.ts";
import { Emitter } from "../src/lib/emitter.ts";
import type { ViewName } from "../src/lib/router-core.ts";
import { fileURLToPath } from "node:url";
import { at, FakeTimers } from "./fixtures.ts";

import {
  availableCardKey,
  availableGoneMessage,
  deviceGoneMessage,
  focusMovedMessage,
  judgeUnconfirmedSave,
  neighbourOrder,
  parseDeviceFieldPath,
  rejectedFieldKey,
  settingsFocusMessage,
  availablePlan,
  bannerIsError,
  deviceFieldLabel,
  rejectionText,
  followDashboardRoute,
  followLevels,
  LEVELS_STALE_MS,
  LevelsWatch,
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

test("availablePlan builds a new card mid-list and orders it there", () => {
  const shown = new Map([
    ["a", "ka"],
    ["c", "kc"],
  ]);
  const plan = availablePlan(shown, [
    { id: "a", key: "ka" },
    { id: "b", key: "kb" },
    { id: "c", key: "kc" },
  ]);
  assert.deepEqual(plan.build, ["b"]);
  assert.deepEqual(plan.order, ["a", "b", "c"], "a new card is ordered by its place in the list, not appended");
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

test("neighbourOrder prefers the nearest item after, then the nearest before", () => {
  assert.deepEqual(neighbourOrder(["a", "b", "c", "d"], 1), ["c", "d", "a"]);
  assert.deepEqual(neighbourOrder(["a", "b", "c", "d"], 3), ["c", "b", "a"]);
  // Several items going at once: the caller takes the first still shown, so
  // with b and c gone focus leaves b for d, the nearest card left after it.
  const left = new Set(["a", "d"]);
  assert.equal(neighbourOrder(["a", "b", "c", "d"], 1).find((id) => left.has(id)), "d");
  assert.deepEqual(neighbourOrder(["a"], 0), []);
  assert.deepEqual(neighbourOrder(["a", "b"], -1), ["a", "b"], "an item not on screen leaves every item as a candidate");
});

test("deviceGoneMessage names the removed device and where focus went", () => {
  assert.equal(deviceGoneMessage("garden", "porch"), "garden was removed. Focus moved to porch settings.");
  assert.equal(deviceGoneMessage("garden", null), "garden was removed. Focus moved to the dashboard.");
  assert.equal(settingsFocusMessage("porch"), "Focus moved to porch settings.");
});

test("availableGoneMessage names the device and says where focus went", () => {
  assert.equal(availableGoneMessage("USB mic (hw:1,0)", "hw:2,0"), "USB mic (hw:1,0) is no longer available. Focus moved to Enable hw:2,0.");
  assert.equal(availableGoneMessage("hw:2,0", null), "hw:2,0 is no longer available. Focus moved to the dashboard.");
});

test("deviceFieldLabel names the field and the device a problem points at", () => {
  const names = ["garden", "bats"];
  assert.equal(deviceFieldLabel("devices[0].name", names), "Device Name of garden");
  assert.equal(deviceFieldLabel("devices[1].streams[0].path", names), "RTSP Path of bats");
  assert.equal(deviceFieldLabel("devices[0].streams[0].opus.bitrate", names), "Opus Bitrate of garden");
  assert.equal(deviceFieldLabel("devices[1].rate", names), "Sample Rate of bats", "a unit is dropped mid-sentence");
  assert.equal(deviceFieldLabel("devices[1].streams", names), "Streams of bats");
  // An index past the list the view knows still names the field.
  assert.equal(deviceFieldLabel("devices[5].name", names), "Device Name");
  assert.equal(deviceFieldLabel("devices"), "The device list");
  assert.equal(deviceFieldLabel("device"), "Device ID");
  // The form edits only the first stream, so a later one is named.
  assert.equal(deviceFieldLabel("devices[0].streams[1].path", names), "RTSP Path of garden's stream 2");
  assert.equal(deviceFieldLabel("devices[5].streams[2].mode", names), "Stream Codec Mode of stream 3");
  // A key that only an object's prototype has is not a field.
  assert.equal(deviceFieldLabel("devices[0].constructor", names), "devices[0].constructor");
  assert.equal(deviceFieldLabel("devices[0].streams[0].bogus", names), "devices[0].streams[0].bogus", "an unknown key shows the path");
  assert.equal(deviceFieldLabel("network.hostname"), "network.hostname");
});

test("parseDeviceFieldPath reads the device, the stream and the key", () => {
  assert.deepEqual(parseDeviceFieldPath("devices[2].name"), { device: 2, stream: 0, key: "name" });
  assert.deepEqual(parseDeviceFieldPath("devices[0].streams[1].path"), { device: 0, stream: 1, key: "path" });
  assert.deepEqual(parseDeviceFieldPath("devices[0].streams[0].opus.bitrate"), { device: 0, stream: 0, key: "bitrate" });
  assert.equal(parseDeviceFieldPath("devices[0].bogus"), null, "a key with no label");
  assert.equal(parseDeviceFieldPath("devices[0].constructor"), null, "a prototype key");
  assert.equal(parseDeviceFieldPath("network.hostname"), null);
});

test("rejectionText says which action failed, on which field of which device", () => {
  assert.equal(
    rejectionText("Save failed", { field: "devices[1].streams[0].path", reason: "must be at most 128 characters" }, ["garden", "bats"]),
    "Save failed: RTSP Path of bats was rejected: must be at most 128 characters",
  );
  assert.equal(rejectionText("Toggle failed", { reason: "invalid config" }, []), "Toggle failed: the configuration was rejected: invalid config");
});

test("rejectedFieldKey marks the edited device's field, and a duplicate of it reported elsewhere", () => {
  const dev = (name: string, device: string, path: string, extra: string[] = []): DeviceConfig => ({
    name, device, path, mode: "pcm", rate: 48000, channels: [1], format: "s16",
    streams: [{ path, mode: "pcm", channels: [1] }, ...extra.map((p) => ({ path: p, mode: "pcm" as const, channels: [1] }))],
  });
  const sent = [dev("garden", "hw:1", "/bats"), dev("bats", "hw:2", "/bats", ["/garden2"])];
  // The edited device's own first stream.
  assert.equal(rejectedFieldKey("devices[0].streams[0].path", sent, "hw:1"), "path");
  assert.equal(rejectedFieldKey("devices[0].rate", sent, "hw:1"), "rate");
  // garden's new path collides with bats', reported at bats (the later one).
  assert.equal(rejectedFieldKey("devices[1].streams[0].path", sent, "hw:1"), "path");
  // A problem with another device's own field is not garden's to fix.
  assert.equal(rejectedFieldKey("devices[1].rate", sent, "hw:1"), null);
  assert.equal(rejectedFieldKey("devices[1].name", sent, "hw:1"), null, "bats' own name is not a duplicate of garden's");
  assert.equal(rejectedFieldKey("devices[1].streams[1].path", sent, "hw:1"), null, "a different path");
  // A duplicate name reported at the other device.
  const named = [dev("porch", "hw:1", "/a"), dev("porch", "hw:2", "/b")];
  assert.equal(rejectedFieldKey("devices[1].name", named, "hw:1"), "name");
  // The edited device's second stream is not in the form.
  assert.equal(rejectedFieldKey("devices[1].streams[1].mode", sent, "hw:2"), null);
  assert.equal(rejectedFieldKey("network.hostname", sent, "hw:1"), null);
});

test("focusMovedMessage says where focus went", () => {
  assert.equal(focusMovedMessage("porch"), "Focus moved to porch settings.");
  assert.equal(focusMovedMessage(null), "Focus moved to the dashboard.");
});

test("judgeUnconfirmedSave judges a lost save by the re-read", () => {
  assert.equal(judgeUnconfirmedSave(false, "a", "b", "b"), "unread");
  assert.equal(judgeUnconfirmedSave(true, "a", "b", "b"), "applied");
  assert.equal(judgeUnconfirmedSave(true, "a", "a", "b"), "notApplied");
  assert.equal(judgeUnconfirmedSave(true, "a", "c", "b"), "changed", "neither as before nor as sent");
  // A save that changed nothing cannot be told apart by the re-read.
  assert.equal(judgeUnconfirmedSave(true, "a", "a", "a"), "unchanged");
  assert.equal(judgeUnconfirmedSave(true, "a", "c", "a"), "changed");
});

// FakeRouter announces routes as the app's router does.
class FakeRouter extends Emitter<{ route: ViewName }> {
  go(view: ViewName): void {
    this.emit("route", view);
  }
}

test("the dashboard follower suspends meters and drops levels off the dashboard", () => {
  const router = new FakeRouter();
  const calls: string[] = [];
  followDashboardRoute(router, {
    setFramesSuspended: (s) => calls.push(`frames ${s ? "off" : "on"}`),
    setLevelsWanted: (w) => calls.push(`levels ${w ? "on" : "off"}`),
  });
  router.go("dashboard");
  router.go("system");
  router.go("events");
  router.go("dashboard");
  assert.deepEqual(calls, [
    "frames on", "levels on",
    "frames off", "levels off",
    "frames off", "levels off",
    "frames on", "levels on",
  ]);
});

// watchHarness builds a LevelsWatch on fake timers and a clock the test sets,
// counting how often it clears the meters.
function watchHarness() {
  const timers = new FakeTimers();
  let clock = 0;
  let clears = 0;
  const watch = new LevelsWatch({ timers, now: () => clock, clear: () => clears++ });
  return {
    watch,
    timers,
    clears: () => clears,
    setClock: (t: number) => {
      clock = t;
    },
    // tick runs the check interval once.
    tick: () => at(timers.intervals(), 0).fn(),
  };
}

test("LevelsWatch clears the meters once when levels stop on a live stream", () => {
  const h = watchHarness();
  h.watch.setShown(true);
  h.watch.setConnected(true);
  h.watch.levels();
  h.setClock(LEVELS_STALE_MS);
  h.tick();
  assert.equal(h.clears(), 0, "a gap of the full window is still current");
  h.setClock(LEVELS_STALE_MS + 1);
  h.tick();
  assert.equal(h.clears(), 1, "a longer gap clears the meters");
  h.setClock(LEVELS_STALE_MS * 3);
  h.tick();
  assert.equal(h.clears(), 1, "a long gap clears them once");
  h.watch.levels();
  h.setClock(LEVELS_STALE_MS * 4 + 1);
  h.tick();
  assert.equal(h.clears(), 2, "levels that came back and stopped again clear again");
});

test("LevelsWatch runs its check only while the dashboard shows on a live stream", () => {
  const h = watchHarness();
  h.watch.setConnected(true);
  assert.equal(h.timers.intervals().length, 0, "the dashboard is not showing");
  h.watch.setShown(true);
  assert.equal(h.timers.intervals().length, 1);
  h.watch.setShown(false);
  assert.equal(h.timers.intervals().length, 0, "leaving the dashboard stops the check");
  h.watch.setShown(true);
  // Showing again starts a new window rather than judging the time away.
  h.setClock(LEVELS_STALE_MS * 10);
  h.watch.setShown(false);
  h.watch.setShown(true);
  h.tick();
  assert.equal(h.clears(), 0);
});

test("LevelsWatch clears at once when the stream goes down and stops its check", () => {
  const h = watchHarness();
  h.watch.setShown(true);
  h.watch.setConnected(true);
  h.watch.setConnected(false);
  assert.equal(h.clears(), 1);
  assert.equal(h.timers.intervals().length, 0);
});

// FakeLevelsEvents announces what the store does about levels.
class FakeLevelsEvents extends Emitter<{ levels: undefined; levelsdropped: undefined; connection: boolean }> {
  fire(name: "levels" | "levelsdropped"): void {
    this.emit(name);
  }
  connection(up: boolean): void {
    this.emit("connection", up);
  }
}

test("followLevels feeds the watch from the store's announcements", () => {
  const h = watchHarness();
  const events = new FakeLevelsEvents();
  followLevels(events, h.watch);
  h.watch.setShown(true);
  events.connection(true);
  assert.equal(h.timers.intervals().length, 1, "a live stream starts the check");
  events.fire("levelsdropped");
  assert.equal(h.clears(), 1, "dropped levels clear the meters");
  events.fire("levels");
  h.setClock(LEVELS_STALE_MS + 1);
  h.tick();
  assert.equal(h.clears(), 2, "levels that stop again clear again");
  events.connection(false);
  assert.equal(h.clears(), 3, "a stream going down clears the meters");
});
