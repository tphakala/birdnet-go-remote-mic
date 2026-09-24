import { router } from "./lib/router.js";
import { store } from "./lib/store.js";
import { DashboardView } from "./views/dashboard.js";
import { SystemView } from "./views/system.js";
import { EventsView } from "./views/events.js";
import { NotificationStore } from "./lib/notifications.js";
import { NotificationCenter } from "./components/notification-center.js";
import { initLoginModal } from "./components/login-modal.js";
import { applyStoredToken } from "./lib/auth.js";

const THEME_KEY = "remote-mic-theme";

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
    // Push a stored token into the clients before the first request so a
    // token-gated appliance loads without a prompt on a returning browser.
    applyStoredToken();
    void store.start().then((running) => {
      // Load the snapshot immediately too, independent of SSE connect timing.
      // When the login prompt is up instead, the stream's connect after login
      // re-syncs the snapshot.
      if (running) void notifications.load();
    });
  }

  // initTheme runs first in init, so a storage access that throws (a private
  // window, site data blocked) would abort the whole app before any view
  // exists. Both accesses fall back instead: dark theme, and a toggle that
  // works but does not persist.
  private initTheme(): void {
    let savedTheme = "dark";
    try {
      if (localStorage.getItem(THEME_KEY) === "light") savedTheme = "light";
    } catch {
      /* storage unavailable: keep the default theme */
    }
    document.documentElement.setAttribute("data-theme", savedTheme);

    const themeToggleBtn = document.getElementById("theme-toggle-btn");
    if (themeToggleBtn) {
      themeToggleBtn.addEventListener("click", () => {
        const currentTheme = document.documentElement.getAttribute("data-theme");
        const nextTheme = currentTheme === "light" ? "dark" : "light";
        document.documentElement.setAttribute("data-theme", nextTheme);
        try {
          localStorage.setItem(THEME_KEY, nextTheme);
        } catch {
          /* storage unavailable: the theme just does not persist */
        }
      });
    }
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
