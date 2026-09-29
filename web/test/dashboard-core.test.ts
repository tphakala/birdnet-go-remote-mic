// Unit tests for the pure dashboard helpers (channel label + tally mapping).
// Run with Node's built-in test runner (see the web:test task): no browser, no
// DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import type { AvailableDevice, Device, DeviceConfig } from "../src/lib/types.ts";
import { Emitter } from "../src/lib/emitter.ts";
import type { ViewName } from "../src/lib/router-core.ts";
import { fileURLToPath } from "node:url";
import { at, FakeTimers } from "./fixtures.ts";

import {
  availableCardKey,
  availableCardShape,
  availableGoneMessage,
  availableLabel,
  deviceConfigKey,
  deviceGoneMessage,
  deviceToConfig,
  focusMovedMessage,
  judgeUnconfirmedSave,
  neighbourOrder,
  parseDeviceFieldPath,
  rejectedFieldKey,
  runtimeEnabled,
  settingsFocusMessage,
  availablePlan,
  avatarLook,
  bannerIsError,
  capsSummary,
  cardShape,
  deviceFieldLabel,
  rejectionText,
  followDashboardRoute,
  followLevels,
  routeLevels,
  LEVELS_STALE_MS,
  LevelsWatch,
  captureFormatLabel,
  channelHiddenMessage,
  channelLabel,
  clientCountOf,
  clientSummary,
  controlGoneMessage,
  downCauseTitle,
  focusFallbackRow,
  footerMetrics,
  hardwareLine,
  hiddenRows,
  meterCount,
  needsNotificationsFallback,
  nonServingFooterText,
  pendingStop,
  rtspUrl,
  streamFormats,
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
  // Several clients play one stream: the real count, not "1 connected".
  assert.equal(footerMetrics({ clientConnected: true, clientCount: 3, droppedFrames: 0 }).clients, "3 connected");
  // An appliance that predates the client count reports only whether one is connected.
  assert.equal(footerMetrics({ clientConnected: true, droppedFrames: 0 }).clients, "1 connected");
  assert.equal(footerMetrics({ clientConnected: false, droppedFrames: 0 }).clients, "0 connected");
});

test("clientCountOf reads the count, falling back to the connected flag", () => {
  assert.equal(clientCountOf({ clientConnected: true, clientCount: 4 }), 4);
  assert.equal(clientCountOf({ clientConnected: false, clientCount: 0 }), 0);
  assert.equal(clientCountOf({ clientConnected: true }), 1);
  assert.equal(clientCountOf({ clientConnected: false }), 0);
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
  assert.deepEqual(down, { paths: ["/a", "/b", "/c"], modes: ["opus", "pcm"], connected: 0, clients: 0 });
  assert.equal(clientSummary(down), "0 of 3 connected");

  const serving = streamSummary({ ...flat, clientConnected: true, streams: [
    { path: "/a", clientConnected: true, droppedFrames: 0 },
    { path: "/b", clientConnected: true, droppedFrames: 0 },
    { path: "/c", clientConnected: false, droppedFrames: 0 },
  ] }, cfg);
  assert.equal(clientSummary(serving), "2 of 3 connected");
  // Several clients on one stream do not change "N of M connected" for a device
  // with several streams.
  const busy = streamSummary({ ...flat, clientConnected: true, clientCount: 4, streams: [
    { path: "/a", clientConnected: true, clientCount: 3, droppedFrames: 0 },
    { path: "/b", clientConnected: true, clientCount: 1, droppedFrames: 0 },
    { path: "/c", clientConnected: false, clientCount: 0, droppedFrames: 0 },
  ] }, cfg);
  assert.equal(busy.clients, 4);
  assert.equal(clientSummary(busy), "2 of 3 connected");
});

