import { router } from "./lib/router.js";
import { store } from "./lib/store.js";
import { DashboardView } from "./views/dashboard.js";
import { SystemView } from "./views/system.js";
import { EventsView } from "./views/events.js";
import { NotificationStore } from "./lib/notifications.js";
import { NotificationCenter } from "./components/notification-center.js";
import { initLoginModal } from "./components/login-modal.js";
import { applyStoredToken } from "./lib/auth.js";
import { needsNotificationsFallback } from "./lib/dashboard-core.js";
import { initTheme, PREFERS_LIGHT_QUERY } from "./lib/theme.js";
import { showToast } from "./components/toast.js";

// How long the boot waits for the stream's connect re-sync to deliver the
// notifications snapshot before loading it directly (see init).
const NOTIFICATIONS_FALLBACK_MS = 3000;

class App {
  public init(): void {
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

  // The toggle and the live OS follow live in lib/theme.ts; this only hands it
  // the page's objects. A storage-blocked browser (a private window, site data
  // blocked) still switches the theme, and learns once why it will not stick.
  private initTheme(): void {
    let media: MediaQueryList | null = null;
    try {
      media = window.matchMedia(PREFERS_LIGHT_QUERY);
    } catch {
      /* matchMedia unavailable: no live follow */
    }
    initTheme({
      root: document.documentElement,
      toggle: document.getElementById("theme-toggle-btn"),
      storage: () => window.localStorage,
      media,
      onSaveFailed: () =>
        showToast("Theme changed for this visit only. This browser is blocking site data, so the choice resets on reload."),
    });
  }

  private initNav(): void {
    document.querySelectorAll<HTMLElement>(".nav-item").forEach((btn) => {
      btn.addEventListener("click", () => {
        const view = btn.dataset.view;
        if (view === "dashboard" || view === "events" || view === "system") {
          router.navigate(view);
        }
      });
    });
  }

  private initViews(notifications: NotificationStore): void {
    new DashboardView();
    new EventsView(notifications);
    new SystemView();
  }
}

// Boot application on DOM ready
document.addEventListener("DOMContentLoaded", () => {
  const app = new App();
  app.init();
});
