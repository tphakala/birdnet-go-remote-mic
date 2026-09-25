// I/O-free logic behind the Events page: the filter model, faceted counts, the
// onset/clear lifecycle pairing, and small formatters. Like notifications-core it
// has no DOM, fetch, storage or timers, so it is unit-tested with node:test. The
// page reads the same NotificationStore state as the bell but does not hide
// entries in the per-browser dismissed set; they only count as read, so clearing
// the bell tidies the popover without removing an entry from the full event log.

import { anchorToMs, type ClockAnchor } from "./notifications-core.js";
import type { Notification, NotificationSeverity } from "./types.js";

// SEVERITIES and CATEGORIES fix the facet order the page renders, most severe
// first and categories in the order an operator scans an appliance (hardware to
// host). A category the server adds later still filters and counts; it is just
// appended after these.
export const SEVERITIES: readonly NotificationSeverity[] = ["error", "warning", "info"];
export const CATEGORIES: readonly string[] = ["device", "audio", "stream", "system", "config"];

// EventFilter is the page's filter state. An empty severities or categories set
// means "all" for that facet; source null means any subject; query is matched
// case-insensitively against the title, message and source.
export interface EventFilter {
  query: string;
  severities: Set<NotificationSeverity>;
  categories: Set<string>;
  source: string | null;
}

// emptyFilter returns a filter that matches every entry.
export function emptyFilter(): EventFilter {
  return { query: "", severities: new Set(), categories: new Set(), source: null };
}

// isFilterActive reports whether any facet narrows the list, which drives the
// Reset control and the "N of M" wording.
export function isFilterActive(f: EventFilter): boolean {
  return f.query.trim() !== "" || f.severities.size > 0 || f.categories.size > 0 || f.source !== null;
}

// Facet names one filter dimension, so facetCounts can leave a facet's own
// selection out when counting it.
type Facet = "severity" | "category";

function matches(n: Notification, f: EventFilter, skip?: Facet): boolean {
  if (skip !== "severity" && f.severities.size > 0 && !f.severities.has(n.severity)) return false;
  if (skip !== "category" && f.categories.size > 0 && !f.categories.has(n.category)) return false;
  if (f.source !== null && n.source !== f.source) return false;
  const q = f.query.trim().toLowerCase();
  if (q !== "") {
    const hay = `${n.title}\n${n.message}\n${n.source ?? ""}`.toLowerCase();
    if (!hay.includes(q)) return false;
  }
  return true;
}

// filterEvents returns the entries that pass every facet, newest first.
export function filterEvents(items: Iterable<Notification>, f: EventFilter): Notification[] {
  const out: Notification[] = [];
  for (const n of items) if (matches(n, f)) out.push(n);
  return out.sort((a, b) => b.id - a.id);
}

// FacetCounts is the number of entries each chip would show if selected.
export interface FacetCounts {
  severity: Record<NotificationSeverity, number>;
  category: Map<string, number>;
}

// facetCounts counts each facet value under every OTHER active filter, the usual
// faceted-search convention: selecting "error" must not zero the warning and info
// chips, or the operator could never see what widening the filter would add.
export function facetCounts(items: Iterable<Notification>, f: EventFilter): FacetCounts {
  const severity: Record<NotificationSeverity, number> = { error: 0, warning: 0, info: 0 };
  const category = new Map<string, number>();
  for (const c of CATEGORIES) category.set(c, 0);
  for (const n of items) {
    if (matches(n, f, "severity") && n.severity in severity) severity[n.severity]++;
    if (matches(n, f, "category")) category.set(n.category, (category.get(n.category) ?? 0) + 1);
  }
  return { severity, category };
}

// Lifecycle describes where a condition entry sits in its onset/clear pair. An
// ongoing onset carries the server uptime it started at; a resolved pair carries
// its duration, or null when the partner entry is no longer retained (the ring
// trimmed the onset of a long-past clear). Both come from the entries' monotonic
// uptimeMs, never their wall-clock times, so a server clock step between onset
// and clear (an RTC-less Pi syncing NTP) cannot inflate or zero a duration.
export type Lifecycle =
  | { state: "ongoing"; sinceUptimeMs: number }
  | { state: "resolved"; durationMs: number | null };

