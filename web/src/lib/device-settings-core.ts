// Pure, DOM-free helpers for the device settings form, split out so they can be
// unit tested with node:test (see web/test/device-settings-core.test.ts).

// Opus bitrate default: 128 kbps for each channel carried, capped at the top of
// the bitrate range Opus supports (510 kbps). Keep in sync with
// config.OpusDefaultBitrate.
export const OPUS_BITRATE_PER_CHANNEL = 128000;
export const OPUS_MAX_BITRATE = 510000;

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
