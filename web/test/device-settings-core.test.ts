// Unit tests for the pure device-settings bitrate helpers. Run with Node's
// built-in test runner over the compiled output (see the web:test task): no
// browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import {
  MAX_NAME_LEN,
  OPUS_BITRATE_PER_CHANNEL,
  OPUS_MAX_BITRATE,
  inputMaxLength,
  lengthError,
  runeLength,
  bitrateFollowsDefault,
  defaultOpusBitrate,
} from "../src/lib/device-settings-core.js";

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