// conditionLifecycles pairs onset and clear entries by key in id order and
// returns the lifecycle of every condition entry, keyed by id. The server
// alternates onset and clear per key (a duplicate of either is refused), so a
// clear always closes the latest open onset for its key. Discrete events get no
// entry.
export function conditionLifecycles(items: Iterable<Notification>): Map<number, Lifecycle> {
  const sorted = [...items].sort((a, b) => a.id - b.id);
  const open = new Map<string, Notification>();
  const out = new Map<number, Lifecycle>();
  for (const n of sorted) {
    if (!n.key) continue;
    if (n.kind === "onset") {
      open.set(n.key, n);
      out.set(n.id, { state: "ongoing", sinceUptimeMs: n.uptimeMs });
    } else if (n.kind === "clear") {
      const onset = open.get(n.key);
      open.delete(n.key);
      let durationMs: number | null = null;
      if (onset) {
        // Uptime is monotonic within a boot, so a negative span only comes from a
        // malformed entry; clamp it rather than render "-3s".
        const d = n.uptimeMs - onset.uptimeMs;
        durationMs = Number.isFinite(d) ? Math.max(0, d) : null;
      }
      const lc: Lifecycle = { state: "resolved", durationMs };
      out.set(n.id, lc);
      if (onset) out.set(onset.id, lc);
    }
  }
  return out;
}

// formatDuration renders a span as a compact label: seconds under a minute,
// minutes under an hour, then hours with a zero-padded minutes field and days
// with an unpadded hours field.
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${String(m % 60).padStart(2, "0")}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

// exportJSON serializes the entries for download, oldest first so the file reads
// as a chronological log. The caller passes the export instant so this stays
// clock-free.
//
// Each entry's own time is the appliance wall clock when it was raised, which
// on a Pi without an RTC can be the pre-NTP clock. The export therefore also
// carries the clock anchor the page places entries with (the browser time at
// which the appliance's uptime was anchor.uptimeMs), and each event an
// anchoredTime derived from it and the entry's uptimeMs, the same corrected
// time the page shows. Both are null before any anchor exists.
export function exportJSON(
  items: readonly Notification[],
  bootId: string | null,
  exportedAt: string,
  anchor: ClockAnchor | null = null,
): string {
  const toIso = (ms: number): string | null => (Number.isFinite(ms) ? new Date(ms).toISOString() : null);
  const clockAnchor = anchor ? { browserTime: toIso(anchor.browserMs), uptimeMs: anchor.uptimeMs } : null;
  const events = [...items]
    .sort((a, b) => a.id - b.id)
    .map((n) => ({
      ...n,
      anchoredTime: toIso(anchorToMs(anchor, n.uptimeMs)),
    }));
  return JSON.stringify({ bootId, exportedAt, clockAnchor, events }, null, 2);
}

// dayKey buckets an instant by the viewer's local calendar day, as a local
// YYYY-MM-DD string (getMonth is zero-based, hence the +1).
export function dayKey(ms: number): string {
  const d = new Date(ms);
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${mm}-${dd}`;
}

// Constructing an Intl formatter costs far more than formatting with one, and
// the Events page formats day labels and the oldest-entry caption on every
// render, so the formatters are built once (as notification-row.ts does).
const DAY_LABEL_FMT = new Intl.DateTimeFormat(undefined, { weekday: "long", month: "short", day: "numeric" });
const CAPTION_TIME_FMT = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" });

// dayLabel names the local day an instant falls on, relative to nowMs: "Today",
// "Yesterday", or the weekday and date.
export function dayLabel(ms: number, nowMs: number): string {
  const key = dayKey(ms);
  if (key === dayKey(nowMs)) return "Today";
  // Compute yesterday by decrementing the calendar date, not by subtracting 24h:
  // around a DST transition a day is 23 or 25 hours, so now minus 86_400_000 can
  // land on today or two days back.
  const y = new Date(nowMs);
  y.setDate(y.getDate() - 1);
  if (key === dayKey(y.getTime())) return "Yesterday";
  return DAY_LABEL_FMT.format(ms);
}

// oldestCaption is the Retained tile's caption for the oldest entry's instant:
// the clock time alone when it is from today, else prefixed with its day label.
// An unknown instant yields "".
export function oldestCaption(ms: number, nowMs: number): string {
  if (!Number.isFinite(ms)) return "";
  const time = CAPTION_TIME_FMT.format(ms);
  return dayKey(ms) === dayKey(nowMs) ? `Oldest ${time}` : `Oldest ${dayLabel(ms, nowMs)} ${time}`;
}

// resultCountLabel is the filter status line: nothing while loading, "N of M"
// while a filter narrows the list, else the plain count, pluralized.
export function resultCountLabel(shown: number, total: number, filterActive: boolean, loading: boolean): string {
  if (loading) return "";
  const noun = total === 1 ? "event" : "events";
  return filterActive ? `Showing ${shown} of ${total} ${noun}` : `${total} ${noun}`;
}
