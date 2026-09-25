// The Events page: the full in-memory event log of the current boot. It renders
// from the same NotificationStore as the bell, but does not hide entries in the
// per-browser dismissed set (they only count as read), so entries cleared from
// the bell stay listed here. The page is reactive: while visible, a burst of
// store "change" events coalesces into one render on the next microtask, and
// while hidden it only marks itself dirty, so a burst of events costs nothing
// off-screen. Rows are cached by id and reused across renders; a row is rebuilt
// only when its rendered state (text, unread, lifecycle) changes.
//
// Every time on the page (row times, day groups, the oldest-entry caption,
// ongoing durations) comes from the entries' server uptime mapped onto the
// browser clock, so a server clock step cannot misplace or mis-measure them.

import { router } from "../lib/router.js";
import { button, clearBusy, downloadBlob, elem, iconSpan, readBoolPref, setBusy, setHidden, setText, switchControl, writeBoolPref } from "../lib/ui.js";
import { showToast } from "../components/toast.js";
import { FilterChips } from "../components/filter-chips.js";
import { StatTile } from "../components/stat-tile.js";
import { RESTAMP_MS, SEVERITY_TO_TOAST, renderNotificationRow, restampRows, type ChipFacet } from "../components/notification-row.js";
import { activeConditions, eventTimeMs, uptimeToMs, type CoreState } from "../lib/notifications-core.js";
import {
  CATEGORIES,
  SEVERITIES,
  conditionLifecycles,
  dayKey,
  dayLabel,
  emptyFilter,
  exportJSON,
  facetCounts,
  filterEvents,
  isFilterActive,
  oldestCaption,
  resultCountLabel,
  rowSignature,
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

// Per-browser opt-out for the "/" search shortcut. A single-character shortcut
// must be possible to turn off (WCAG 2.1.4): it can fire from speech input or a
// stray keypress.
const SLASH_PREF_KEY = "remote-mic-events-slash-shortcut";

// Load-failure copy. The notice covers a failure with entries listed, the empty
// state one with nothing listed; each is spoken exactly as it reads.
// NotificationStore retries a failed load with a backoff on its own, so Retry
// only brings the next attempt forward.
const RETRY_HINT = "The page keeps retrying on its own; Retry tries again now.";
const NOTICE_TEXT = `The event log could not be loaded from the appliance, so events may be missing. ${RETRY_HINT}`;
const FAILED_TITLE = "Could not load the event log";
const FAILED_BODY = `The event log could not be loaded from the appliance. ${RETRY_HINT}`;
const RELOADED_TEXT = "Event log reloaded.";

const SEVERITY_CHIP_LABEL: Record<NotificationSeverity, string> = {
  error: "Errors",
  warning: "Warnings",
  info: "Info",
};

interface CachedRow {
  sig: string;
  el: HTMLElement;
}

// FocusMark records where keyboard focus sat inside a row before a render, so it
// can be put back if the render rebuilt or removed that row.
interface FocusMark {
  el: HTMLElement;
  list: HTMLElement;
  id: string;
  index: number;
}

// syncChildren reconciles parent's children with nodes in place. Nodes that stay
// in the list and keep their relative order are never detached: they are an
// in-order subsequence once the stale nodes are gone, so the insert walk finds
// each already at its index and leaves it (and its focus) untouched. A node that
// is itself removed or rebuilt loses focus; render() puts it back (see
// restoreFocus). The removal pass runs first so a node dropped from above no
// longer shifts the survivors' indices, which would otherwise force a needless
// move. The parent is expected to hold only managed nodes (rows and day headers
// here), so indexing its children directly is safe.
function syncChildren(parent: HTMLElement, nodes: HTMLElement[]): void {
  const want = new Set(nodes);
  for (const c of Array.from(parent.children)) if (!want.has(c as HTMLElement)) c.remove();
  for (let i = 0; i < nodes.length; i++) {
    if (parent.children[i] !== nodes[i]) parent.insertBefore(nodes[i], parent.children[i] ?? null);
  }
  while (parent.children.length > nodes.length) parent.lastElementChild?.remove();
}

// RowContext is what every row in one render shares.
interface RowContext {
  nowMs: number;
  toMs: (uptimeMs: number) => number;
  lifecycles: Map<number, Lifecycle>;
  isUnread: (n: Notification) => boolean;
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
  // The local day the day headers were last drawn for. The restamp tick
  // re-renders when it changes, so "Today" rolls over to "Yesterday" at
  // midnight on a page left open with no new events.
  private renderedDay = "";
  // Set while a Retry of a failed load is in flight, so the page shows "Loading"
  // until that retry settles. It is cleared by the retry's own promise, not by
  // the next store change: an older load failing meanwhile would otherwise flip
  // the page back to the failure state while the retry may still succeed.
  private retrying = false;
  // Set while a store-driven render is queued for the next microtask, so a burst
  // of store changes (a re-sync plus live events) renders once.
  private renderScheduled = false;
  // Whether the failure notice was last announced, so it is spoken once per
  // failure rather than on every render.
  private failAnnounced = false;
  private slashShortcut = readBoolPref(SLASH_PREF_KEY, true);

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
  private logTitle!: HTMLElement;
  private unreadPill!: HTMLElement;
  // bootId the id-keyed caches (the row and active caches) were built under. Ids
  // restart from 1 on an appliance restart, so a cached row for id N would show
  // the previous boot's event. The day cache keys on the calendar day, not an id,
  // so it is boot-independent and not cleared here.
  private cacheBoot: string | null = null;
  private search!: HTMLInputElement;
  private searchKbd!: HTMLElement;
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
  private emptyRetry!: HTMLButtonElement;
  private loadNotice!: HTMLElement;
  private noticeRetry!: HTMLButtonElement;
  // Set while the notice's Retry is in flight. Its busy state is aria-disabled,
  // which does not stop a click, so this keeps a second press from stacking
  // another load.
  private noticeRetrying = false;
  private retentionNote!: HTMLElement;

  constructor(store: NotificationStore) {
    this.store = store;
    const root = document.getElementById("view-events");
    if (!root) {
      console.error("Events view container (#view-events) not found.");
      return;
    }
    this.root = root;
    this.build();

    this.store.addEventListener("change", () => this.scheduleRender());
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
      active: new StatTile({ label: "Active Issues", tone: "ok" }),
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
    card.setAttribute("aria-label", "Active Issues");
    const { head, descEl } = this.sectionHead(ICON_ALERT, "Active Issues", "");
    this.activeDesc = descEl;
    this.activeList = elem("div", "ev-list ev-active-list");
    card.append(head, this.activeList);
    card.hidden = true;
    this.activeCard = card;
    return card;
  }

  private buildLogCard(): HTMLElement {
    const card = elem("section", "config-section-card ev-log-card");
    card.setAttribute("aria-label", "Event Log");
    const { head, titleEl, actions } = this.sectionHead(
      ICON_LOG,
      "Event Log",
      "Recent events from this boot, including those cleared from the bell.",
    );
    // Focus lands here when the row holding keyboard focus disappears (a
    // condition clears, or the ring trims it); see restoreFocus.
    titleEl.tabIndex = -1;
    this.logTitle = titleEl;
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
    this.searchKbd = elem("kbd", "ev-search-kbd", "/");
    this.searchKbd.setAttribute("aria-hidden", "true");
    searchWrap.append(this.search, this.searchKbd);
    this.applySlashShortcut();

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
    // The "/" shortcut switch sits beside the search box it jumps to, where an
    // operator looking for it will look.
    const searchRow = elem("div", "ev-search-row");
    searchRow.append(searchWrap, this.buildSlashToggle());
    toolbar.append(searchRow, facets);

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

    // A failed snapshot while entries are listed (live events, or an earlier
    // snapshot): the log may be missing events, so say so and offer Retry. With
    // nothing listed the empty state below carries the failure instead.
    this.loadNotice = elem("div", "ev-load-notice");
    this.noticeRetry = button({ variant: "secondary", label: "Retry", onClick: () => this.retryFromNotice() });
    this.loadNotice.append(elem("p", "ev-load-notice-text", NOTICE_TEXT), this.noticeRetry);
    this.loadNotice.hidden = true;

    this.listEl = elem("div", "ev-list ev-log-list");

    this.emptyEl = elem("div", "ev-empty");
    this.emptyTitle = elem("p", "ev-empty-title");
    this.emptyBody = elem("p", "ev-empty-body");
    this.emptyReset = button({ variant: "secondary", label: "Reset filters", onClick: () => this.resetFilters() });
    this.emptyRetry = button({ variant: "secondary", label: "Retry", onClick: () => this.retryLoad() });
    this.emptyEl.append(this.emptyTitle, this.emptyBody, this.emptyReset, this.emptyRetry);
    this.emptyEl.hidden = true;

    const foot = elem("div", "ev-log-foot");
    this.retentionNote = elem("p", "ev-retention-note");
    foot.append(this.retentionNote);

    card.append(head, toolbar, this.filterBar, this.loadNotice, this.listEl, this.emptyEl, foot);
    return card;
  }

  // buildSlashToggle is the per-browser switch for the "/" shortcut.
  private buildSlashToggle(): HTMLElement {
    const { el, input } = switchControl({
      label: "Use / to jump to search",
      extraClass: "ev-shortcut-toggle",
      checked: this.slashShortcut,
    });
    input.addEventListener("change", () => {
      this.slashShortcut = input.checked;
      writeBoolPref(SLASH_PREF_KEY, input.checked);
      this.applySlashShortcut();
    });
    return el;
  }

  // applySlashShortcut shows the key hint and advertises the shortcut only while
  // it is on.
  private applySlashShortcut(): void {
    setHidden(this.searchKbd, !this.slashShortcut);
    if (this.slashShortcut) this.search.setAttribute("aria-keyshortcuts", "/");
    else this.search.removeAttribute("aria-keyshortcuts");
  }

  // ---------- visibility and timers ----------

  private setVisible(on: boolean): void {
    if (on === this.visible) return;
    this.visible = on;
    if (on) {
      if (this.dirty) this.render();
      else this.tick();
      this.timer = setInterval(() => this.tick(), RESTAMP_MS);
    } else {
      if (this.timer !== null) {
        clearInterval(this.timer);
        this.timer = null;
      }
      // A pending search announcement must not speak once the page is gone.
      this.cancelAnnounce();
    }
  }

  // tick refreshes the times in place, or re-renders once the local day has
  // changed since the last render so the day headers and the oldest-entry
  // caption roll over.
  private tick(): void {
    if (dayKey(Date.now()) !== this.renderedDay) {
      this.render();
      return;
    }
    const state = this.store.getState();
    restampRows(this.root, (u) => uptimeToMs(state, u));
  }

  // cancelAnnounce drops a pending debounced search announcement.
  private cancelAnnounce(): void {
    if (this.announceTimer !== null) {
      clearTimeout(this.announceTimer);
      this.announceTimer = null;
    }
  }

  // "/" jumps to the search box while the page is showing, unless the operator
  // turned the shortcut off or is already typing somewhere.
  private readonly onKeydown = (e: KeyboardEvent): void => {
    if (!this.visible || !this.slashShortcut || e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
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

  // retryLoad is the empty state's Retry: with nothing listed the page switches
  // to "Loading" until this retry settles.
  private retryLoad(): void {
    this.retrying = true;
    // Empty the status region first: setText writes only on change, so a repeat
    // failure would otherwise leave the same text in place and say nothing.
    setText(this.announceEl, "");
    this.render();
    // The Retry button is hidden by that render; keep keyboard focus on the page
    // without scrolling it.
    this.logTitle.focus({ preventScroll: true });
    // Settle from this retry's own outcome (see retrying). Its change event may
    // already have rendered with retrying still set, so render once more.
    void this.store
      .load()
      .then((ok) => {
        if (ok) setText(this.announceEl, RELOADED_TEXT);
      })
      .finally(() => {
        this.retrying = false;
        this.scheduleRender();
      });
  }

  // retryFromNotice is the failure notice's Retry. The listed entries stay, so
  // the notice stays up with its button busy (and focused) until the retry
  // settles, rather than flashing a loading state over a usable list.
  private retryFromNotice(): void {
    if (this.noticeRetrying) return;
    this.noticeRetrying = true;
    const btn = this.noticeRetry;
    setBusy(btn, "Retrying");
    setText(this.announceEl, "");
    void this.store
      .load()
      .then((ok) => {
        if (ok) {
          setText(this.announceEl, RELOADED_TEXT);
          // Success hides the notice (on the change render, which may already
          // have run), so a focused Retry would drop focus to the body. Move it
          // to the log heading without scrolling.
          const a = document.activeElement;
          if (a === btn || a === null || a === document.body) this.logTitle.focus({ preventScroll: true });
        } else if (this.store.hasFailed()) {
          // Still failing: the notice stayed up, so say it again.
          setText(this.announceEl, NOTICE_TEXT);
        }
      })
      .finally(() => {
        this.noticeRetrying = false;
        clearBusy(btn, "Retry");
      });
  }

  private exportShown(): void {
    const state = this.store.getState();
    const shown = filterEvents(state.items.values(), this.filter);
    if (shown.length === 0) {
      showToast("Nothing to export", "warn");
      return;
    }
    const json = exportJSON(shown, state.bootId, new Date().toISOString(), state.anchor);
    downloadBlob(
      new Blob([json], { type: "application/json" }),
      `remote-mic-events-${new Date().toISOString().replace(/[:.]/g, "-")}.json`,
    );
  }

  // ---------- focus ----------

  // captureFocus notes the row control holding keyboard focus, if any.
  private captureFocus(): FocusMark | null {
    const a = document.activeElement;
    if (!(a instanceof HTMLElement) || !this.root.contains(a)) return null;
    const row = a.closest<HTMLElement>(".notif-row[data-id]");
    const list = row?.parentElement;
    if (!row || !list || row.dataset.id === undefined) return null;
    const controls = Array.from(row.querySelectorAll<HTMLElement>("button"));
    return { el: a, list, id: row.dataset.id, index: controls.indexOf(a) };
  }

  // restoreFocus runs after a render. When the focused control's row was rebuilt,
  // focus moves to the same control in the new row; when the row is gone, to the
  // Event log heading, so keyboard focus never falls back to the page body.
  private restoreFocus(mark: FocusMark | null): void {
    if (!mark || (mark.el.isConnected && document.activeElement === mark.el)) return;
    // A user action during the render may have moved focus on purpose.
    if (document.activeElement && document.activeElement !== document.body) return;
    const row = mark.list.querySelector<HTMLElement>(`.notif-row[data-id="${mark.id}"]`);
    const controls = row ? Array.from(row.querySelectorAll<HTMLElement>("button")) : [];
    const target = controls[mark.index] ?? controls[0] ?? this.logTitle;
    // The fallback can sit far from the vanished row; keep the viewport where
    // the operator left it rather than jumping to the heading.
    target.focus({ preventScroll: true });
  }

  // ---------- render ----------

  // scheduleRender coalesces store-driven renders: a burst of changes in one
  // task renders once, on the next microtask. While hidden the page only marks
  // itself dirty and renders on showing.
  private scheduleRender(): void {
    if (!this.visible) {
      this.dirty = true;
      return;
    }
    if (this.renderScheduled) return;
    this.renderScheduled = true;
    queueMicrotask(() => {
      this.renderScheduled = false;
      if (this.visible) this.render();
      else this.dirty = true;
    });
  }

  private render(): void {
    const focus = this.captureFocus();
    this.renderAll();
    this.restoreFocus(focus);
  }

  private renderAll(): void {
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
    this.renderedDay = dayKey(nowMs);
    const items = [...state.items.values()];
    const ctx: RowContext = {
      nowMs,
      toMs: (u) => uptimeToMs(state, u),
      lifecycles: conditionLifecycles(items),
      isUnread: (n) => n.id > state.readWatermark && !state.dismissed.has(n.id),
    };

    // Before the first snapshot an empty items list is "not fetched yet", not
    // "no events"; show a loading state instead of asserting. A failed load is
    // shown whether or not entries are listed: live events (or an earlier
    // snapshot) do not make the log complete, so it stays visible with Retry.
    const loading = !this.store.hasLoaded() && items.length === 0;
    const failed = this.store.hasFailed() && !this.retrying;

    setText(
      this.retentionNote,
      `Kept in memory only: the appliance holds the most recent ${state.capacity} events plus any issue still in effect, and the log starts empty after a restart.`,
    );

    // activeConditions is pure over state; compute it once and share it with the
    // tiles and the active-issues card rather than deriving it twice.
    const active = activeConditions(state);
    this.renderTiles(state, items, nowMs, active, loading, failed);
    this.renderActive(active, ctx);

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
    const resultLabel = resultCountLabel(shown.length, items.length, filterActive, loading);
    setText(this.resultText, resultLabel);
    // Mirror the count into the status region only for a user filter change; a
    // store-driven (live-event) render leaves announce false so it stays silent.
    if (this.announce) {
      setText(this.announceEl, resultLabel);
      this.announce = false;
    }
    // Speak a load failure once when it appears; it is not a user filter change,
    // but the page would otherwise sit silently on an error.
    // It is spoken as it reads: the notice's text with entries listed, else the
    // empty state's title and body.
    if (failed && !this.failAnnounced) {
      setText(this.announceEl, items.length > 0 ? NOTICE_TEXT : `${FAILED_TITLE}. ${FAILED_BODY}`);
    }
    this.failAnnounced = failed;
    setHidden(this.resetBtn, !filterActive);

    const unread = items.filter(ctx.isUnread).length;
    setText(this.unreadPill, `${unread} unread`);
    setHidden(this.unreadPill, unread === 0);

    this.renderList(state, shown, ctx);

    // Empty states: the load failed, nothing fetched yet, nothing at all, or
    // nothing matching the filter.
    const empty = shown.length === 0;
    // The notice covers a failure with entries listed; the empty state below
    // covers one with nothing listed, so the two never show together.
    setHidden(this.loadNotice, !(failed && items.length > 0));
    setHidden(this.emptyEl, !empty);
    setHidden(this.listEl, empty);
    setHidden(this.emptyRetry, !(empty && failed && items.length === 0));
    if (empty) {
      if (items.length === 0) {
        if (failed) {
          setText(this.emptyTitle, FAILED_TITLE);
          setText(this.emptyBody, FAILED_BODY);
        } else if (loading) {
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

  private renderTiles(state: CoreState, items: Notification[], nowMs: number, active: Notification[], loading: boolean, failed: boolean): void {
    if (loading) {
      // No snapshot yet: every count is unknown, so show a dash rather than
      // asserting zeros the fetch has not confirmed.
      this.tiles.active.set("-", failed ? "Unavailable" : "Loading");
      this.tiles.active.setTone("neutral");
      this.tiles.active.setAlert(false);
      this.tiles.error.set("-", "");
      this.tiles.warning.set("-", "");
      this.tiles.info.set("-", "");
      this.tiles.retained.set("-", "");
      return;
    }
    if (failed && active.length === 0) {
      // The last load failed, so a condition raised since the last good snapshot
      // may be missing: zero is not a confirmed "All clear".
      this.tiles.active.set("0", "Possibly incomplete");
      this.tiles.active.setTone("neutral");
      this.tiles.active.setAlert(false);
    } else {
      this.tiles.active.set(String(active.length), active.length === 0 ? "All clear" : "Still in effect");
      // Red only when something is wrong: a clear board reads in the OK tone.
      this.tiles.active.setTone(active.length > 0 ? "error" : "ok");
      this.tiles.active.setAlert(active.length > 0);
    }

    const bySev: Record<NotificationSeverity, number> = { error: 0, warning: 0, info: 0 };
    let oldest: Notification | null = null;
    for (const n of items) {
      if (n.severity in bySev) bySev[n.severity]++;
      if (!oldest || n.id < oldest.id) oldest = n;
    }
    this.tiles.error.set(String(bySev.error), "In log");
    this.tiles.warning.set(String(bySev.warning), "In log");
    this.tiles.info.set(String(bySev.info), "In log");

    const caption = oldest ? oldestCaption(eventTimeMs(state, oldest), nowMs) : "";
    this.tiles.retained.set(`${items.length}`, caption || "In memory");
  }

  // cachedRow returns n's row from cache, rebuilding it only when its rendered
  // state (text, unread, lifecycle; see rowSignature) changed since it was
  // drawn.
  private cachedRow(cache: Map<number, CachedRow>, n: Notification, ctx: RowContext, headingLevel: "h3" | "h4"): HTMLElement {
    const unread = ctx.isUnread(n);
    const lifecycle = ctx.lifecycles.get(n.id);
    const sig = rowSignature(n, unread, lifecycle);
    let cached = cache.get(n.id);
    if (!cached || cached.sig !== sig) {
      const el = renderNotificationRow(n, { nowMs: ctx.nowMs, toMs: ctx.toMs, full: true, headingLevel, unread, lifecycle, onChip: this.onChip });
      cached = { sig, el };
      cache.set(n.id, cached);
    }
    return cached.el;
  }

  private renderActive(active: Notification[], ctx: RowContext): void {
    const sorted = [...active].sort((a, b) => b.id - a.id);
    setHidden(this.activeCard, sorted.length === 0);
    setText(
      this.activeDesc,
      `${sorted.length} ${sorted.length === 1 ? "issue is" : "issues are"} still in effect. They clear on their own when the appliance recovers.`,
    );
    // The active card's title is an h2 with no day headers, so its rows are h3.
    const nodes = sorted.map((n) => this.cachedRow(this.activeCache, n, ctx, "h3"));
    // An issue that cleared is no longer active, so drop its cached row.
    const keep = new Set(sorted.map((n) => n.id));
    for (const id of this.activeCache.keys()) {
      if (!keep.has(id)) this.activeCache.delete(id);
    }
    syncChildren(this.activeList, nodes);
    // Reused rows carry the times they were drawn with; bring them up to date.
    restampRows(this.activeList, ctx.toMs);
  }

  private renderList(state: CoreState, shown: Notification[], ctx: RowContext): void {
    const nodes: HTMLElement[] = [];
    const keepDays = new Set<string>();
    // null, not "": an unknown time also yields the "" key, and the first group
    // must still get its "Unknown time" header.
    let lastDay: string | null = null;
    for (const n of shown) {
      const ms = eventTimeMs(state, n);
      const key = Number.isFinite(ms) ? dayKey(ms) : "";
      if (key !== lastDay) {
        lastDay = key;
        const label = Number.isFinite(ms) ? dayLabel(ms, ctx.nowMs) : "Unknown time";
        const dayKeyLabel = `${key}|${label}`;
        let h: HTMLElement;
        if (keepDays.has(dayKeyLabel)) {
          // This day group already appeared earlier in this render. Rows are
          // ordered by id; uptime-derived times follow id order, but a browser
          // clock change between re-syncs (or two runs of "Unknown time") can
          // still split one label. The cache holds a single node per key and a
          // node cannot sit in two places, so the repeat gets an uncached header.
          h = elem("h3", "ev-day", label);
        } else {
          h = this.dayCache.get(dayKeyLabel) ?? elem("h3", "ev-day", label);
          this.dayCache.set(dayKeyLabel, h);
          keepDays.add(dayKeyLabel);
        }
        nodes.push(h);
      }
      // Log rows sit under h3 day headers, so their titles are h4.
      nodes.push(this.cachedRow(this.rowCache, n, ctx, "h4"));
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
    // Reused rows carry the times they were drawn with; bring them up to date.
    restampRows(this.listEl, ctx.toMs);
  }
}
