// One notification rendered as a row, shared by the bell's popover (compact) and
// the Events page (full). Keeping a single renderer means the two surfaces cannot
// drift in how they show severity, chips, or times. The full variant renders as an
// <article> with a heading title (h3 by default, or the tag given by headingLevel)
// and an absolute timestamp; the unread marker, the lifecycle badge, and clickable
// filter chips are independent opt-in options (unread, lifecycle, onChip) that the
// Events page turns on.
//
// Times come from each entry's server uptime, mapped onto the browser clock by
// the caller's toMs (see uptimeToMs in notifications-core), not from its
// wall-clock time: that corrects browser/server skew and survives a server clock
// step. Each row keeps its uptime in a data attribute so restampRows can re-map
// it after a later re-sync moves the anchor.

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

// RESTAMP_MS is how often a visible list refreshes its relative "N ago" times, its
// absolute times and tooltips, and ongoing durations. Times are minute-resolution
// above a minute, so a passive list does not need second-accurate updates.
export const RESTAMP_MS = 15_000;

// ChipFacet names the filter a clicked chip narrows.
export type ChipFacet = "category" | "source";

// UptimeToMs maps a server uptime onto the browser clock (NaN when unknown).
export type UptimeToMs = (uptimeMs: number) => number;

export interface RowOptions {
  nowMs: number;
  toMs: UptimeToMs;
  // full selects the Events-page layout; omitted renders the compact bell row.
  full?: boolean;
  // headingLevel picks the title's heading tag for a full row (default h3), so a
  // caller can nest it correctly under its own headings. Ignored for compact rows.
  headingLevel?: "h3" | "h4";
  unread?: boolean;
  lifecycle?: Lifecycle;
  // onChip makes the category and source chips buttons that apply a filter.
  onChip?: (facet: ChipFacet, value: string) => void;
}

// Constructing an Intl formatter is far costlier than formatting with one, and
// restampRows runs both over every row on each tick, so hoist them to module
// scope and reuse them. ABS_TIME_FMT matches the old toLocaleTimeString options;
// FULL_FMT's explicit fields match toLocaleString's numeric default.
const ABS_TIME_FMT = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
const FULL_FMT = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", second: "numeric" });

// relTime renders an instant as a relative "N ago" string.
function relTime(ms: number, nowMs: number): string {
  return Number.isFinite(ms) ? formatRelative(ms, nowMs) : "";
}

// absTime renders an instant as a clock time in the viewer's locale.
function absTime(ms: number): string {
  return Number.isFinite(ms) ? ABS_TIME_FMT.format(ms) : "";
}

function fullTimestamp(ms: number): string {
  return Number.isFinite(ms) ? FULL_FMT.format(ms) : "Unknown time";
}

function isoTime(ms: number): string {
  return Number.isFinite(ms) ? new Date(ms).toISOString() : "";
}

// setDatetime writes a <time> element's machine-readable value, diffed so an
// unchanged time does not churn the DOM. An unknown time removes the attribute:
// an empty datetime is not a valid value.
function setDatetime(t: HTMLElement, iso: string): void {
  if (iso === "") t.removeAttribute("datetime");
  else if (t.getAttribute("datetime") !== iso) t.setAttribute("datetime", iso);
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

// ongoingText is the badge label for a condition still in effect, measured on the
// browser clock from the onset's mapped uptime.
function ongoingText(sinceUptimeMs: number, nowMs: number, toMs: UptimeToMs): string {
  const d = formatDuration(nowMs - toMs(sinceUptimeMs));
  return d ? `Ongoing ${d}` : "Ongoing";
}

// lifecycleText is the badge label for a condition entry.
function lifecycleText(lc: Lifecycle, n: Notification, opts: RowOptions): string {
  if (lc.state === "ongoing") return ongoingText(lc.sinceUptimeMs, opts.nowMs, opts.toMs);
  if (lc.durationMs === null) return "Resolved";
  const d = formatDuration(lc.durationMs);
  return n.kind === "clear" ? `Resolved after ${d}` : `Lasted ${d}`;
}

function lifecycleBadge(lc: Lifecycle, n: Notification, opts: RowOptions): HTMLElement {
  const cls = lc.state === "ongoing" ? "ev-life ev-life-ongoing" : "ev-life ev-life-resolved";
  const badge = elem("span", cls, lifecycleText(lc, n, opts));
  if (lc.state === "ongoing") {
    // Restamped in place while the page stays open; see restampRows.
    badge.dataset.since = String(lc.sinceUptimeMs);
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
  icon.innerHTML = TOAST_ICONS[sev]; // static, trusted markup

  const main = elem("div", "notif-row-main");

  const top = elem("div", "notif-row-top");
  const titleWrap = elem("div", "notif-row-titlewrap");
  const title = elem(opts.full ? (opts.headingLevel ?? "h3") : "span", "notif-row-title");
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

  const atMs = opts.toMs(n.uptimeMs);
  const time = elem("time", "notif-row-time", relTime(atMs, opts.nowMs));
  time.dataset.uptime = String(n.uptimeMs);
  setDatetime(time, isoTime(atMs));
  time.title = fullTimestamp(atMs);
  if (opts.full) {
    const when = elem("div", "ev-when");
    when.append(elem("span", "ev-abs-time mono", absTime(atMs)), time);
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

// restampRows refreshes every relative time, its datetime and full-timestamp
// tooltip, the absolute time on full rows, and every ongoing-duration badge under
// root, so a list left open stays current (including after a re-sync moves the
// clock anchor) without a full re-render (which would disturb scroll and focus).
// Every write is diffed, so an unchanged time does not churn the DOM.
export function restampRows(root: ParentNode, toMs: UptimeToMs): void {
  const nowMs = Date.now();
  root.querySelectorAll<HTMLElement>("time.notif-row-time[data-uptime]").forEach((t) => {
    const atMs = toMs(Number(t.dataset.uptime));
    setText(t, relTime(atMs, nowMs));
    setDatetime(t, isoTime(atMs));
    const full = fullTimestamp(atMs);
    if (t.title !== full) t.title = full;
    // Full rows show an absolute time beside the relative one, in the same slot.
    const abs = t.parentElement?.querySelector<HTMLElement>(".ev-abs-time");
    if (abs) setText(abs, absTime(atMs));
  });
  root.querySelectorAll<HTMLElement>(".ev-life-ongoing[data-since]").forEach((b) => {
    setText(b, ongoingText(Number(b.dataset.since), nowMs, toMs));
  });
}
