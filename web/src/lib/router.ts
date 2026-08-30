export type ViewName = "dashboard" | "system";

export class Router extends EventTarget {
  private currentView: ViewName = "dashboard";

  constructor() {
    super();
    window.addEventListener("hashchange", () => this.handleHashChange());
  }

  public init(): void {
    this.handleHashChange();
  }

  public getCurrentView(): ViewName {
    return this.currentView;
  }

  public navigate(view: ViewName): void {
    window.location.hash = `#/${view}`;
  }

  private handleHashChange(): void {
    const rawHash = window.location.hash.replace(/^#\/?/, "");
    let view: ViewName = "dashboard";

    if (rawHash === "system") {
      view = rawHash;
    }

    this.currentView = view;
    this.updateDOM(view);
    this.dispatchEvent(new CustomEvent("route", { detail: view }));
  }

  private updateDOM(activeView: ViewName): void {
    // Hide all view containers and show the active one
    document.querySelectorAll<HTMLElement>(".view-container").forEach((el) => {
      const viewAttr = el.id.replace("view-", "");
      if (viewAttr === activeView) {
        el.style.display = "flex";
      } else {
        el.style.display = "none";
      }
    });

    // Update active state on nav items
    document.querySelectorAll<HTMLElement>(".nav-item").forEach((btn) => {
      const btnView = btn.dataset.view;
      if (btnView === activeView) {
        btn.classList.add("active");
      } else {
        btn.classList.remove("active");
      }
    });
  }
}

export const router = new Router();
