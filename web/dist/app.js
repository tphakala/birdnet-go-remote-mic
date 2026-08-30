import { router } from "./lib/router.js";
import { store } from "./lib/store.js";
import { DashboardView } from "./views/dashboard.js";
import { SystemView } from "./views/system.js";
import { triggerApplianceRestart } from "./components/restart-modal.js";
class App {
    init() {
        this.initTheme();
        this.initNav();
        this.initViews();
        router.init();
        store.loadInitial();
        store.startPolling();
    }
    initTheme() {
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
    initNav() {
        document.querySelectorAll(".nav-item").forEach((btn) => {
            btn.addEventListener("click", () => {
                const view = btn.dataset.view;
                if (view === "dashboard" || view === "system") {
                    router.navigate(view);
                }
            });
        });
        const restartBtn = document.getElementById("header-restart-btn");
        if (restartBtn)
            restartBtn.addEventListener("click", () => triggerApplianceRestart());
    }
    initViews() {
        new DashboardView();
        new SystemView();
    }
}
// Boot application on DOM ready
document.addEventListener("DOMContentLoaded", () => {
    const app = new App();
    app.init();
});
