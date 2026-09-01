import { router } from "./lib/router.js";
import { store } from "./lib/store.js";
import { DashboardView } from "./views/dashboard.js";
import { SystemView } from "./views/system.js";
import { triggerApplianceRestart } from "./components/restart-modal.js";
import { initLoginModal } from "./components/login-modal.js";
import { applyStoredToken } from "./lib/auth.js";

class App {
  public init(): void {
    this.initTheme();
    this.initNav();
    this.initViews();
    initLoginModal();

    router.init();
    // Push a stored token into the clients before the first request so a
    // token-gated appliance loads without a prompt on a returning browser.
    applyStoredToken();
    store.loadInitial();
    store.startPolling();
  }

  private initTheme(): void {
    const savedTheme = localStorage.getItem("remote-mic-theme") || "dark";
    document.documentElement.setAttribute("data-theme", savedTheme);

    const themeToggleBtn = document.getElementById("theme-toggle-btn");
    if (themeToggleBtn) {
      themeToggleBtn.addEventListener("click", () => {
        const currentTheme = document.documentElement.getAttribute("data-theme");
        const nextTheme = currentTheme === "light" ? "dark" : "light";
        document.documentElement.setAttribute("data-theme", nextTheme);
        localStorage.setItem("remote-mic-theme", nextTheme);
      });
    }
  }

  private initNav(): void {
    document.querySelectorAll<HTMLElement>(".nav-item").forEach((btn) => {
      btn.addEventListener("click", () => {
        const view = btn.dataset.view;
        if (view === "dashboard" || view === "system") {
          router.navigate(view);
        }
      });
    });

    const restartBtn = document.getElementById("header-restart-btn");
    if (restartBtn) restartBtn.addEventListener("click", () => triggerApplianceRestart());
  }

  private initViews(): void {
    new DashboardView();
    new SystemView();
  }
}

// Boot application on DOM ready
document.addEventListener("DOMContentLoaded", () => {
  const app = new App();
  app.init();
});
