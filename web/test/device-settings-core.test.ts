// Unit tests for the pure device-settings bitrate helpers. Run with Node's
// built-in test runner (see the web:test task): no browser, no DOM, no
// dependencies.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  DEVICE_FIELD_LABELS,
  MAX_NAME_LEN,
  MAX_PATH_LEN,
  OPUS_BITRATE_PER_CHANNEL,
  OPUS_MAX_BITRATE,
  inputMaxLength,
  lengthError,
  runeLength,
  bitrateFollowsDefault,
  defaultOpusBitrate,
  extraStreamsNote,
  otherOpusStream,
  withFirstStream,
} from "../src/lib/device-settings-core.ts";
import { group } from "./fixtures.ts";
import type { DeviceConfig } from "../src/lib/types.ts";

test("defaultOpusBitrate scales per channel, floors at one, and caps at the Opus ceiling", () => {
  assert.equal(defaultOpusBitrate(1), OPUS_BITRATE_PER_CHANNEL); // 128000
  assert.equal(defaultOpusBitrate(2), OPUS_BITRATE_PER_CHANNEL * 2); // 256000
  assert.equal(defaultOpusBitrate(0), OPUS_BITRATE_PER_CHANNEL); // floored at one channel
  assert.equal(defaultOpusBitrate(-3), OPUS_BITRATE_PER_CHANNEL); // floored at one channel
  assert.equal(defaultOpusBitrate(100), OPUS_MAX_BITRATE); // capped at the Opus ceiling
});

test("bitrateFollowsDefault treats unset/zero and the exact per-channel default as following", () => {
  // Unset or zero always follows the default.
  assert.equal(bitrateFollowsDefault(undefined, 1), true);
  assert.equal(bitrateFollowsDefault(0, 2), true);
  // The exact default for the channel count (Opus capped at two) follows.
  assert.equal(bitrateFollowsDefault(128000, 1), true);
  assert.equal(bitrateFollowsDefault(256000, 2), true);
  // More than two capture channels still seeds the default from two.
  assert.equal(bitrateFollowsDefault(256000, 4), true);
  // A hand-picked value that differs from the default does not follow.
  assert.equal(bitrateFollowsDefault(192000, 2), false);
  assert.equal(bitrateFollowsDefault(128000, 2), false);
});

test("runeLength counts code points, not UTF-16 units", () => {
  assert.equal(runeLength("garden"), 6);
  assert.equal(runeLength("pöllö"), 5);
  // Each bird emoji is one code point but two UTF-16 units.
  const birds = "\u{1F426}\u{1F426}";
  assert.equal(birds.length, 4);
  assert.equal(runeLength(birds), 2);
});

test("lengthError accepts up to the limit in code points and names the overflow", () => {
  const atLimit = "\u{1F426}".repeat(MAX_NAME_LEN);
  // Twice the limit in UTF-16 units, but exactly the limit in characters, and
  // within the maxlength attribute the input carries.
  assert.equal(lengthError(atLimit, "", MAX_NAME_LEN, "Name"), "");
  assert.ok(atLimit.length <= inputMaxLength(MAX_NAME_LEN));
  const over = "a".repeat(MAX_NAME_LEN + 1);
  assert.equal(
    lengthError(over, "", MAX_NAME_LEN, "Name"),
    `Name must be at most ${MAX_NAME_LEN} characters (now ${MAX_NAME_LEN + 1}).`,
  );
});

test("lengthError accepts an unchanged stored value over the limit, as the server does", () => {
  const stored = "b".repeat(MAX_NAME_LEN + 10);
  assert.equal(lengthError(stored, stored, MAX_NAME_LEN, "Name"), "");
  assert.ok(lengthError(`${stored}x`, stored, MAX_NAME_LEN, "Name") !== "");
});

test("lengthError enforces the stream path limit", () => {
  const atLimit = `/${"p".repeat(MAX_PATH_LEN - 1)}`;
  assert.equal(lengthError(atLimit, "", MAX_PATH_LEN, "Path"), "");
  assert.ok(atLimit.length <= inputMaxLength(MAX_PATH_LEN));
  assert.equal(
    lengthError(`${atLimit}x`, "", MAX_PATH_LEN, "Path"),
    `Path must be at most ${MAX_PATH_LEN} characters (now ${MAX_PATH_LEN + 1}).`,
  );
});

// Resolve from import.meta.url rather than process.cwd(), so the test finds
// the file whatever directory the runner is invoked from.
const CONFIG_GO = fileURLToPath(new URL("../../internal/config/config.go", import.meta.url));

// goConst reads an integer constant from the Go config source, so the UI limits
// cannot drift from the ones the appliance enforces.
function goConst(src: string, name: string): number {
  const m = new RegExp(`^\\s*${name}\\s*=\\s*(\\d+)\\s*$`, "m").exec(src);
  assert.ok(m, `${name} not found in internal/config/config.go`);
  return Number(m[1]);
}

