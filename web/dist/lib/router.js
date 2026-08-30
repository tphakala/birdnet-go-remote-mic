export class Router extends EventTarget {
    currentView = "dashboard";
    constructor() {
        super();
        window.addEventListener("hashchange", () => this.handleHashChange());
    }
    init() {
        this.handleHashChange();
    }
    getCurrentView() {
        return this.currentView;
    }
    navigate(view) {
        window.location.hash = `#/${view}`;
    }
    handleHashChange() {
        const rawHash = window.location.hash.replace(/^#\/?/, "");
        let view = "dashboard";
        if (rawHash === "system") {
            view = rawHash;
        }
        this.currentView = view;
        this.updateDOM(view);
        this.dispatchEvent(new CustomEvent("route", { detail: view }));
    }
    updateDOM(activeView) {
        // Hide all view containers and show the active one
        document.querySelectorAll(".view-container").forEach((el) => {
            const viewAttr = el.id.replace("view-", "");
            if (viewAttr === activeView) {
                el.style.display = "flex";
            }
            else {
                el.style.display = "none";
            }
        });
        // Update active state on nav items
        document.querySelectorAll(".nav-item").forEach((btn) => {
            const btnView = btn.dataset.view;
            if (btnView === activeView) {
                btn.classList.add("active");
            }
            else {
                btn.classList.remove("active");
            }
        });
    }
}
export const router = new Router();
