// I/O-free logic for the Notifications settings card. No DOM, no fetch: the
// field catalogue, the input-string to patch transform, and the server-error to
// field mapping all live here so they are unit-tested with node:test and no
// browser. The view (views/system.ts) owns the DOM and calls into here.

import type { AudioAlertSettings, HostAlertSettings, NotificationSettings } from "./types.js";

// NotifyGroup is the settings sub-object a field belongs to. It is also the
// nested key in the patch (notifications.audio / notifications.host).
export type NotifyGroup = "audio" | "host";

// NotifyFieldSpec describes one threshold input: where it lives in the settings
// tree (group + camelCase key, matching the TS settings field), how it is
// labelled, the contract's inclusive integer bounds (mirrored onto the input's
// min/max, kept in sync with api/openapi.yaml), and the dotted snake path the
// API returns in a 422 so a validation error can be pinned to the right input.
export interface NotifyFieldSpec {
  group: NotifyGroup;
  key: string;
  label: string;
  min: number;
  max: number;
  hint: string;
  server: string;
}

// NOTIFY_FIELDS is the catalogue the card renders and saves, in display order:
// the five audio-signal thresholds, then the eight host-health thresholds. The
// bounds and defaults mirror AudioAlertSettings / HostAlertSettings in
// api/openapi.yaml; the clear thresholds additionally must sit below their onset
// (the server enforces it and returns the paired path on a violation).
export const NOTIFY_FIELDS: NotifyFieldSpec[] = [
  { group: "audio", key: "quietDbfs", label: "Very-quiet level (dBFS)", min: -99, max: -1,
    hint: "Peak level below which the input counts as very quiet. Default -60.",
    server: "notifications.audio.quiet_dbfs" },
  { group: "audio", key: "quietSeconds", label: "Very-quiet duration (s)", min: 10, max: 86400,
    hint: "Seconds below the level before the very-quiet warning raises. Default 600.",
    server: "notifications.audio.quiet_seconds" },
  { group: "audio", key: "zeroSeconds", label: "No-signal duration (s)", min: 5, max: 3600,
    hint: "Seconds at digital zero before the no-signal warning raises. Default 30.",
    server: "notifications.audio.zero_seconds" },
  { group: "audio", key: "clipPercent", label: "Clipping threshold (%)", min: 1, max: 100,
    hint: "Percent of clipped windows that raises the clipping warning. Default 20.",
    server: "notifications.audio.clip_percent" },
  { group: "audio", key: "clipWindowSeconds", label: "Clipping window (s)", min: 1, max: 600,
    hint: "Window over which clipping is measured. Default 10.",
    server: "notifications.audio.clip_window_seconds" },
  { group: "host", key: "cpuPercent", label: "CPU warning (%)", min: 1, max: 100,
    hint: "CPU usage that raises the warning. Default 90.",
    server: "notifications.host.cpu_percent" },
  { group: "host", key: "cpuClearPercent", label: "CPU clear (%)", min: 1, max: 99,
    hint: "CPU usage below which it clears; must be below the warning. Default 75.",
    server: "notifications.host.cpu_clear_percent" },
  { group: "host", key: "tempCelsius", label: "Temperature warning (C)", min: 30, max: 120,
    hint: "SoC temperature that raises the warning. Default 80.",
    server: "notifications.host.temp_celsius" },
  { group: "host", key: "tempClearCelsius", label: "Temperature clear (C)", min: 1, max: 119,
    hint: "Temperature below which it clears; must be below the warning. Default 75.",
    server: "notifications.host.temp_clear_celsius" },
  { group: "host", key: "diskPercent", label: "Disk warning (%)", min: 1, max: 100,
    hint: "Disk-used percent that raises the warning. Default 90.",
    server: "notifications.host.disk_percent" },
  { group: "host", key: "diskClearPercent", label: "Disk clear (%)", min: 1, max: 99,
    hint: "Disk-used percent below which it clears; must be below the warning. Default 85.",
    server: "notifications.host.disk_clear_percent" },
  { group: "host", key: "memFreePercent", label: "Low memory (% free)", min: 1, max: 90,
    hint: "Available-memory percent below which the warning raises. Default 10.",
    server: "notifications.host.mem_free_percent" },
  { group: "host", key: "memFreeMiB", label: "Low memory (MiB free)", min: 1, max: 65536,
    hint: "Available memory in MiB below which the warning raises. Default 64.",
    server: "notifications.host.mem_free_mib" },
];

// parseThreshold parses a raw input string to an integer, or null when the box
// is blank or does not hold a finite whole number. Out-of-range values still
// parse: the server is the authority on bounds and returns a field-specific 422,
// which the card surfaces on the offending input.
export function parseThreshold(raw: string): number | null {
  const trimmed = raw.trim();
  if (trimmed === "") return null;
  const n = Number(trimmed);
  if (!Number.isFinite(n) || !Number.isInteger(n)) return null;
  return n;
}

// buildNotificationsPatch assembles the block the PATCH sends from the enabled
// flag and the raw string value of every field, keyed by NotifyFieldSpec.key. A
// field that does not parse is omitted so a transiently cleared box leaves that
// threshold unchanged rather than sending garbage; every field is normally
// present because the card is populated from the materialized GET /config.
export function buildNotificationsPatch(
  enabled: boolean,
  values: Record<string, string>,
): NotificationSettings {
  const audio: Record<string, number> = {};
  const host: Record<string, number> = {};
  for (const spec of NOTIFY_FIELDS) {
    const parsed = parseThreshold(values[spec.key] ?? "");
    if (parsed === null) continue;
    (spec.group === "audio" ? audio : host)[spec.key] = parsed;
  }
  const patch: NotificationSettings = { enabled };
  if (Object.keys(audio).length > 0) patch.audio = audio as AudioAlertSettings;
  if (Object.keys(host).length > 0) patch.host = host as HostAlertSettings;
  return patch;
}

// fieldForServerPath maps a 422 validation field path to its spec so the view
// can mark the offending input, or null for a path the card does not own (a
// device or auth error routed elsewhere). Matching is exact on the dotted snake
// path the API emits; the three paired "clear below onset" errors (cpu, temp,
// disk) reuse their clear field's own path, so all three land on the clear input.
export function fieldForServerPath(path: string): NotifyFieldSpec | null {
  return NOTIFY_FIELDS.find((f) => f.server === path) ?? null;
}
