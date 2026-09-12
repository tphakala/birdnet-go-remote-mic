// The notification center: the header bell's badge plus the popover panel. It
// renders from the NotificationStore's "change" event and owns only view
// concerns (open/close, focus, keyboard, DOM). All state lives in the store and
// its pure core.

import { elem, formatRelative, setHidden, setText } from "../lib/ui.js";
import { TOAST_ICONS, ICON_CLOSE, type ToastType } from "./toast.js";
import { activeConditions, unreadCount, type CoreState } from "../lib/notifications-core.js";
import type { Notification, NotificationSeverity } from "../lib/types.js";
import type { NotificationStore } from "../lib/notifications.js";

// Notification severity maps onto the toast icon set so the center and the
// toasts show the same glyphs (warning uses the "warn" toast icon).
const SEVERITY_TO_TOAST: Record<NotificationSeverity, ToastType> = {
  error: "error",
  warning: "warn",
  info: "info",
};

// Spoken severity prefix: the icon is aria-hidden and color is not announced, so
// a screen reader would otherwise not hear whether a row is an error or info.
const SEVERITY_LABEL: Record<NotificationSeverity, string> = {
  error: "Error",
  warning: "Warning",
  info: "Info",
};

export class NotificationCenter {
  private readonly store: NotificationStore;
  private bell!: HTMLElement;
  private badge!: HTMLElement | null;
  private live!: HTMLElement | null;
  private panel!: HTMLElement;
  private activeEl!: HTMLElement;
  private listEl!: HTMLElement;
  private emptyEl!: HTMLElement;
  private isOpen = false;

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

    const readBtn = elem("button", "btn btn-secondary notif-panel-btn", "Mark all read");
    readBtn.setAttribute("type", "button");
    readBtn.addEventListener("click", () => this.store.markAllRead());

    const clearBtn = elem("button", "btn btn-secondary notif-panel-btn", "Clear");
    clearBtn.setAttribute("type", "button");
    clearBtn.addEventListener("click", () => this.store.clearAll());

    const closeBtn = elem("button", "icon-btn notif-panel-close");
    closeBtn.setAttribute("type", "button");
    closeBtn.setAttribute("aria-label", "Close notifications");
    closeBtn.innerHTML = ICON_CLOSE; // static, trusted markup
    closeBtn.addEventListener("click", () => this.close(true));

    actions.append(readBtn, clearBtn, closeBtn);
    head.append(title, actions);

    this.activeEl = elem("div", "notif-active");
    this.listEl = elem("div", "notif-list");
    this.emptyEl = elem("p", "notif-empty", "No notifications since start.");
    const body = elem("div", "notif-body");
    body.append(this.activeEl, this.listEl, this.emptyEl);

    panel.append(head, body);
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
    const offsetMs = state.serverOffsetMs;
    const active = activeConditions(state).filter((n) => !state.dismissed.has(n.id));
    const activeIds = new Set(active.map((n) => n.id));
    const history = [...state.items.values()]
      .filter((n) => !state.dismissed.has(n.id) && !activeIds.has(n.id))
      .sort((a, b) => b.id - a.id);

    this.activeEl.replaceChildren();
    if (active.length > 0) {
      const label = `${active.length} active ${active.length === 1 ? "issue" : "issues"}`;
      this.activeEl.append(elem("div", "notif-group-head", label));
      for (const n of active) this.activeEl.append(this.renderRow(n, nowMs, offsetMs));
    }

    this.listEl.replaceChildren();
    for (const n of history) this.listEl.append(this.renderRow(n, nowMs, offsetMs));

    setHidden(this.emptyEl, active.length > 0 || history.length > 0);
  }

  private renderRow(n: Notification, nowMs: number, offsetMs: number): HTMLElement {
    const row = elem("div", `notif-row notif-${n.severity}`);

    const icon = elem("span", "notif-row-icon");
    icon.setAttribute("aria-hidden", "true");
    icon.innerHTML = TOAST_ICONS[SEVERITY_TO_TOAST[n.severity]] ?? TOAST_ICONS.info; // trusted markup

    const main = elem("div", "notif-row-main");

    const top = elem("div", "notif-row-top");
    const title = elem("span", "notif-row-title");
    title.append(elem("span", "visually-hidden", `${SEVERITY_LABEL[n.severity]}: `));
    title.append(document.createTextNode(n.title));
    top.append(title);
    const time = elem("time", "notif-row-time", formatRelative(Date.parse(n.time) + offsetMs, nowMs));
    time.setAttribute("datetime", n.time);
    top.append(time);

    const meta = elem("div", "notif-row-meta");
    meta.append(elem("span", "notif-chip", n.category));
    if (n.source) meta.append(elem("span", "notif-chip notif-chip-source", n.source));

    main.append(top, meta);
    if (n.message) main.append(elem("div", "notif-row-msg", n.message));

    row.append(icon, main);
    return row;
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
      this.close(true);
    }
  };

  private readonly onHashChange = (): void => {
    this.close();
  };
}
