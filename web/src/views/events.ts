// The Events page: the full in-memory event log of the current boot. It renders
// from the same NotificationStore as the bell, but does not hide entries in the
// per-browser dismissed set (they only count as read), so entries cleared from
// the bell stay listed here. The page is reactive: it re-renders on every store
// "change" while visible, and only marks itself dirty while hidden, so a burst of
// events costs nothing off-screen. Rows are cached by id and reused across
// renders; a row is rebuilt only when its rendered state (unread, lifecycle)
// changes.

import { router } from "../lib/router.js";
import { button, elem, iconSpan, setHidden, setText } from "../lib/ui.js";
import { showToast } from "../components/toast.js";
import { FilterChips } from "../components/filter-chips.js";
import { StatTile } from "../components/stat-tile.js";
import { RESTAMP_MS, SEVERITY_TO_TOAST, renderNotificationRow, restampRows, type ChipFacet } from "../components/notification-row.js";
import { activeConditions, type CoreState } from "../lib/notifications-core.js";
import {
  CATEGORIES,
  SEVERITIES,
  conditionLifecycles,
  emptyFilter,
  exportJSON,
  facetCounts,
  filterEvents,
  isFilterActive,
  type EventFilter,
  type Lifecycle,
} from "../lib/events-core.js";
import type { NotificationStore } from "../lib/notifications.js";
import type { Notification, NotificationSeverity } from "../lib/types.js";

const ICON_ALERT =
  '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"></path></svg>';
const ICON_LOG =
  '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 6h13"></path><path d="M8 12h13"></path><path d="M8 18h13"></path><path d="M3 6h.01"></path><path d="M3 12h.01"></path><path d="M3 18h.01"></path></svg>';
const ICON_SEARCH =
  '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="8"></circle><path d="m21 21-4.3-4.3"></path></svg>';
const ICON_DOWNLOAD =
  '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>';
const ICON_CHECK =
  '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"></path></svg>';
const ICON_X =
  '<svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>';

// The ring depth the server keeps (notify.defaultCapacity), quoted in the
// retention note so an operator knows the log is bounded.
const RETAINED_MAX = 500;

const SEVERITY_CHIP_LABEL: Record<NotificationSeverity, string> = {
  error: "Errors",
  warning: "Warnings",
  info: "Info",
};

interface CachedRow {
  sig: string;
  el: HTMLElement;
}

// rowSig is the part of a row's rendered state that can change after it is
// first drawn. Relative and absolute times, the tooltip, and ongoing durations
// are restamped in place, so they are deliberately left out.
function rowSig(unread: boolean, lc: Lifecycle | undefined): string {
  if (!lc) return unread ? "u" : "r";
  const life = lc.state === "ongoing" ? "on" : `res:${lc.durationMs ?? "?"}`;
  return `${unread ? "u" : "r"}|${life}`;
}

