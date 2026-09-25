// The notification center: the header bell's badge plus the popover panel. It
// renders from the NotificationStore's "change" event and owns only view
// concerns (open/close, focus, keyboard, DOM). All state lives in the store and
// its pure core.

import { button, elem, setHidden, setText } from "../lib/ui.js";
import { ICON_CLOSE } from "./toast.js";
import { RESTAMP_MS, renderNotificationRow, restampRows } from "./notification-row.js";
import { activeConditions, unreadCount, uptimeToMs, type CoreState } from "../lib/notifications-core.js";
import type { NotificationStore } from "../lib/notifications.js";

// The popover renders at most this many history rows. The server ring holds far
// more (its capacity, 500 by default); the Events page (#/events) shows the full
// log, so the bell need not rebuild the whole ring on every live event. Active
// issues are always shown in full above the history.
const PANEL_HISTORY_MAX = 50;

export class NotificationCenter {
  private readonly store: NotificationStore;
  private bell!: HTMLElement;
  private badge!: HTMLElement | null;
  private live!: HTMLElement | null;
  private panel!: HTMLElement;
  private activeEl!: HTMLElement;
  private listEl!: HTMLElement;
  private emptyEl!: HTMLElement;
  // "Showing 50 of N events" beside the Events link, shown only when history is capped,
  // so a badge counting more unread entries than the list shows is explained.
  private countEl!: HTMLElement;
  private isOpen = false;
  // Handle for the interval that refreshes relative times while the panel is
  // open; null when the panel is closed.
  private timeTimer: ReturnType<typeof setInterval> | null = null;

  constructor(store: NotificationStore) {
    this.store = store;
    const bell = document.getElementById("header-bell-btn");
    if (!bell) {
      // The markup is always present in index.html; bail inert rather than throw
      // if a future layout drops the bell, so the rest of the app still boots.
      console.error("Notification bell button (#header-bell-btn) not found.");
      return;
    }
    this.bell = bell;
    this.badge = document.getElementById("notif-badge");
    this.live = document.getElementById("notif-unread-live");
    this.panel = this.buildPanel();
    (bell.parentElement ?? document.body).appendChild(this.panel);

    bell.addEventListener("click", () => this.toggle());
    this.store.addEventListener("change", () => this.onChange());
    this.onChange();
  }

  private buildPanel(): HTMLElement {
    const panel = elem("div", "notif-panel");
    panel.id = "notif-panel";
    panel.setAttribute("role", "dialog");
    panel.setAttribute("aria-label", "Notifications");
    panel.tabIndex = -1;
    panel.hidden = true;

    const head = elem("div", "notif-panel-head");
    const title = elem("h2", "notif-panel-title", "Notifications");
    const actions = elem("div", "notif-panel-actions");

    const readBtn = button({ variant: "secondary", extraClass: "notif-panel-btn", label: "Mark all read", onClick: () => this.store.markAllRead() });

    const clearBtn = button({ variant: "secondary", extraClass: "notif-panel-btn", label: "Clear", onClick: () => this.store.clearAll() });

    const closeBtn = elem("button", "icon-btn notif-panel-close");
    closeBtn.setAttribute("type", "button");
    closeBtn.setAttribute("aria-label", "Close notifications");
    closeBtn.innerHTML = ICON_CLOSE; // static, trusted markup
    closeBtn.addEventListener("click", () => this.close(true));

    actions.append(readBtn, clearBtn, closeBtn);
    head.append(title, actions);

    this.activeEl = elem("div", "notif-active");
    this.listEl = elem("div", "notif-list");
    this.emptyEl = elem("p", "notif-empty", "No notifications. Older and cleared events are on the Events page.");
    const body = elem("div", "notif-body");
    body.append(this.activeEl, this.listEl, this.emptyEl);

    // The popover shows what is still undismissed; the Events page keeps the full
    // log (cleared entries included), so point there for anything older.
    const foot = elem("div", "notif-panel-foot");
    const all = elem("a", "notif-panel-all", "View all events");
    all.setAttribute("href", "#/events");
    // Already on #/events the hash does not change, so close explicitly too.
    all.addEventListener("click", () => {
      this.close();
      // Closing the panel would drop focus to the body. The router moves focus to
      // the Events view section on a view change, but already on #/events the
      // hash does not change, so focus that same section here too (a harmless
      // repeat otherwise).
      requestAnimationFrame(() => document.getElementById("view-events")?.focus());
    });
    this.countEl = elem("span", "notif-panel-count");
    this.countEl.hidden = true;
    foot.append(this.countEl, all);

    panel.append(head, body, foot);
    return panel;
  }

  private onChange(): void {
    const state = this.store.getState();
    const count = unreadCount(state);
    if (this.badge) {
      setText(this.badge, count > 99 ? "99+" : String(count));
      setHidden(this.badge, count === 0);
    }
    // The badge is decorative (aria-hidden); the count reaches assistive tech
    // through this polite live region instead, announced only when non-zero.
    if (this.live) {
      setText(this.live, count > 0 ? `${count} unread notification${count === 1 ? "" : "s"}` : "");
    }
    if (this.isOpen) this.renderPanel(state);
  }

