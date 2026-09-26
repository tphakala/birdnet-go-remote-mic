// Pure route decisions for the hash router (lib/router.ts), which adds a window
// listener on import and so cannot load in node:test. No DOM here.

export type ViewName = "dashboard" | "events" | "system" | "about";

// The document title per route, so a browser tab, history entry and a screen
// reader's page announcement all name the page being shown.
const APP_TITLE = "BirdNET-Go Remote Mic";
const VIEW_TITLES: Record<ViewName, string> = {
  dashboard: "Dashboard",
  events: "Events",
  system: "System",
  about: "About",
};

// isViewName narrows a hash fragment or a nav item's data-view to a route. An
// own-property check, so an inherited name such as toString or __proto__ from
// a crafted hash is not taken for a route.
export function isViewName(v: string | undefined): v is ViewName {
  return v !== undefined && Object.hasOwn(VIEW_TITLES, v);
}

export function documentTitle(view: ViewName): string {
  return `${VIEW_TITLES[view]} - ${APP_TITLE}`;
}
