// Pure, DOM-free helpers for the device settings form, split out so they can be
// unit tested with node:test (see web/test/device-settings-core.test.ts).

// Opus bitrate default: 128 kbps for each channel carried, capped at the top of
// the bitrate range Opus supports (510 kbps). Keep in sync with
// config.OpusDefaultBitrate.
export const OPUS_BITRATE_PER_CHANNEL = 128000;
export const OPUS_MAX_BITRATE = 510000;

// Length limits the appliance enforces on a device's name and stream path, in
// characters (Unicode code points). They mirror MaxNameLen and MaxPathLen in
// internal/config/config.go; test/device-settings-core.test.ts reads that file
// and fails if they drift.
export const MAX_NAME_LEN = 128;
export const MAX_PATH_LEN = 128;

// runeLength counts code points, as the server's utf8.RuneCountInString does;
// String.length counts UTF-16 units, which double counts an emoji or any other
// character outside the Basic Multilingual Plane.
export function runeLength(s: string): number {
  return [...s].length;
}

// inputMaxLength is the maxlength attribute for a field limited to maxRunes code
// points. maxlength counts UTF-16 units, and a code point takes at most two, so
// twice the limit can never refuse a valid value while still stopping a runaway
// paste; the exact check is lengthError.
export function inputMaxLength(maxRunes: number): number {
  return maxRunes * 2;
}

// lengthError returns the inline error for a value over maxRunes code points,
// or "" when it fits. A value equal to the stored one is accepted whatever its
// length, matching the server, which only enforces the limit on new values so
// that a config written before the limit existed still saves.
export function lengthError(value: string, stored: string, maxRunes: number, what: string): string {
  if (value === stored || runeLength(value) <= maxRunes) return "";
  return `${what} must be at most ${maxRunes} characters (now ${runeLength(value)}).`;
}

// defaultOpusBitrate is the per-channel default for the given channel count,
// floored at one channel and capped at the Opus ceiling.
export function defaultOpusBitrate(channels: number): number {
  return Math.min(OPUS_BITRATE_PER_CHANNEL * Math.max(1, channels), OPUS_MAX_BITRATE);
}

// bitrateFollowsDefault reports whether a saved Opus bitrate is still the
// per-channel default, so a change to the channel count should move it along
// (128 kbps mono, 256 kbps stereo). An unset bitrate (absent or 0) follows the
// default, and so does a saved value equal to it; a value picked by hand does
// not. Opus carries at most two channels, so the default is seeded from at most
// two even on a device with more capture channels.
export function bitrateFollowsDefault(savedBitrate: number | undefined, channelCount: number): boolean {
  const def = defaultOpusBitrate(Math.min(2, channelCount));
  return (savedBitrate || def) === def;
}
