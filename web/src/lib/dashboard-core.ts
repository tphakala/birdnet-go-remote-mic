// Pure, DOM-free helpers for the dashboard view, the device rows of the System
// view and the app's notifications fallback, split out so they can be unit tested with node:test (see
// web/test/dashboard-core.test.ts) without a DOM.

import type { FieldProblem } from "./api.ts";
import { DEVICE_FIELD_LABELS } from "./device-settings-core.ts";
import type { AvailableDevice, Device, DeviceConfig, StreamMode } from "./types.ts";

// channelLabel renders a streamed channel selection, e.g. "Ch 1", "Ch 1+2", or
// "Ch 1+3" for a non-contiguous pair. An empty selection renders nothing.
export function channelLabel(channels: number[]): string {
  if (!channels.length) return "";
  if (channels.length === 1) return `Ch ${channels[0]}`;
  return "Ch " + channels.join("+");
}

// StreamSummary is one device's streams as the System view's Stream Status table
// lists them: every path, the distinct modes in stream order, and how many
// streams have a client.
export interface StreamSummary {
  paths: string[];
  modes: StreamMode[];
  connected: number;
}

// streamSummary combines a device record with its config. The config lists every
// stream even while the device is down; the record's streams array exists only
// while serving, and its flat fields cover the first stream alone (an older
// appliance, or before GET /config loads).
export function streamSummary(
  d: Pick<Device, "path" | "mode" | "clientConnected" | "streams">,
  cfg?: Pick<DeviceConfig, "streams">,
): StreamSummary {
  const configured = cfg?.streams ?? [];
  const paths = configured.length > 0 ? configured.map((s) => s.path) : d.streams?.map((s) => s.path) ?? [d.path];
  const modes = configured.length > 0 ? [...new Set(configured.map((s) => s.mode))] : [d.mode];
  const connected = d.streams ? d.streams.filter((s) => s.clientConnected).length : d.clientConnected ? 1 : 0;
  return { paths, modes, connected };
}

