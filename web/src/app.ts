import { isViewName, router } from "./lib/router.js";
import { store } from "./lib/store.js";
import { DashboardView } from "./views/dashboard.js";
import { SystemView } from "./views/system.js";
import { EventsView } from "./views/events.js";
import { NotificationStore } from "./lib/notifications.js";
import { NotificationCenter } from "./components/notification-center.js";
import { initLoginModal } from "./components/login-modal.js";
import { applyStoredToken } from "./lib/auth.js";
import { needsNotificationsFallback } from "./lib/dashboard-core.js";
import { initTheme, PREFERS_LIGHT_QUERY, type Theme, type ThemeMode } from "./lib/theme.js";
import { prefSaveNotice } from "./lib/prefs.js";
import { ERROR_TTL_MS, showToast } from "./components/toast.js";
import { MenuButton } from "./components/menu-button.js";
import { AboutView } from "./views/about.js";

// How long the boot waits for the stream's connect re-sync to deliver the
// notifications snapshot before loading it directly (see init).
const NOTIFICATIONS_FALLBACK_MS = 3000;

// How long the "preferences not saved" warning stays up: as long as an error
// toast, since it is two sentences and can appear while the whole page changes
// colour.
const SAVE_FAILED_TOAST_MS = ERROR_TTL_MS;

// Icons for the theme menu's items (static, trusted markup), matching the
// header button's icons in index.html.
const ICON_SYSTEM = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="20" height="14" x="2" y="3" rx="2"></rect><line x1="8" x2="16" y1="21" y2="21"></line><line x1="12" x2="12" y1="17" y2="21"></line></svg>`;
const ICON_SUN = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"></circle><path d="M12 2v2"></path><path d="M12 20v2"></path><path d="m4.93 4.93 1.41 1.41"></path><path d="m17.66 17.66 1.41 1.41"></path><path d="M2 12h2"></path><path d="M20 12h2"></path><path d="m6.34 17.66-1.41 1.41"></path><path d="m19.07 4.93-1.41 1.41"></path></svg>`;
const ICON_MOON = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"></path></svg>`;

const MODE_LABEL: Record<ThemeMode, string> = { system: "System", light: "Light", dark: "Dark" };

class App {
  public init(): void {
    // One notice for every per-browser preference that cannot be saved (the
    // theme, hidden channels, notification read marks): a warning, held long
    // enough to read, since it explains why choices appear to reset on reload.
    prefSaveNotice.setHandler(() =>
      showToast(
        "This browser could not save your display preferences (such as the theme), so they reset on reload. Check that this browser allows site data here to keep them.",
        "warn",
        SAVE_FAILED_TOAST_MS,
      ),
    );
    this.initTheme();
    this.initNav();
    initLoginModal();

    // Construct the notification store (and its bell) before polling starts so
    // its "connection" subscription is in place when startPolling opens the SSE
    // stream; the store re-syncs the snapshot on every (re)connect.
    const notifications = new NotificationStore();
    new NotificationCenter(notifications);
    this.initViews(notifications);

    router.init();
    // A hidden page (a background tab, a minimized window) pauses the poll, and
    // showing it again refreshes at once; see AppStore.setPageHidden.
    store.setPageHidden(document.hidden);
    document.addEventListener("visibilitychange", () => store.setPageHidden(document.hidden));
    // Push a stored token into the clients before the first request so a
    // token-gated appliance loads without a prompt on a returning browser.
    applyStoredToken();
    // The stream's connect re-syncs the notifications snapshot, and that load
    // is the one that must run: it follows the subscription, so nothing raised
    // between the snapshot and the stream is missed; a direct load here would
    // fetch it a second time. Load directly only as a fallback, when no snapshot
    // has arrived shortly after the app starts (a stream that cannot connect, or
    // a proxy that buffers it), after boot or after a login alike. The stream
    // being down is the trigger too, not only a snapshot never loaded: after a
    // re-login the previous session's snapshot would otherwise count as fresh.
    const armNotificationsFallback = (): void => {
      window.setTimeout(() => {
        if (needsNotificationsFallback(notifications.hasLoaded(), store.getState().connected)) void notifications.load();
      }, NOTIFICATIONS_FALLBACK_MS);
    };
    store.addEventListener("authok", armNotificationsFallback);
    void store.start().then((running) => {
      if (running) armNotificationsFallback();
    });
  }

  // The mode and the live OS follow live in lib/theme.ts; this hands it the
  // page's objects and binds the header menu button to it. A storage-blocked
  // browser (a private window, site data blocked) still switches the theme,
  // and learns once why it will not stick.
  private initTheme(): void {
    let media: MediaQueryList | null = null;
    try {
      media = window.matchMedia(PREFERS_LIGHT_QUERY);
    } catch {
      /* matchMedia unavailable: no live follow */
    }
    const btn = document.getElementById("theme-toggle-btn");
    let menu: MenuButton | null = null;
    // The label spells out the mode and the theme it shows, and the title
    // repeats it word for word, so a screen reader does not read the tooltip
    // again as a description.
    const show = (mode: ThemeMode, theme: Theme): void => {
      if (!btn) return;
      const label = mode === "system" ? `Theme: System (${theme})` : `Theme: ${MODE_LABEL[mode]}`;
      if (btn.getAttribute("aria-label") !== label) btn.setAttribute("aria-label", label);
      if (btn.title !== label) btn.title = label;
      if (btn.dataset.mode !== mode) btn.dataset.mode = mode;
      menu?.set(mode);
    };
    const controller = initTheme({
      root: document.documentElement,
      storage: () => window.localStorage,
      media,
      onStorage: (listener) => window.addEventListener("storage", (e) => listener({ key: e.key, newValue: e.newValue })),
      onApply: show,
      onSaveFailed: () => prefSaveNotice.report(),
    });
    if (!btn) return;
    menu = new MenuButton(btn, {
      label: "Theme",
      choices: [
        { value: "system", label: "System", icon: ICON_SYSTEM },
        { value: "light", label: "Light", icon: ICON_SUN },
        { value: "dark", label: "Dark", icon: ICON_MOON },
      ],
      onSelect: (v) => {
        if (v === "system" || v === "light" || v === "dark") controller.setMode(v);
      },
    });
    menu.set(controller.mode());
  }

  private initNav(): void {
    document.querySelectorAll<HTMLElement>(".nav-item").forEach((btn) => {
      btn.addEventListener("click", () => {
        const view = btn.dataset.view;
        if (isViewName(view)) router.navigate(view);
      });
    });
  }

  private initViews(notifications: NotificationStore): void {
    new DashboardView();
    new EventsView(notifications);
    new SystemView();
    new AboutView();
  }
}

// Boot application on DOM ready
document.addEventListener("DOMContentLoaded", () => {
  const app = new App();
  app.init();
});