test("streamSummary falls back to the record without a config", () => {
  const s = streamSummary({ path: "/a", mode: "pcm", clientConnected: true });
  assert.deepEqual(s, { paths: ["/a"], modes: ["pcm"], connected: 1, clients: 1 });
  assert.equal(clientSummary(s), "Connected");
  // A single stream with more than one client shows the count.
  assert.equal(clientSummary(streamSummary({ path: "/a", mode: "pcm", clientConnected: true, clientCount: 2 })), "2 connected");
  assert.equal(clientSummary(streamSummary({ path: "/a", mode: "pcm", clientConnected: true, clientCount: 1 })), "Connected");
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

const shown = (entries: [string, string, string][]) => new Map(entries.map(([id, shape, key]) => [id, { shape, key }]));

test("availablePlan keeps unchanged cards and builds only new ones or ones with other rows", () => {
  const plan = availablePlan(shown([["a", "s", "ka"], ["b", "s", "kb"], ["c", "s", "kc"]]), [
    { id: "a", shape: "s", key: "ka" },
    { id: "b", shape: "t", key: "kb2" },
    { id: "d", shape: "s", key: "kd" },
  ]);
  assert.deepEqual(plan.remove, ["c"]);
  assert.deepEqual(plan.build, ["b", "d"], "only the reshaped and the new card are built");
  assert.deepEqual(plan.patch, [], "a rebuilt card is not also patched");
});

test("availablePlan patches a card whose text changed in place", () => {
  const plan = availablePlan(shown([["a", "s", "ka"], ["b", "s", "kb"]]), [
    { id: "a", shape: "s", key: "ka" },
    { id: "b", shape: "s", key: "kb2" },
  ]);
  assert.deepEqual(plan.patch, ["b"]);
  assert.deepEqual(plan.build, [], "a same-shaped change keeps the node");
});

test("availablePlan builds a new card mid-list and orders it there", () => {
  const plan = availablePlan(shown([["a", "s", "ka"], ["c", "s", "kc"]]), [
    { id: "a", shape: "s", key: "ka" },
    { id: "b", shape: "s", key: "kb" },
    { id: "c", shape: "s", key: "kc" },
  ]);
  assert.deepEqual(plan.build, ["b"]);
  assert.deepEqual(plan.order, ["a", "b", "c"], "a new card is ordered by its place in the list, not appended");
});

test("availablePlan orders cards like the list", () => {
  const plan = availablePlan(shown([["a", "s", "ka"], ["b", "s", "kb"]]), [
    { id: "b", shape: "s", key: "kb" },
    { id: "a", shape: "s", key: "ka" },
  ]);
  assert.deepEqual(plan.order, ["b", "a"]);
  assert.deepEqual(plan.build, [], "a reorder rebuilds nothing");
  assert.deepEqual(plan.patch, []);
  assert.deepEqual(plan.remove, []);
});

test("availablePlan patches a card whose busy state changed", () => {
  const d = { device: "hw:1,0", friendlyName: "USB mic" } as unknown as AvailableDevice;
  const shape = availableCardShape(d);
  // The Enable settled: the same device, no longer in flight.
  const plan = availablePlan(shown([[d.device, shape, availableCardKey(d, true)]]), [{ id: d.device, shape, key: availableCardKey(d, false) }]);
  assert.deepEqual(plan.patch, [d.device], "a card left busy is patched idle");
  assert.deepEqual(plan.build, []);
  assert.notEqual(availableCardKey(d, true), availableCardKey(d, false));
  assert.equal(availableCardKey(d, false), availableCardKey({ ...d }, false), "equal data gives equal keys");
});

test("availableCardShape changes only when an optional row appears or goes", () => {
  const base = { device: "usb-serial-1", hwAddr: "hw:1,0", supportedChannels: [1], supportedRates: [48000] } as unknown as AvailableDevice;
  const shape = availableCardShape(base);
  assert.equal(availableCardShape({ ...base, friendlyName: "Renamed" } as AvailableDevice), shape, "a name is text, not a row");
  assert.equal(availableCardShape({ ...base, supportedRates: [96000] }), shape, "other capability text is the same row");
  assert.notEqual(availableCardShape({ ...base, supportedChannels: [], supportedRates: [] }), shape, "no probe result drops the capabilities row");
  assert.notEqual(availableCardShape({ ...base, idStable: false }), shape, "the no stable ID note is a row");
  assert.notEqual(availableCardShape({ ...base, hwAddr: undefined }), shape, "no address drops the id row");
  assert.notEqual(availableCardShape({ ...base, device: "hw:1,0" }), shape, "an id equal to the address is not shown twice");
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

test("the dashboard follower suspends meters, drops levels and stops the stale check off the dashboard", () => {
  const router = new FakeRouter();
  const calls: string[] = [];
  followDashboardRoute(router, {
    setFramesSuspended: (s) => calls.push(`frames ${s ? "off" : "on"}`),
    setLevelsWanted: (w) => calls.push(`levels ${w ? "on" : "off"}`),
    setWatched: (w) => calls.push(`watch ${w ? "on" : "off"}`),
  });
  router.go("dashboard");
  router.go("system");
  router.go("events");
  router.go("dashboard");
  assert.deepEqual(calls, [
    "frames on", "levels on", "watch on",
    "frames off", "levels off", "watch off",
    "frames off", "levels off", "watch off",
    "frames on", "levels on", "watch on",
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
  h.watch.setWatched(true);
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
  // Levels arrived at 3x: half a window later they are still current.
  h.setClock(LEVELS_STALE_MS * 3 + LEVELS_STALE_MS / 2);
  h.tick();
  assert.equal(h.clears(), 1, "levels() must restart the window, not only allow another clear");
  h.setClock(LEVELS_STALE_MS * 4 + 1);
  h.tick();
  assert.equal(h.clears(), 2, "levels that came back and stopped again clear again");
});

test("LevelsWatch runs its check only while the dashboard shows on a live stream", () => {
  const h = watchHarness();
  h.watch.setConnected(true);
  assert.equal(h.timers.intervals().length, 0, "the dashboard is not showing");
  h.watch.setWatched(true);
  assert.equal(h.timers.intervals().length, 1);
  h.watch.setWatched(false);
  assert.equal(h.timers.intervals().length, 0, "leaving the dashboard stops the check");
  h.watch.setWatched(true);
  // Showing again starts a new window rather than judging the time away.
  h.setClock(LEVELS_STALE_MS * 10);
  h.watch.setWatched(false);
  h.watch.setWatched(true);
  h.tick();
  assert.equal(h.clears(), 0);
});

test("LevelsWatch clears at once when the stream goes down and stops its check", () => {
  const h = watchHarness();
  h.watch.setWatched(true);
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
  h.watch.setWatched(true);
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

test("routeLevels feeds each channel's meter and clears a card the event lacks", () => {
  const calls: string[] = [];
  const cards = [
    { name: "garden", meters: ["g0", "g1"] },
    { name: "bats", meters: ["b0"] },
  ];
  const channels = [
    { channel: 1, rmsDbfs: -20, peakDbfs: -6, clipped: false },
    { channel: 0, rmsDbfs: -30, peakDbfs: -0.05, clipped: true },
    { channel: 5, rmsDbfs: -1, peakDbfs: -1, clipped: false },
  ];
  routeLevels(cards, new Map([["garden", { channels }]]), {
    set: (m, rms, peak, clipped) => calls.push(`${m} ${rms} ${peak} ${clipped}`),
    clear: (m) => calls.push(`${m} clear`),
  });
  assert.deepEqual(
    calls,
    ["g1 -20 -6 false", "g0 -30 -0.05 true", "b0 clear"],
    "each channel's own levels and latch reach its meter; a channel with no meter is skipped; bats is missing, so cleared",
  );
});

test("runtimeEnabled is off only for a disabled device", () => {
  assert.equal(runtimeEnabled("disabled"), false);
  for (const state of ["serving", "failed", "skipped"]) assert.equal(runtimeEnabled(state), true, state);
});

const RUNTIME: Device = {
  name: "Garden",
  device: "usb:0d8c:0014:s=A:if=0,0",
  path: "/garden",
  mode: "opus",
  format: "s16",
  rate: 48000,
  channels: [1],
  state: "failed",
  clientConnected: false,
  droppedFrames: 0,
  opus: { bitrate: 64000 },
};

test("deviceToConfig projects the configured fields and leaves quietAlert out", () => {
  assert.deepEqual(deviceToConfig(RUNTIME), {
    name: "Garden", device: RUNTIME.device, path: "/garden", mode: "opus",
    rate: 48000, channels: [1], format: "s16", enabled: true, opus: { bitrate: 64000 },
  });
  const pcm = deviceToConfig({ ...RUNTIME, mode: "pcm", state: "disabled", opus: undefined });
  assert.equal(pcm.enabled, false);
  assert.equal("opus" in pcm, false);
  assert.equal("quietAlert" in pcm, false);
});

test("deviceConfigKey ignores enabled and reads an absent quietAlert as true", () => {
  const cd: DeviceConfig = { ...deviceToConfig(RUNTIME), quietAlert: true };
  assert.equal(deviceConfigKey(undefined), "");
  assert.equal(deviceConfigKey({ ...cd, enabled: false }), deviceConfigKey(cd));
  assert.equal(deviceConfigKey({ ...cd, quietAlert: undefined }), deviceConfigKey(cd));
  assert.notEqual(deviceConfigKey({ ...cd, quietAlert: false }), deviceConfigKey(cd));
  assert.notEqual(deviceConfigKey({ ...cd, opus: { bitrate: 96000 } }), deviceConfigKey(cd));
  assert.notEqual(deviceConfigKey({ ...cd, channels: [1, 2] }), deviceConfigKey(cd));
  // Every field the settings form edits is in the key.
  const edits: Partial<DeviceConfig>[] = [{ name: "Pond" }, { path: "/pond" }, { mode: "pcm" }, { rate: 96000 }, { format: "s32" as DeviceConfig["format"] }];
  for (const edit of edits) assert.notEqual(deviceConfigKey({ ...cd, ...edit }), deviceConfigKey(cd), JSON.stringify(edit));
  // A config payload without channels does not throw.
  const noChannels = { ...cd } as Partial<DeviceConfig>;
  delete noChannels.channels;
  assert.equal(deviceConfigKey(noChannels as DeviceConfig), deviceConfigKey({ ...cd, channels: [] }));
});

test("pendingStop flags only a serving device the config now disables", () => {
  assert.equal(pendingStop(false, "serving"), true);
  assert.equal(pendingStop(true, "serving"), false);
  assert.equal(pendingStop(false, "failed"), false);
  assert.equal(pendingStop(true, "disabled"), false);
});

test("nonServingFooterText says whether a disabled device is starting", () => {
  assert.equal(nonServingFooterText("disabled", true), "Enabling; this device starts serving shortly.");
  assert.equal(nonServingFooterText("disabled", false), "Streaming is disabled for this device. Enable it to start serving.");
  const excluded = "Excluded from the RTSP stream server. Other active devices continue serving without interruption.";
  assert.equal(nonServingFooterText("failed", true), excluded);
  assert.equal(nonServingFooterText("skipped", false), excluded);
});

test("rtspUrl takes the port from the listen address, 8554 without one", () => {
  assert.equal(rtspUrl("mic.local", ":8555", "/garden"), "rtsp://mic.local:8555/garden");
  assert.equal(rtspUrl("mic.local", "0.0.0.0:8556", "/garden"), "rtsp://mic.local:8556/garden");
  assert.equal(rtspUrl("mic.local", "8557", "/garden"), "rtsp://mic.local:8557/garden");
  assert.equal(rtspUrl("mic.local", undefined, "/garden"), "rtsp://mic.local:8554/garden");
  assert.equal(rtspUrl("mic.local", "", "/garden"), "rtsp://mic.local:8554/garden");
  // The port follows the last colon, as in an IPv6 listen address.
  assert.equal(rtspUrl("mic.local", "[::]:8555", "/garden"), "rtsp://mic.local:8555/garden");
});

test("meterCount and cardShape follow the captured channels of a serving device", () => {
  assert.equal(meterCount({ negotiatedChannels: 2, channels: [1] }), 2);
  assert.equal(meterCount({ channels: [1, 2, 3] }), 3);
  assert.equal(meterCount({ negotiatedChannels: 0, channels: [] }), 1);
  assert.equal(cardShape({ state: "serving", negotiatedChannels: 2, channels: [1] }), "serving:2");
  assert.equal(cardShape({ state: "serving", channels: [1] }), "serving:1");
  for (const state of ["disabled", "failed", "skipped"] as const) {
    assert.equal(cardShape({ state, negotiatedChannels: 2, channels: [1] }), "idle", state);
  }
});

test("avatarLook marks a serving PCM device ultrasonic and a down one as an error", () => {
  assert.deepEqual(avatarLook("serving", "pcm"), { icon: "ultra", color: "var(--ultrasonic-purple)" });
  assert.deepEqual(avatarLook("serving", "opus"), { icon: "mic", color: "" });
  assert.deepEqual(avatarLook("disabled", "pcm"), { icon: "mic", color: "" });
  assert.deepEqual(avatarLook("failed", "opus"), { icon: "error", color: "var(--signal-crit)" });
  assert.deepEqual(avatarLook("skipped", "pcm"), { icon: "error", color: "var(--signal-crit)" });
});

test("hardwareLine shows the address, a distinct model, and a card-index warning", () => {
  const d = { name: "Garden", device: "usb:0d8c:0014:s=A:if=0,0", state: "serving" as const };
  assert.equal(hardwareLine({ ...d, hwAddr: "hw:2,0", friendlyName: "USB Audio Device" }), "ALSA: hw:2,0 · USB Audio Device");
  // A model that only repeats the name, or a blank one, is left out.
  assert.equal(hardwareLine({ ...d, hwAddr: "hw:2,0", friendlyName: " garden " }), "ALSA: hw:2,0");
  assert.equal(hardwareLine({ ...d, hwAddr: "hw:2,0", friendlyName: "  " }), "ALSA: hw:2,0");
  // Unresolved: a serving device shows its configured id, anything else says so.
  assert.equal(hardwareLine({ ...d, device: "hw:1,0" }), "ALSA: hw:1,0");
  assert.equal(hardwareLine({ ...d, state: "skipped" }), "No matching hardware");
  // A device that is down but present shows its address.
  assert.equal(hardwareLine({ ...d, state: "failed", hwAddr: "hw:2,0" }), "ALSA: hw:2,0");
  assert.equal(
    hardwareLine({ ...d, state: "skipped", idStable: false }),
    "No matching hardware · card index (can change after a reboot)",
  );
});

test("availableLabel adds the address to a friendly name, else falls back", () => {
  const device = "usb:2752:0019:s=Y8ZQ2BM1:if=0,0";
  assert.equal(availableLabel({ device, hwAddr: "hw:4,0", friendlyName: "Scarlett 2i2" }), "Scarlett 2i2 (hw:4,0)");
  assert.equal(availableLabel({ device, friendlyName: "Scarlett 2i2" }), "Scarlett 2i2");
  assert.equal(availableLabel({ device, hwAddr: "hw:4,0" }), "hw:4,0");
  assert.equal(availableLabel({ device }), device);
});

test("capsSummary names the channel support and the top rate", () => {
  assert.equal(capsSummary({ supportedChannels: [1, 2], supportedRates: [44100, 48000, 192000] }), "mono/stereo · up to 192 kHz");
  assert.equal(capsSummary({ supportedChannels: [2], supportedRates: [44100] }), "stereo · up to 44.1 kHz");
  assert.equal(capsSummary({ supportedChannels: [1] }), "mono");
  assert.equal(capsSummary({ supportedRates: [384000] }), "up to 384 kHz");
  assert.equal(capsSummary({}), "");
  // Hardware that only offers wider layouts names the counts it found, never "mono".
  assert.equal(capsSummary({ supportedChannels: [4] }), "4 channels");
  assert.equal(capsSummary({ supportedChannels: [8, 4, 6], supportedRates: [48000] }), "4/6/8 channels · up to 48 kHz");
  assert.equal(capsSummary({ supportedChannels: [1, 2, 4] }), "1/2/4 channels");
  assert.equal(capsSummary({ supportedChannels: [2, 4] }), "2/4 channels");
  // Sorted by number, not as text.
  assert.equal(capsSummary({ supportedChannels: [10, 2] }), "2/10 channels");
});

test("streamFormats describes each stream, not the hardware capture", () => {
  const rec = { mode: "opus" as const, rate: 48000, channels: [1, 2], streams: undefined };
  // No config: the record's flat fields describe the one stream. Opus is 48 kHz
  // whatever the capture rate, at the default 128 kbps per channel carried.
  assert.deepEqual(streamFormats({ ...rec, negotiatedRate: 96000 }), ["OPUS 48,000 Hz · 256 kbps"]);
  assert.deepEqual(streamFormats({ ...rec, channels: [1] }), ["OPUS 48,000 Hz · 128 kbps"]);
  assert.deepEqual(streamFormats({ ...rec, opus: { bitrate: 64000 } }), ["OPUS 48,000 Hz · 64 kbps"]);
  // PCM is L16 at the negotiated capture rate.
  assert.deepEqual(streamFormats({ mode: "pcm", rate: 384000, channels: [1] }), ["PCM L16 384,000 Hz · 16-bit"]);
  assert.deepEqual(streamFormats({ mode: "pcm", rate: 192000, negotiatedRate: 384000, channels: [1] }), ["PCM L16 384,000 Hz · 16-bit"]);
  // The config lists every stream, in path order, each with its own settings.
  const cfg = { streams: [
    { path: "/a", mode: "opus" as const, channels: [1], opus: { bitrate: 32000 } },
    { path: "/b", mode: "pcm" as const, channels: [1, 2] },
    { path: "/c", mode: "opus" as const, channels: [1, 2, 3, 4, 5] },
  ] };
  assert.deepEqual(streamFormats(rec, cfg), ["OPUS 48,000 Hz · 32 kbps", "PCM L16 48,000 Hz · 16-bit", "OPUS 48,000 Hz · 510 kbps"]);
  // A bitrate that is not positive selects the default, as the appliance does.
  assert.deepEqual(streamFormats({ ...rec, opus: { bitrate: 0 } }), ["OPUS 48,000 Hz · 256 kbps"]);
  assert.deepEqual(streamFormats({ ...rec, opus: { bitrate: -1 } }), ["OPUS 48,000 Hz · 256 kbps"]);
  // No channels reads as one, as the default's floor does.
  assert.deepEqual(streamFormats({ ...rec, channels: [] }), ["OPUS 48,000 Hz · 128 kbps"]);
  // A fractional kbps keeps its decimals.
  assert.deepEqual(streamFormats({ ...rec, opus: { bitrate: 24500 } }), ["OPUS 48,000 Hz · 24.5 kbps"]);
  // A serving record with more streams than the flat fields describe (no
  // config yet) still yields one line per stream, so the cell stays aligned
  // with the paths.
  const runtime = [0, 1].map((i) => ({ path: `/${i}`, clientConnected: false, droppedFrames: 0 }));
  assert.deepEqual(streamFormats({ ...rec, streams: runtime }), ["OPUS 48,000 Hz · 256 kbps", "Format unknown"]);
  // With a config the lines follow the config, however many streams the record
  // lists: a stale runtime record must not add lines beyond the paths.
  assert.deepEqual(streamFormats({ ...rec, streams: runtime }, { streams: [cfg.streams[0]] as never }), ["OPUS 48,000 Hz · 32 kbps"]);
  // An empty config stream list falls back to the flat fields, as no config does.
  assert.deepEqual(streamFormats(rec, { streams: [] }), ["OPUS 48,000 Hz · 256 kbps"]);
  // No streams and no config: the flat record's one line.
  assert.deepEqual(streamFormats(rec), ["OPUS 48,000 Hz · 256 kbps"]);
});
