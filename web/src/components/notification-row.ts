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

import { formatRelative, h, setAttr, setText } from "../lib/ui.ts";
import { TOAST_ICONS, type ToastType } from "./toast.ts";
import { formatDuration, isoOrNull, type Lifecycle } from "../lib/events-core.ts";
import type { Notification, NotificationSeverity } from "../lib/types.ts";

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

// RESTAMP_MS is how often a visible list refreshes its relative "N ago" times
// and ongoing durations (and its absolute times and tooltips, when a re-sync
// moved the clock anchor). Times are minute-resolution above a minute, so a
// passive list does not need second-accurate updates.
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
// every row render and every anchor move format with both, so hoist them to
// module scope and reuse them. ABS_TIME_FMT matches the old toLocaleTimeString
// options; FULL_FMT's explicit fields match toLocaleString's numeric default.
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
  return isoOrNull(ms) ?? "";
}

// setDatetime writes a <time> element's machine-readable value, diffed so an
// unchanged time does not churn the DOM. An unknown time removes the attribute:
// an empty datetime is not a valid value.
function setDatetime(t: HTMLElement, iso: string): void {
  if (iso === "") t.removeAttribute("datetime");
  else setAttr(t, "datetime", iso);
}

function chip(label: string, extraClass: string, facet: ChipFacet, value: string, opts: RowOptions): HTMLElement {
  if (!opts.onChip) return h("span", { class: `notif-chip ${extraClass}`.trim() }, label);
  const b = h("button", { type: "button", class: `notif-chip notif-chip-btn ${extraClass}`.trim(), "aria-label": `Show only ${facet} ${value}`, title: `Filter by ${facet}` }, label);
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
  // An ongoing badge is restamped in place while the page stays open; see
  // restampRows.
  return h("span", { class: cls, "data-since": lc.state === "ongoing" ? lc.sinceUptimeMs : null }, lifecycleText(lc, n, opts));
}

export function renderNotificationRow(n: Notification, opts: RowOptions): HTMLElement {
  // One lookup feeds both the severity badge colour and the glyph, so a row can
  // never end up with an error colour and an info icon.
  const sev = SEVERITY_TO_TOAST[n.severity] ?? "info";
  const atMs = opts.toMs(n.uptimeMs);
  const iso = isoTime(atMs);
  const time = h(
    "time",
    {
      class: "notif-row-time",
      "data-uptime": n.uptimeMs,
      // The instant the absolute fields below were formatted for; restampRows
      // re-formats them only when the mapped instant moves.
      "data-at": atMs,
      datetime: iso === "" ? null : iso,
      title: fullTimestamp(atMs),
    },
    relTime(atMs, opts.nowMs),
  );

  const icon = h("span", { class: "notif-row-icon sev-badge", "aria-hidden": "true" });
  icon.innerHTML = TOAST_ICONS[sev]; // static, trusted markup

  return h(
    opts.full ? "article" : "div",
    { class: `notif-row sev-${sev}${opts.full ? " ev-row" : ""}${opts.unread ? " is-unread" : ""}`, "data-id": n.id },
    icon,
    h(
      "div",
      { class: "notif-row-main" },
      h(
        "div",
        { class: "notif-row-top" },
        h(
          "div",
          { class: "notif-row-titlewrap" },
          h(
            opts.full ? (opts.headingLevel ?? "h3") : "span",
            { class: "notif-row-title" },
            h("span", { class: "visually-hidden" }, `${SEVERITY_LABEL[n.severity]}: `),
            n.title,
          ),
          opts.unread && h("span", { class: "ev-unread-dot", title: "Unread" }, h("span", { class: "visually-hidden" }, "(unread)")),
        ),
        opts.full ? h("div", { class: "ev-when" }, h("span", { class: "ev-abs-time mono" }, absTime(atMs)), time) : time,
      ),
      h(
        "div",
        { class: "notif-row-meta" },
        chip(n.category, "", "category", n.category, opts),
        n.source && chip(n.source, "notif-chip-source", "source", n.source, opts),
        opts.lifecycle && lifecycleBadge(opts.lifecycle, n, opts),
      ),
      n.message && h("div", { class: "notif-row-msg" }, n.message),
    ),
  );
}

// restampRows refreshes every relative time and every ongoing-duration badge
// under root, so a list left open stays current without a full re-render (which
// would disturb scroll and focus). A row's mapped instant moves only when a
// re-sync moves the clock anchor, so its datetime, full-timestamp tooltip and
// the absolute time on full rows are re-formatted only when that instant
// differs from the one the row was last stamped with (data-at); on a plain
// tick only the relative text is formatted. Every write is diffed, so an
// unchanged time does not churn the DOM.
export function restampRows(root: ParentNode, toMs: UptimeToMs): void {
  const nowMs = Date.now();
  root.querySelectorAll<HTMLElement>("time.notif-row-time[data-uptime]").forEach((t) => {
    const atMs = toMs(Number(t.dataset.uptime));
    setText(t, relTime(atMs, nowMs));
    const at = String(atMs);
    if (t.dataset.at === at) return;
    t.dataset.at = at;
    setDatetime(t, isoTime(atMs));
    const full = fullTimestamp(atMs);
    setAttr(t, "title", full);
    // Full rows show an absolute time beside the relative one, in the same slot.
    const abs = t.parentElement?.querySelector<HTMLElement>(".ev-abs-time");
    if (abs) setText(abs, absTime(atMs));
  });
  root.querySelectorAll<HTMLElement>(".ev-life-ongoing[data-since]").forEach((b) => {
    setText(b, ongoingText(Number(b.dataset.since), nowMs, toMs));
  });
}