test("the name and path limits match internal/config MaxNameLen and MaxPathLen", () => {
  const src = readFileSync(CONFIG_GO, "utf8");
  assert.equal(MAX_NAME_LEN, goConst(src, "MaxNameLen"));
  assert.equal(MAX_PATH_LEN, goConst(src, "MaxPathLen"));
});

test("withFirstStream sends a single-stream device flat", () => {
  const edited: DeviceConfig = { name: "a", device: "hw:1,0", path: "/a", mode: "pcm", rate: 48000, channels: [1], format: "s16" };
  assert.deepEqual(withFirstStream(edited, undefined), edited);
  assert.deepEqual(withFirstStream(edited, [{ path: "/old", mode: "opus", channels: [1] }]), edited);
});

test("withFirstStream puts the edit in streams[0] and keeps the others", () => {
  const edited: DeviceConfig = {
    name: "a", device: "hw:1,0", path: "/new", mode: "opus", rate: 48000, channels: [2], format: "s16", opus: { bitrate: 0 },
  };
  const second = { path: "/b", mode: "pcm" as const, channels: [1, 2] };
  const got = withFirstStream(edited, [{ path: "/old", mode: "pcm", channels: [1] }, second]);
  assert.deepEqual(got.streams, [{ path: "/new", mode: "opus", channels: [2], opus: { bitrate: 0 } }, second]);
  // The flat fields stay too, mirroring streams[0] as the API does.
  assert.equal(got.path, "/new");
});

test("withFirstStream keeps every other stream exactly, and an edit without opus adds none", () => {
  const edited: DeviceConfig = { name: "a", device: "hw:1,0", path: "/a", mode: "pcm", rate: 48000, channels: [1, 2], format: "s16" };
  const second = { path: "/b", mode: "opus" as const, channels: [1], opus: { bitrate: 64000 } };
  const third = { path: "/c", mode: "pcm" as const, channels: [2] };
  const got = withFirstStream(edited, [{ path: "/a", mode: "opus", channels: [1], opus: { bitrate: 96000 } }, second, third]);
  assert.deepEqual(got.streams, [{ path: "/a", mode: "pcm", channels: [1, 2] }, second, third]);
});

test("withFirstStream takes the other streams from the list it is given, not the edit", () => {
  // The save passes the config current at save time; a stream added there
  // since the form opened is kept, and one removed there stays removed.
  // The edit carries a stale streams list, which must not win.
  const edited: DeviceConfig = {
    name: "a", device: "hw:1,0", path: "/a", mode: "pcm", rate: 48000, channels: [1], format: "s16",
    streams: [{ path: "/a", mode: "pcm", channels: [1] }, { path: "/stale", mode: "pcm", channels: [2] }],
  };
  const now = [{ path: "/a", mode: "pcm" as const, channels: [1] }, { path: "/new", mode: "pcm" as const, channels: [2] }];
  assert.deepEqual(withFirstStream(edited, now).streams?.map((st) => st.path), ["/a", "/new"]);
  assert.equal(withFirstStream(edited, now.slice(0, 1)).streams, undefined, "down to one stream, the edit goes flat");
});

test("otherOpusStream looks only past the first stream", () => {
  const pcm = { path: "/a", mode: "pcm" as const, channels: [1] };
  const opus = { path: "/b", mode: "opus" as const, channels: [1] };
  assert.equal(otherOpusStream(undefined), false);
  assert.equal(otherOpusStream([opus]), false, "the first stream is the form's own");
  assert.equal(otherOpusStream([opus, pcm]), false);
  assert.equal(otherOpusStream([pcm, pcm, opus]), true);
});

test("extraStreamsNote speaks only for a multi-stream device", () => {
  assert.equal(extraStreamsNote(undefined), "");
  assert.equal(extraStreamsNote([{ path: "/a", mode: "pcm", channels: [1] }]), "");
  const note = extraStreamsNote([{ path: "/a", mode: "pcm", channels: [1] }, { path: "/b", mode: "pcm", channels: [2] }]);
  assert.ok(note.includes("serves 2 streams"), note);
  assert.ok(note.includes("Opus bitrate"), note);
});

test("every device config field the appliance validates has a form label", () => {
  // The rejection toast names a field by the last part of its path; config.go
  // builds the paths with field("x") and sfield("x").
  const src = readFileSync(CONFIG_GO, "utf8");
  const keys = new Set([...src.matchAll(/\bs?field\("([a-z_.]+)"\)/g)].map((m) => group(m, 1).split(".").at(-1) ?? ""));
  assert.ok(keys.size >= 5, `found only ${keys.size} field keys in config.go; did the helpers change?`);
  const labels: Readonly<Record<string, string>> = DEVICE_FIELD_LABELS;
  for (const key of keys) assert.ok(labels[key], `config.go validates "${key}" but DEVICE_FIELD_LABELS has no label for it`);
});
