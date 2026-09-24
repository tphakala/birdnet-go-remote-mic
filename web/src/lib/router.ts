export type ViewName = "dashboard" | "events" | "system";

export class Router extends EventTarget {
  private currentView: ViewName = "dashboard";

  constructor() {
    super();
    window.addEventListener("hashchange", () => this.handleHashChange(true));
  }

  public init(): void {
    // The first route comes from the page load, not a navigation, so focus stays
    // where the browser put it.
    this.handleHashChange(false);
  }

  public getCurrentView(): ViewName {
    return this.currentView;
  }

  public navigate(view: ViewName): void {
    window.location.hash = `#/${view}`;
  }

  private handleHashChange(moveFocus: boolean): void {
    const rawHash = window.location.hash.replace(/^#\/?/, "");
    let view: ViewName = "dashboard";

    if (rawHash === "system" || rawHash === "events") {
      view = rawHash;
    }

    const changed = view !== this.currentView;
    this.currentView = view;
    this.updateDOM(view);
    this.dispatchEvent(new CustomEvent("route", { detail: view }));
    // A hash route swaps the content without a page load, so a keyboard or screen
    // reader user would otherwise be left on the nav link with no cue that the
    // page changed. Move focus to the main landmark (tabindex=-1), as a page load
    // would, but only on an actual view change.
    if (moveFocus && changed) document.getElementById("main-content")?.focus();
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

    // Update active state on nav items. aria-current tells assistive tech which
    // link is the current page; the class alone is only visual.
    document.querySelectorAll<HTMLElement>(".nav-item").forEach((btn) => {
      const current = btn.dataset.view === activeView;
      btn.classList.toggle("active", current);
      if (current) btn.setAttribute("aria-current", "page");
      else btn.removeAttribute("aria-current");
    });
  }
}

export const router = new Router();
