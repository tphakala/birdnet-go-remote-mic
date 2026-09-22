// One notification rendered as a row, shared by the bell's popover (compact) and
// the Events page (full). Keeping a single renderer means the two surfaces cannot
// drift in how they show severity, chips, or times. The full variant adds an
// absolute timestamp, an unread marker, the condition lifecycle badge, and
// clickable chips that narrow the page's filter.

import { elem, formatRelative, setText } from "../lib/ui.js";
import { TOAST_ICONS, type ToastType } from "./toast.js";
import { formatDuration, type Lifecycle } from "../lib/events-core.js";
import type { Notification, NotificationSeverity } from "../lib/types.js";

// Notification severity maps onto the toast icon set so every surface shows the
// same glyphs (warning uses the "warn" toast icon).
export const SEVERITY_TO_TOAST: Record<NotificationSeverity, ToastType> = {
  error: "error",
  warning: "warn",
  info: "info",
};

// Spoken severity prefix: the icon is aria-hidden and color is not announced, so
// a screen reader would otherwise not hear whether a row is an error or info.
export const SEVERITY_LABEL: Record<NotificationSeverity, string> = {
  error: "Error",
  warning: "Warning",
  info: "Info",
};

// RESTAMP_MS is how often a visible list refreshes its relative "N ago" times and
// ongoing durations. Times are minute-resolution above a minute, so a passive
// list does not need second-accurate updates.
export const RESTAMP_MS = 15_000;

// ChipFacet names the filter a clicked chip narrows.
export type ChipFacet = "category" | "source";

export interface RowOptions {
  nowMs: number;
  // offsetMs corrects server timestamps for browser/server clock skew.
  offsetMs: number;
  // full selects the Events-page layout; omitted renders the compact bell row.
  full?: boolean;
  unread?: boolean;
  lifecycle?: Lifecycle;
  // onChip makes the category and source chips buttons that apply a filter.
  onChip?: (facet: ChipFacet, value: string) => void;
}

// relTime renders one event's ISO timestamp as a relative "N ago" string,
// correcting for server clock skew.
export function relTime(iso: string, offsetMs: number, nowMs: number): string {
  return formatRelative(Date.parse(iso) + offsetMs, nowMs);
}

// absTime renders an event's timestamp in the viewer's locale, skew corrected.
function absTime(iso: string, offsetMs: number): string {
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return iso;
  return new Date(ms + offsetMs).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function fullTimestamp(iso: string, offsetMs: number): string {
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? new Date(ms + offsetMs).toLocaleString() : iso;
}

function chip(label: string, extraClass: string, facet: ChipFacet, value: string, opts: RowOptions): HTMLElement {
  if (!opts.onChip) return elem("span", `notif-chip ${extraClass}`.trim(), label);
  const b = elem("button", `notif-chip notif-chip-btn ${extraClass}`.trim(), label);
  b.setAttribute("type", "button");
  b.setAttribute("aria-label", `Show only ${facet} ${value}`);
  b.title = `Filter by ${facet}`;
  const onChip = opts.onChip;
  b.addEventListener("click", () => onChip(facet, value));
  return b;
}

// lifecycleText is the badge label for a condition entry. An ongoing duration is
// measured on the browser clock from the skew-corrected onset time.
function lifecycleText(lc: Lifecycle, n: Notification, nowMs: number, offsetMs: number): string {
  if (lc.state === "ongoing") {
    const d = formatDuration(nowMs - (lc.sinceMs + offsetMs));
    return d ? `Ongoing ${d}` : "Ongoing";
  }
  if (lc.durationMs === null) return "Resolved";
  const d = formatDuration(lc.durationMs);
  return n.kind === "clear" ? `Resolved after ${d}` : `Lasted ${d}`;
}

function lifecycleBadge(lc: Lifecycle, n: Notification, opts: RowOptions): HTMLElement {
  const cls = lc.state === "ongoing" ? "ev-life ev-life-ongoing" : "ev-life ev-life-resolved";
  const badge = elem("span", cls, lifecycleText(lc, n, opts.nowMs, opts.offsetMs));
  if (lc.state === "ongoing") {
    // Restamped in place while the page stays open; see restampRows.
    badge.dataset.since = String(lc.sinceMs);
  }
  return badge;
}

export function renderNotificationRow(n: Notification, opts: RowOptions): HTMLElement {
  // One lookup feeds both the severity badge colour and the glyph, so a row can
  // never end up with an error colour and an info icon.
  const sev = SEVERITY_TO_TOAST[n.severity] ?? "info";
  const row = elem(opts.full ? "article" : "div", `notif-row sev-${sev}${opts.full ? " ev-row" : ""}`);
  if (opts.unread) row.classList.add("is-unread");
  row.dataset.id = String(n.id);

  const icon = elem("span", "notif-row-icon sev-badge");
  icon.setAttribute("aria-hidden", "true");
  icon.innerHTML = TOAST_ICONS[sev]; // trusted markup

  const main = elem("div", "notif-row-main");

  const top = elem("div", "notif-row-top");
  const titleWrap = elem("div", "notif-row-titlewrap");
  const title = elem(opts.full ? "h3" : "span", "notif-row-title");
  title.append(elem("span", "visually-hidden", `${SEVERITY_LABEL[n.severity]}: `));
  title.append(document.createTextNode(n.title));
  titleWrap.append(title);
  if (opts.unread) {
    const dot = elem("span", "ev-unread-dot");
    dot.title = "Unread";
    dot.append(elem("span", "visually-hidden", "(unread)"));
    titleWrap.append(dot);
  }
  top.append(titleWrap);

  const time = elem("time", "notif-row-time", relTime(n.time, opts.offsetMs, opts.nowMs));
  time.setAttribute("datetime", n.time);
  time.title = fullTimestamp(n.time, opts.offsetMs);
  if (opts.full) {
    const when = elem("div", "ev-when");
    when.append(elem("span", "ev-abs-time mono", absTime(n.time, opts.offsetMs)), time);
    top.append(when);
  } else {
    top.append(time);
  }

  const meta = elem("div", "notif-row-meta");
  meta.append(chip(n.category, "", "category", n.category, opts));
  if (n.source) meta.append(chip(n.source, "notif-chip-source", "source", n.source, opts));
  if (opts.lifecycle) meta.append(lifecycleBadge(opts.lifecycle, n, opts));

  main.append(top, meta);
  if (n.message) main.append(elem("div", "notif-row-msg", n.message));

  row.append(icon, main);
  return row;
}

// restampRows refreshes every relative time and ongoing-duration badge under
// root from its data attributes, so a list left open stays current without a
// full re-render (which would disturb scroll and focus).
export function restampRows(root: ParentNode, offsetMs: number): void {
  const nowMs = Date.now();
  root.querySelectorAll<HTMLElement>("time.notif-row-time").forEach((t) => {
    const iso = t.getAttribute("datetime");
    if (iso) setText(t, relTime(iso, offsetMs, nowMs));
  });
  root.querySelectorAll<HTMLElement>(".ev-life-ongoing[data-since]").forEach((b) => {
    const since = Number(b.dataset.since);
    const d = formatDuration(nowMs - (since + offsetMs));
    setText(b, d ? `Ongoing ${d}` : "Ongoing");
  });
}
