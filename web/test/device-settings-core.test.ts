// Unit tests for the pure device-settings bitrate helpers. Run with Node's
// built-in test runner over the compiled output (see the web:test task): no
// browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import {
  OPUS_BITRATE_PER_CHANNEL,
  OPUS_MAX_BITRATE,
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