// clientSummary is the Client cell: a single stream says Connected or "-", and a
// device with several says how many of them have a client.
export function clientSummary(s: StreamSummary): string {
  if (s.paths.length > 1) return `${s.connected} of ${s.paths.length} connected`;
  return s.connected > 0 ? "Connected" : "-";
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

// needsNotificationsFallback reports whether the app should load the
// notifications snapshot directly once the post-boot or post-login grace has
// passed: when none has loaded, or when the stream is still down (a previous
// session's snapshot is not fresh, and only a connected stream re-syncs it).
export function needsNotificationsFallback(hasLoaded: boolean, connected: boolean): boolean {
  return !hasLoaded || !connected;
}

// bannerIsError reports whether a non-serving device's banner shows the error
// icon rather than the warning one. A failed device does, except one that was
// unplugged: it is waiting to be reconnected, not broken.
export function bannerIsError(state: string, cause: string | undefined): boolean {
  return state === "failed" && cause !== "disconnected";
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

// FooterMetrics is the text of a serving device card's footer counters.
export interface FooterMetrics {
  clients: string;
  dropped: string;
  overruns: string;
}

// footerMetrics formats a serving device's footer counters. An appliance that
// predates the overrun counter omits it, which reads as zero.
export function footerMetrics(d: { clientConnected: boolean; droppedFrames: number; overruns?: number }): FooterMetrics {
  return {
    clients: d.clientConnected ? "1 connected" : "0 connected",
    dropped: String(d.droppedFrames),
    overruns: String(d.overruns ?? 0),
  };
}

// hiddenRows decides which meter rows the per-device "hide inactive channels"
// preference hides: the rows no stream carries (states[i] false). It never
// hides every row: if no row is streamed (a channel-count/selection mismatch),
// all rows show rather than leave an empty meter console.
export function hiddenRows(states: boolean[], hideInactive: boolean): boolean[] {
  const anyLive = states.some((s) => s);
  return states.map((on) => hideInactive && anyLive && !on);
}

// focusFallbackRow picks where keyboard focus goes when the row holding it is
// hidden: the first row still visible, or -1 when none is (the caller then
// falls back to the card's settings button).
export function focusFallbackRow(hidden: boolean[]): number {
  return hidden.indexOf(false);
}

// Polite announcements for a focus move the operator did not make, so a screen
// reader user learns why focus jumped rather than finding it somewhere new.
// channelHiddenMessage names the 1-based channel whose row hid (because its
// channel left the stream, or another tab turned on hiding inactive channels,
// so the cause is left neutral) and where focus actually landed: the target
// channel's row, or the device settings when target is null.
export function channelHiddenMessage(hidden: number, target: number | null): string {
  const where = target === null ? "the device settings" : `channel ${target}`;
  return `Channel ${hidden} is hidden. Focus moved to ${where}.`;
}

// tokenHiddenMessage is said when a device's Token tag, which held focus, hides
// because access control was turned off.
export function tokenHiddenMessage(device: string): string {
  return `${device} no longer needs the access token. Focus moved to its device settings.`;
}

// controlName names a card control by its data-focus key (copy, token, clip-N
// with N the zero-based channel row), for the message below.
export function controlName(key: string): string {
  if (key === "copy") return "Copy URL button";
  if (key === "token") return "Token tag";
  const clip = /^clip-(\d+)$/.exec(key);
  if (clip) return `channel ${Number(clip[1]) + 1} clip button`;
  return "control";
}

// controlGoneMessage is said when a card rebuilds into a shape without the
// control that held focus (a copy or clip button, or the token tag, after a
// flip from serving to idle); a token tag hidden because the token is no
// longer required gets tokenHiddenMessage instead. It names the device, the
// control, and why it went.
export function controlGoneMessage(device: string, key: string, serving: boolean): string {
  const why = serving ? "is no longer shown" : "went away because the device stopped streaming";
  return `${device}: the ${controlName(key)} ${why}. Focus moved to its device settings.`;
}

// Said after a device is removed, following the "Removed" toast: its card and
// the Remove button that held focus are gone, so focus moved to the dashboard.
export const REMOVED_FOCUS_MESSAGE = "Focus moved to the dashboard.";

// availableCardKey is what an Available Devices card shows: the device's data
// and whether an Enable is in flight for it. A card is rebuilt only when this
// changes. A render during an Enable therefore builds the card busy, and the
// render after it settles builds it idle again.
export function availableCardKey(d: AvailableDevice, enabling: boolean): string {
  return JSON.stringify([d, enabling]);
}

// AvailablePlan is how to turn the cards on screen into the ones for a new
// list: which to remove, which to build (new, or changed since shown), and the
// order to show them in.
export interface AvailablePlan {
  remove: string[];
  build: string[];
  order: string[];
}

// availablePlan diffs the shown cards (device id to card key) against the
// next list, so an unchanged card keeps its node, and with it the operator's
// focus and text selection.
export function availablePlan(shown: ReadonlyMap<string, string>, next: readonly { id: string; key: string }[]): AvailablePlan {
  const nextIds = new Set(next.map((c) => c.id));
  return {
    remove: [...shown.keys()].filter((id) => !nextIds.has(id)),
    build: next.filter((c) => shown.get(c.id) !== c.key).map((c) => c.id),
    order: next.map((c) => c.id),
  };
}

// availableGoneMessage is announced when a poll takes away the Available
// Devices card that held keyboard focus: which device went, and where focus
// went when no other card was left to take it.
export function availableGoneMessage(label: string, toNeighbour: boolean): string {
  return toNeighbour ? `${label} is no longer available.` : `${label} is no longer available. ${REMOVED_FOCUS_MESSAGE}`;
}

// neighbourOrder lists where focus goes, in order of preference, when the
// item at index of order (the ids on screen before a render) goes away: the
// items after it, nearest first, then those before it, nearest first. The
// caller takes the first one still on screen, so several items going at once
// still leave focus next to where it was.
export function neighbourOrder(order: readonly string[], index: number): string[] {
  if (index < 0) return [...order];
  return [...order.slice(index + 1), ...order.slice(0, index).reverse()];
}

// settingsFocusMessage is said when focus moves to a device card's settings
// button the operator did not choose: after an Enable, or when the card that
// held focus went away.
export function settingsFocusMessage(name: string): string {
  return `Focus moved to ${name} settings.`;
}

// deviceGoneMessage is said when a render removes the configured device card
// that held keyboard focus (another tab removed it, or a config reload
// dropped it): which device went, and where focus went (the next card's
// settings, or the dashboard when no card is left).
export function deviceGoneMessage(name: string, next: string | null): string {
  return `${name} was removed. ${next === null ? REMOVED_FOCUS_MESSAGE : settingsFocusMessage(next)}`;
}

// deviceFieldLabel turns a validation problem's field path into what an
// operator reads: the field's label (without a unit in parentheses, as it
// reads mid-sentence), and for a path into the device list the name of the
// device it points at (names is the device list the request sent, in order),
// since a save sends every device. A field of a stream after the first names
// that stream, since the form edits only the first. It returns the path
// itself for one it does not know. The appliance also reports the list as a
// whole ("devices", config.go:591) and a provision's device id ("device",
// mgmtserver/devices.go:182).
export function deviceFieldLabel(path: string, names: readonly string[] = []): string {
  if (path === "devices") return "The device list";
  if (path === "device") return DEVICE_FIELD_LABELS.device;
  const f = parseDeviceFieldPath(path);
  if (!f) return path;
  const label = DEVICE_FIELD_LABELS[f.key].replace(/ \([^)]*\)$/, "");
  const name = names[f.device];
  const owner = f.stream > 0 ? (name ? `${name}'s stream ${f.stream + 1}` : `stream ${f.stream + 1}`) : name;
  return owner ? `${label} of ${owner}` : label;
}

// DeviceFieldPath is a validation path into the device list: the device's
// index in the list the request sent, the stream's index (0 for a device
// field), and the field's key in DEVICE_FIELD_LABELS.
export interface DeviceFieldPath {
  device: number;
  stream: number;
  key: keyof typeof DEVICE_FIELD_LABELS;
}

// parseDeviceFieldPath reads devices[i].<key>, devices[i].streams[j].<key>
// and devices[i].streams[j].opus.bitrate, or returns null for any other path
// or a key with no label.
export function parseDeviceFieldPath(path: string): DeviceFieldPath | null {
  const m = /^devices\[(\d+)\]\.(?:streams\[(\d+)\]\.)?(?:opus\.)?([a-z_]+)$/.exec(path);
  const key = m?.[3];
  if (!m || key === undefined || !Object.hasOwn(DEVICE_FIELD_LABELS, key)) return null;
  return { device: Number(m[1]), stream: Number(m[2] ?? 0), key: key as keyof typeof DEVICE_FIELD_LABELS };
}

// rejectionText is the toast for a request the appliance refused with a
// validation problem: the action that failed, the field by its form label and
// the device by its name in names (the device list the request sent), and the
// reason: "Save failed: RTSP Path of garden was rejected: ...".
export function rejectionText(prefix: string, problem: FieldProblem, names: readonly string[]): string {
  const what = problem.field ? deviceFieldLabel(problem.field, names) : "the configuration";
  return `${prefix}: ${what} was rejected: ${problem.reason}`;
}

// rejectedFieldKey is the settings form field to mark for a validation
// problem on a save of the device edited (its id), sent being the device list
// the save sent: the problem's own field when it points at that device's
// first stream, the one the form edits. A duplicate name or path is reported
// at its later occurrence (internal/config/config.go:625 and :682), which may
// be another device or stream, so a duplicate of the edited device's name or
// path marks that field too. It returns null for a problem that is not the
// edited device's; which keys the form can show is markRejected's call.
export function rejectedFieldKey(field: string, sent: readonly DeviceConfig[], edited: string): keyof typeof DEVICE_FIELD_LABELS | null {
  const at = parseDeviceFieldPath(field);
  const own = sent.find((d) => d.device === edited);
  if (!at || !own) return null;
  const there = sent[at.device];
  if (there?.device === edited && at.stream === 0) return at.key;
  if (at.key === "name" && there?.name === own.name) return "name";
  if (at.key === "path") {
    const path = there?.streams?.[at.stream]?.path ?? (at.stream === 0 ? there?.path : undefined);
    if (path !== undefined && path === (own.streams?.[0]?.path ?? own.path)) return "path";
  }
  return null;
}