  private renderPanel(state: CoreState): void {
    const nowMs = Date.now();
    const toMs = (u: number): number => uptimeToMs(state, u);
    const active = activeConditions(state).filter((n) => !state.dismissed.has(n.id));
    const activeIds = new Set(active.map((n) => n.id));
    const allHistory = [...state.items.values()]
      .filter((n) => !state.dismissed.has(n.id) && !activeIds.has(n.id))
      .sort((a, b) => b.id - a.id);
    const history = allHistory.slice(0, PANEL_HISTORY_MAX);

    this.activeEl.replaceChildren();
    if (active.length > 0) {
      const label = `${active.length} active ${active.length === 1 ? "issue" : "issues"}`;
      this.activeEl.append(elem("div", "notif-group-head", label));
      for (const n of active) this.activeEl.append(renderNotificationRow(n, { nowMs, toMs }));
    }

    this.listEl.replaceChildren();
    for (const n of history) this.listEl.append(renderNotificationRow(n, { nowMs, toMs }));

    setHidden(this.emptyEl, active.length > 0 || history.length > 0);
    const capped = allHistory.length > history.length;
    setText(this.countEl, capped ? `Showing ${history.length} of ${allHistory.length} events` : "");
    setHidden(this.countEl, !capped);
  }

  // restampTimes keeps an open panel's relative "N ago" times current without a
  // full re-render (which would disturb scroll and focus).
  private restampTimes(): void {
    const state = this.store.getState();
    restampRows(this.panel, (u) => uptimeToMs(state, u));
  }

  private toggle(): void {
    if (this.isOpen) this.close(true);
    else this.open();
  }

  private open(): void {
    if (this.isOpen) return;
    this.isOpen = true;
    this.renderPanel(this.store.getState());
    this.panel.hidden = false;
    this.bell.setAttribute("aria-expanded", "true");
    // Capture phase so an outside click closes the panel before it activates
    // whatever it landed on; the bell and panel are excluded.
    document.addEventListener("click", this.onDocClick, true);
    document.addEventListener("keydown", this.onKeydown);
    window.addEventListener("hashchange", this.onHashChange);
    // Close when a keyboard user Tabs focus off the panel onto page content
    // behind it; the capture-phase click handler only covers pointer users.
    this.panel.addEventListener("focusout", this.onFocusOut);
    // Keep the relative "N ago" times from freezing while the panel sits open
    // with no store change to drive a re-render.
    this.timeTimer = setInterval(() => this.restampTimes(), RESTAMP_MS);
    this.panel.focus();
  }

  // restoreFocus returns focus to the bell; pass it only for deliberate
  // dismissals (Escape, the close button, the bell toggle). An outside click or
  // a route change must NOT pull focus to the bell, or it would steal focus from
  // whatever the user just clicked (e.g. an input elsewhere on the page).
  private close(restoreFocus = false): void {
    if (!this.isOpen) return;
    this.isOpen = false;
    this.panel.hidden = true;
    this.bell.setAttribute("aria-expanded", "false");
    document.removeEventListener("click", this.onDocClick, true);
    document.removeEventListener("keydown", this.onKeydown);
    window.removeEventListener("hashchange", this.onHashChange);
    this.panel.removeEventListener("focusout", this.onFocusOut);
    if (this.timeTimer !== null) {
      clearInterval(this.timeTimer);
      this.timeTimer = null;
    }
    if (restoreFocus) this.bell.focus();
  }

  private readonly onDocClick = (e: MouseEvent): void => {
    const target = e.target as Node | null;
    if (!target) return;
    if (this.panel.contains(target) || this.bell.contains(target)) return;
    this.close();
  };

  private readonly onKeydown = (e: KeyboardEvent): void => {
    if (e.key === "Escape") {
      e.stopPropagation();
      // The handler is on document and the panel has no focus trap, so Escape
      // can fire while focus is elsewhere on the page. Return focus to the bell
      // only when focus is actually inside the panel; otherwise leave it be.
      this.close(this.panel.contains(document.activeElement));
    }
  };

  private readonly onFocusOut = (e: FocusEvent): void => {
    const next = e.relatedTarget as Node | null;
    // Keep the panel open when focus stays inside it or moves to the bell, and
    // when focus leaves the document entirely (relatedTarget null, e.g. the
    // window blurred) so alt-tabbing away does not dismiss it. Close only when
    // focus lands on page content behind the panel, mirroring the outside-click
    // close for keyboard users who Tab past the last control. No restoreFocus:
    // focus has already moved on, so pulling it back to the bell would fight it.
    if (!next) return;
    if (this.panel.contains(next) || this.bell.contains(next)) return;
    this.close();
  };

  private readonly onHashChange = (): void => {
    this.close();
  };
}