// dayKey buckets a skew-corrected instant by the viewer's local calendar day,
// as a local YYYY-MM-DD string (getMonth is zero-based, hence the +1).
function dayKey(ms: number): string {
  const d = new Date(ms);
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${mm}-${dd}`;
}

function dayLabel(ms: number, nowMs: number): string {
  if (dayKey(ms) === dayKey(nowMs)) return "Today";
  // Compute yesterday by decrementing the calendar date, not by subtracting 24h:
  // around a DST transition a day is 23 or 25 hours, so now minus 86_400_000 can
  // land on today or two days back.
  const y = new Date(nowMs);
  y.setDate(y.getDate() - 1);
  if (dayKey(ms) === dayKey(y.getTime())) return "Yesterday";
  return new Date(ms).toLocaleDateString([], { weekday: "long", month: "short", day: "numeric" });
}

// syncChildren reconciles parent's children with nodes in place. Nodes that stay
// in the list and keep their relative order are never detached: they are an
// in-order subsequence once the stale nodes are gone, so the insert walk finds
// each already at its index and leaves it (and its focus) untouched. A node that
// is itself removed or rebuilt loses focus, a documented limitation. The removal
// pass runs first so a node dropped from above no longer shifts the survivors'
// indices, which would otherwise force a needless move. The parent is expected to
// hold only managed nodes (rows and day headers here), so indexing its children
// directly is safe.
function syncChildren(parent: HTMLElement, nodes: HTMLElement[]): void {
  const want = new Set(nodes);
  for (const c of Array.from(parent.children)) if (!want.has(c as HTMLElement)) c.remove();
  for (let i = 0; i < nodes.length; i++) {
    if (parent.children[i] !== nodes[i]) parent.insertBefore(nodes[i], parent.children[i] ?? null);
  }
  while (parent.children.length > nodes.length) parent.lastElementChild?.remove();
}

export class EventsView {
  private readonly store: NotificationStore;
  private root!: HTMLElement;
  private filter: EventFilter = emptyFilter();
  private visible = false;
  private dirty = true;
  private timer: ReturnType<typeof setInterval> | null = null;
  private readonly rowCache = new Map<number, CachedRow>();
  // Active-condition rows are cached like the log's rows so an unchanged render
  // reuses the same nodes (see syncChildren). Cleared on a boot change alongside
  // rowCache, pruned to the currently active ids each render.
  private readonly activeCache = new Map<number, CachedRow>();
  // Day headers keyed by dayKey plus label, reused as nodes across renders and
  // pruned to the headers drawn this render.
  private readonly dayCache = new Map<string, HTMLElement>();

  private tiles!: {
    active: StatTile;
    error: StatTile;
    warning: StatTile;
    info: StatTile;
    retained: StatTile;
  };
  private activeCard!: HTMLElement;
  private activeList!: HTMLElement;
  private activeDesc!: HTMLElement;
  private unreadPill!: HTMLElement;
  // bootId the id-keyed caches (the row and active caches) were built under. Ids
  // restart from 1 on an appliance restart, so a cached row for id N would show
  // the previous boot's event. The day cache keys on the calendar day, not an id,
  // so it is boot-independent and not cleared here.
  private cacheBoot: string | null = null;
  private search!: HTMLInputElement;
  private sevChips!: FilterChips;
  private catChips!: FilterChips;
  private filterBar!: HTMLElement;
  private sourcePill!: HTMLElement;
  private sourcePillText!: HTMLElement;
  private resultText!: HTMLElement;
  // A visually-hidden status region that mirrors the result count, but only after
  // a USER filter change: store-driven (live-event) renders leave it untouched so
  // a screen reader is not interrupted by traffic the operator did not ask about.
  private announceEl!: HTMLElement;
  private announce = false;
  private announceTimer: ReturnType<typeof setTimeout> | null = null;
  private resetBtn!: HTMLButtonElement;
  private listEl!: HTMLElement;
  private emptyEl!: HTMLElement;
  private emptyTitle!: HTMLElement;
  private emptyBody!: HTMLElement;
  private emptyReset!: HTMLButtonElement;

  constructor(store: NotificationStore) {
    this.store = store;
    const root = document.getElementById("view-events");
    if (!root) {
      console.error("Events view container (#view-events) not found.");
      return;
    }
    this.root = root;
    this.build();

    this.store.addEventListener("change", () => {
      if (this.visible) this.render();
      else this.dirty = true;
    });
    router.addEventListener("route", (e: Event) => {
      this.setVisible((e as CustomEvent<string>).detail === "events");
    });
    document.addEventListener("keydown", this.onKeydown);
    this.setVisible(router.getCurrentView() === "events");
  }

  // ---------- construction ----------

  private build(): void {
    const tilesEl = elem("div", "ev-tiles");
    this.tiles = {
      active: new StatTile({ label: "Active issues", tone: "error" }),
      error: new StatTile({ label: "Errors", tone: "error", onClick: () => this.soloSeverity("error") }),
      warning: new StatTile({ label: "Warnings", tone: "warn", onClick: () => this.soloSeverity("warning") }),
      info: new StatTile({ label: "Info", tone: "info", onClick: () => this.soloSeverity("info") }),
      retained: new StatTile({ label: "Retained", tone: "neutral" }),
    };
    tilesEl.append(
      this.tiles.active.el,
      this.tiles.error.el,
      this.tiles.warning.el,
      this.tiles.info.el,
      this.tiles.retained.el,
    );

    this.root.append(tilesEl, this.buildActiveCard(), this.buildLogCard());
  }

  private sectionHead(icon: string, title: string, desc: string): { head: HTMLElement; titleEl: HTMLElement; descEl: HTMLElement; actions: HTMLElement } {
    const head = elem("div", "section-head ev-section-head");
    const left = elem("div");
    const titleEl = elem("h2", "section-title");
    titleEl.append(iconSpan(icon), elem("span", undefined, title));
    const descEl = elem("span", "section-desc", desc);
    left.append(titleEl, descEl);
    const actions = elem("div", "ev-head-actions");
    head.append(left, actions);
    return { head, titleEl, descEl, actions };
  }

  private buildActiveCard(): HTMLElement {
    const card = elem("section", "config-section-card ev-active-card");
    card.setAttribute("aria-label", "Active conditions");
    const { head, descEl } = this.sectionHead(ICON_ALERT, "Active conditions", "");
    this.activeDesc = descEl;
    this.activeList = elem("div", "ev-list ev-active-list");
    card.append(head, this.activeList);
    card.hidden = true;
    this.activeCard = card;
    return card;
  }

  private buildLogCard(): HTMLElement {
    const card = elem("section", "config-section-card ev-log-card");
    card.setAttribute("aria-label", "Event log");
    const { head, titleEl, actions } = this.sectionHead(
      ICON_LOG,
      "Event log",
      "Recent events from this boot, including those cleared from the bell.",
    );
    this.unreadPill = elem("span", "ev-unread-pill");
    this.unreadPill.hidden = true;
    titleEl.append(this.unreadPill);

    actions.append(
      button({ variant: "secondary", icon: ICON_CHECK, label: "Mark all read", onClick: () => this.store.markAllRead() }),
      button({ variant: "secondary", icon: ICON_DOWNLOAD, label: "Export", title: "Download the shown events as JSON", onClick: () => this.exportShown() }),
    );

    // Toolbar: search plus the two facet groups.
    const toolbar = elem("div", "ev-toolbar");
    const searchWrap = elem("label", "ev-search");
    searchWrap.append(iconSpan(ICON_SEARCH, "ev-search-icon"));
    this.search = document.createElement("input");
    this.search.type = "search";
    this.search.className = "field-input ev-search-input";
    this.search.placeholder = "Search title, message or source";
    this.search.setAttribute("aria-label", "Search events");
    this.search.setAttribute("aria-keyshortcuts", "/");
    this.search.autocomplete = "off";
    this.search.spellcheck = false;
    this.search.addEventListener("input", () => {
      this.filter.query = this.search.value;
      this.render();
      // Announce the result count only once typing settles, so a screen reader is
      // not interrupted on every keystroke. render() has already refreshed the
      // visible resultText; the timer mirrors its final text into the status region.
      this.cancelAnnounce();
      this.announceTimer = window.setTimeout(() => {
        this.announceTimer = null;
        setText(this.announceEl, this.resultText.textContent ?? "");
      }, 500);
    });
    const kbd = elem("kbd", "ev-search-kbd", "/");
    kbd.setAttribute("aria-hidden", "true");
    searchWrap.append(this.search, kbd);

    this.sevChips = new FilterChips({
      label: "Severity",
      options: SEVERITIES.map((s) => ({ value: s, label: SEVERITY_CHIP_LABEL[s], tone: SEVERITY_TO_TOAST[s] })),
      onChange: (sel) => {
        this.filter.severities = sel as Set<NotificationSeverity>;
        this.announce = true;
        this.render();
      },
    });
    this.catChips = new FilterChips({
      label: "Category",
      options: CATEGORIES.map((c) => ({ value: c, label: c })),
      onChange: (sel) => {
        this.filter.categories = sel;
        this.announce = true;
        this.render();
      },
    });
    const facets = elem("div", "ev-facets");
    facets.append(this.sevChips.el, this.catChips.el);
    toolbar.append(searchWrap, facets);

    // Filter status line: an optional source pill, the result count, Reset.
    this.filterBar = elem("div", "ev-filterbar");
    this.sourcePill = elem("button", "ev-source-pill");
    this.sourcePill.setAttribute("type", "button");
    this.sourcePillText = elem("span", "mono");
    this.sourcePill.append(elem("span", "ev-source-pill-key", "Source"), this.sourcePillText, iconSpan(ICON_X, "ev-source-pill-x"));
    this.sourcePill.addEventListener("click", () => {
      this.filter.source = null;
      this.announce = true;
      this.render();
      // The render hides the pill, which would drop focus to the body; move it to
      // the search box (as resetFilters does).
      this.search.focus();
    });
    this.sourcePill.hidden = true;
    this.resultText = elem("span", "ev-result-text");
    this.announceEl = elem("span", "visually-hidden");
    this.announceEl.setAttribute("role", "status");
    this.resetBtn = elem("button", "btn-link ev-reset", "Reset filters") as HTMLButtonElement;
    this.resetBtn.type = "button";
    this.resetBtn.addEventListener("click", () => this.resetFilters());
    this.filterBar.append(this.sourcePill, this.resultText, this.announceEl, this.resetBtn);

    this.listEl = elem("div", "ev-list ev-log-list");

    this.emptyEl = elem("div", "ev-empty");
    this.emptyTitle = elem("p", "ev-empty-title");
    this.emptyBody = elem("p", "ev-empty-body");
    this.emptyReset = button({ variant: "secondary", label: "Reset filters", onClick: () => this.resetFilters() });
    this.emptyEl.append(this.emptyTitle, this.emptyBody, this.emptyReset);
    this.emptyEl.hidden = true;

    const note = elem(
      "p",
      "ev-retention-note",
      `Kept in memory only: the appliance holds the most recent ${RETAINED_MAX} events plus any condition still in effect, and the log starts empty after a restart.`,
    );

    card.append(head, toolbar, this.filterBar, this.listEl, this.emptyEl, note);
    return card;
  }

  // ---------- visibility and timers ----------

  private setVisible(on: boolean): void {
    if (on === this.visible) return;
    this.visible = on;
    if (on) {
      if (this.dirty) this.render();
      else restampRows(this.root, this.store.getState().serverOffsetMs);
      this.timer = setInterval(() => restampRows(this.root, this.store.getState().serverOffsetMs), RESTAMP_MS);
    } else {
      if (this.timer !== null) {
        clearInterval(this.timer);
        this.timer = null;
      }
      // A pending search announcement must not speak once the page is gone.
      this.cancelAnnounce();
    }
  }

  // cancelAnnounce drops a pending debounced search announcement.
  private cancelAnnounce(): void {
    if (this.announceTimer !== null) {
      clearTimeout(this.announceTimer);
      this.announceTimer = null;
    }
  }

  // "/" jumps to the search box while the page is showing, unless the operator
  // is already typing somewhere.
  private readonly onKeydown = (e: KeyboardEvent): void => {
    if (!this.visible || e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
    const t = e.target as HTMLElement | null;
    if (t && (t.isContentEditable || t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT")) return;
    e.preventDefault();
    this.search.focus();
  };

  // ---------- filter actions ----------

  private soloSeverity(sev: NotificationSeverity): void {
    const only = this.filter.severities.size === 1 && this.filter.severities.has(sev);
    this.filter.severities = only ? new Set() : new Set([sev]);
    this.announce = true;
    this.render();
  }

  private onChip = (facet: ChipFacet, value: string): void => {
    if (facet === "source") this.filter.source = value;
    else this.filter.categories = new Set([value]);
    this.announce = true;
    this.render();
  };

  private resetFilters(): void {
    // Reset announces immediately; a search announcement still pending would
    // repeat it half a second later.
    this.cancelAnnounce();
    this.filter = emptyFilter();
    this.search.value = "";
    this.announce = true;
    this.render();
    this.search.focus();
  }

  private exportShown(): void {
    const state = this.store.getState();
    const shown = filterEvents(state.items.values(), this.filter);
    if (shown.length === 0) {
      showToast("Nothing to export", "warn");
      return;
    }
    const json = exportJSON(shown, state.bootId, new Date().toISOString());
    const url = URL.createObjectURL(new Blob([json], { type: "application/json" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `remote-mic-events-${new Date().toISOString().replace(/[:.]/g, "-")}.json`;
    document.body.append(a);
    a.click();
    a.remove();
    // Revoke on the next tick: some browsers start the download asynchronously.
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }

  // ---------- render ----------

  private render(): void {
    this.dirty = false;
    const state = this.store.getState();
    // Ids restart from 1 on an appliance restart, so a cached row keyed by id
    // would show the previous boot's event. Drop both row caches on a boot change.
    if (state.bootId !== this.cacheBoot) {
      this.rowCache.clear();
      this.activeCache.clear();
      this.cacheBoot = state.bootId;
    }
    const nowMs = Date.now();
    const items = [...state.items.values()];
    const lifecycles = conditionLifecycles(items);
    const isUnread = (n: Notification): boolean => n.id > state.readWatermark && !state.dismissed.has(n.id);

    // Before the first snapshot (or after a failed load) an empty items list is
    // "not fetched yet", not "no events"; show a loading state instead of asserting.
    const loading = !this.store.hasLoaded() && items.length === 0;

    // activeConditions is pure over state; compute it once and share it with the
    // tiles and the active-conditions card rather than deriving it twice.
    const active = activeConditions(state);
    this.renderTiles(state, items, nowMs, active, loading);
    this.renderActive(state, active, lifecycles, isUnread, nowMs);

    // Facet chips and their counts.
    const counts = facetCounts(items, this.filter);
    this.sevChips.setSelected(this.filter.severities);
    this.sevChips.setCounts(new Map(Object.entries(counts.severity)));
    this.catChips.setSelected(this.filter.categories);
    this.catChips.setCounts(counts.category);
    for (const s of SEVERITIES) {
      this.tiles[s].setPressed(this.filter.severities.size === 1 && this.filter.severities.has(s));
    }

    const shown = filterEvents(items, this.filter);
    const filterActive = isFilterActive(this.filter);

    // Filter status line.
    setHidden(this.sourcePill, this.filter.source === null);
    setText(this.sourcePillText, this.filter.source ?? "");
    this.sourcePill.setAttribute("aria-label", `Source ${this.filter.source ?? ""}, remove filter`);
    const resultLabel = loading
      ? ""
      : filterActive
        ? `Showing ${shown.length} of ${items.length} ${items.length === 1 ? "event" : "events"}`
        : `${items.length} ${items.length === 1 ? "event" : "events"}`;
    setText(this.resultText, resultLabel);
    // Mirror the count into the status region only for a user filter change; a
    // store-driven (live-event) render leaves announce false so it stays silent.
    if (this.announce) {
      setText(this.announceEl, resultLabel);
      this.announce = false;
    }
    setHidden(this.resetBtn, !filterActive);

    const unread = items.filter(isUnread).length;
    setText(this.unreadPill, `${unread} unread`);
    setHidden(this.unreadPill, unread === 0);

    this.renderList(shown, lifecycles, isUnread, state.serverOffsetMs, nowMs);

    // Empty states: nothing at all yet, or nothing matching the filter.
    const empty = shown.length === 0;
    setHidden(this.emptyEl, !empty);
    setHidden(this.listEl, empty);
    if (empty) {
      if (items.length === 0) {
        if (loading) {
          setText(this.emptyTitle, "Loading events");
          setText(this.emptyBody, "Waiting for the event log from the appliance.");
        } else {
          setText(this.emptyTitle, "No events yet");
          setText(this.emptyBody, "Device, audio, stream, system and config events appear here as they happen.");
        }
        setHidden(this.emptyReset, true);
      } else {
        setText(this.emptyTitle, "No matching events");
        setText(this.emptyBody, "Nothing retained matches the current filters.");
        setHidden(this.emptyReset, false);
      }
    }
  }

  private renderTiles(state: CoreState, items: Notification[], nowMs: number, active: Notification[], loading: boolean): void {
    if (loading) {
      // No snapshot yet: every count is unknown, so show a dash rather than
      // asserting zeros the fetch has not confirmed.
      this.tiles.active.set("-", "Loading");
      this.tiles.active.setAlert(false);
      this.tiles.error.set("-", "");
      this.tiles.warning.set("-", "");
      this.tiles.info.set("-", "");
      this.tiles.retained.set("-", "");
      return;
    }
    this.tiles.active.set(
      String(active.length),
      active.length === 0 ? "All clear" : active.length === 1 ? "Condition in effect" : "Conditions in effect",
    );
    this.tiles.active.setAlert(active.length > 0);

    const bySev: Record<NotificationSeverity, number> = { error: 0, warning: 0, info: 0 };
    let oldest: Notification | null = null;
    for (const n of items) {
      if (n.severity in bySev) bySev[n.severity]++;
      if (!oldest || n.id < oldest.id) oldest = n;
    }
    this.tiles.error.set(String(bySev.error), "In log");
    this.tiles.warning.set(String(bySev.warning), "In log");
    this.tiles.info.set(String(bySev.info), "In log");

    let caption = "";
    if (oldest) {
      const ms = Date.parse(oldest.time) + state.serverOffsetMs;
      if (Number.isFinite(ms)) {
        const time = new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
        caption = dayKey(ms) === dayKey(nowMs) ? `Oldest ${time}` : `Oldest ${dayLabel(ms, nowMs)} ${time}`;
      }
    }
    this.tiles.retained.set(`${items.length}`, caption || "In memory");
  }

  private renderActive(
    state: CoreState,
    active: Notification[],
    lifecycles: Map<number, Lifecycle>,
    isUnread: (n: Notification) => boolean,
    nowMs: number,
  ): void {
    const sorted = [...active].sort((a, b) => b.id - a.id);
    setHidden(this.activeCard, sorted.length === 0);
    setText(
      this.activeDesc,
      `${sorted.length} ${sorted.length === 1 ? "condition is" : "conditions are"} still in effect. They clear on their own when the appliance recovers.`,
    );
    // Cache active rows by id so an unchanged render reuses the same nodes and
    // syncChildren leaves them (and any focus) in place.
    const nodes: HTMLElement[] = [];
    const keep = new Set<number>();
    for (const n of sorted) {
      const unread = isUnread(n);
      const lc = lifecycles.get(n.id);
      const sig = rowSig(unread, lc);
      let cached = this.activeCache.get(n.id);
      if (!cached || cached.sig !== sig) {
        cached = {
          sig,
          el: renderNotificationRow(n, {
            nowMs,
            offsetMs: state.serverOffsetMs,
            full: true,
            // The active card's title is an h2 with no day headers, so its rows are h3.
            headingLevel: "h3",
            unread,
            lifecycle: lc,
            onChip: this.onChip,
          }),
        };
        this.activeCache.set(n.id, cached);
      }
      keep.add(n.id);
      nodes.push(cached.el);
    }
    // A condition that cleared is no longer active, so drop its cached row.
    for (const id of this.activeCache.keys()) {
      if (!keep.has(id)) this.activeCache.delete(id);
    }
    syncChildren(this.activeList, nodes);
    // Reused rows carry the relative time they were drawn with; bring them up to
    // date in one pass.
    restampRows(this.activeList, state.serverOffsetMs);
  }

  private renderList(
    shown: Notification[],
    lifecycles: Map<number, Lifecycle>,
    isUnread: (n: Notification) => boolean,
    offsetMs: number,
    nowMs: number,
  ): void {
    const state = this.store.getState();
    const nodes: HTMLElement[] = [];
    const keepDays = new Set<string>();
    // null, not "": an unparseable time also yields the "" key, and the first
    // group must still get its "Unknown time" header.
    let lastDay: string | null = null;
    for (const n of shown) {
      const ms = Date.parse(n.time) + offsetMs;
      const key = Number.isFinite(ms) ? dayKey(ms) : "";
      if (key !== lastDay) {
        lastDay = key;
        const label = Number.isFinite(ms) ? dayLabel(ms, nowMs) : "Unknown time";
        const dayKeyLabel = `${key}|${label}`;
        let h: HTMLElement;
        if (keepDays.has(dayKeyLabel)) {
          // This day group already appeared earlier in this render. Rows are
          // ordered by id, not time, so a backward clock step (or two runs of
          // "Unknown time") can split one label into separate groups. The cache
          // holds a single node per key and a node cannot sit in two places, so
          // the repeat gets a fresh uncached header.
          h = elem("h3", "ev-day", label);
        } else {
          h = this.dayCache.get(dayKeyLabel) ?? elem("h3", "ev-day", label);
          this.dayCache.set(dayKeyLabel, h);
          keepDays.add(dayKeyLabel);
        }
        nodes.push(h);
      }
      const unread = isUnread(n);
      const lc = lifecycles.get(n.id);
      const sig = rowSig(unread, lc);
      let cached = this.rowCache.get(n.id);
      if (!cached || cached.sig !== sig) {
        cached = {
          sig,
          // Log rows sit under h3 day headers, so their titles are h4.
          el: renderNotificationRow(n, { nowMs, offsetMs, full: true, headingLevel: "h4", unread, lifecycle: lc, onChip: this.onChip }),
        };
        this.rowCache.set(n.id, cached);
      }
      nodes.push(cached.el);
    }
    // Drop cached rows the server no longer retains so the cache stays bounded
    // by the ring. Rows merely filtered out this render are still in state.items,
    // so they are kept for a quick un-filter.
    for (const id of this.rowCache.keys()) {
      if (!state.items.has(id)) this.rowCache.delete(id);
    }
    // Prune day headers not drawn this render so the header cache stays small.
    for (const dk of this.dayCache.keys()) {
      if (!keepDays.has(dk)) this.dayCache.delete(dk);
    }
    syncChildren(this.listEl, nodes);
    // Reused rows carry the relative time they were drawn with; bring them up to
    // date in one pass.
    restampRows(this.listEl, offsetMs);
  }
}
