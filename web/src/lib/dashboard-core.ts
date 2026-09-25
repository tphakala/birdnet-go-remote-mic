// Pure, DOM-free helpers for the dashboard view, split out so they can be unit
// tested with node:test (see web/test/dashboard-core.test.ts) without a DOM.

// channelLabel renders a streamed channel selection, e.g. "Ch 1", "Ch 1+2", or
// "Ch 1+3" for a non-contiguous pair. An empty selection renders nothing.
export function channelLabel(channels: number[]): string {
  if (!channels.length) return "";
  if (channels.length === 1) return `Ch ${channels[0]}`;
  return "Ch " + channels.join("+");
}

// tallyStates maps the streamed channel numbers (1-based) to a per-row on/off
// flag for a device's meter console, which has one row per captured hardware
// channel indexed from zero. Row i (hardware channel i+1) is on when a stream
// carries that channel, so the tally lights the streamed channels and the idle
// captured channels stay dim.
export function tallyStates(streamed: number[], rowCount: number): boolean[] {
  const set = new Set(streamed);
  const out: boolean[] = [];
  for (let i = 0; i < rowCount; i++) out.push(set.has(i + 1));
  return out;
}

// captureFormatLabel renders a negotiated hardware capture format token as the
// bit depth an operator reads on a device row. The management API reports one of
// s16, s24_le, s24_3le or s32; the stream is always S16LE, so a wider capture
// (24- or 32-bit) is downconverted, which this surfaces. f32 is mapped ahead of
// the capture layer ever negotiating it, and any other token falls back to its
// uppercased form so a future format still shows.
export function captureFormatLabel(token: string): string {
  switch (token) {
    case "s16":
      return "16-bit";
    case "s24_le":
    case "s24_3le":
      return "24-bit";
    case "s32":
      return "32-bit";
    case "f32":
      return "32-bit float";
    default:
      return token.toUpperCase();
  }
}

// downCauseTitle is the banner title for a device that is not serving, matching
// the title of the notification the appliance raises for the same cause. An
// absent or unknown cause (an older appliance, or a class added later) keeps the
// generic title.
export function downCauseTitle(cause: string | undefined): string {
  switch (cause) {
    case "not-connected":
      return "Device not connected";
    case "ambiguous":
      return "Device ambiguous";
    case "malformed":
      return "Invalid device id";
    case "same-hardware":
      return "Device conflict";
    case "resolve-failed":
    case "open-failed":
      return "Device unavailable";
    case "disconnected":
      return "Device disconnected";
    case "failed":
      return "Device failed";
    default:
      return "Device excluded from streaming";
  }
}
