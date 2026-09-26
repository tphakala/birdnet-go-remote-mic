// The light/dark theme toggle and the live OS-preference follow. theme-init.ts
// has already applied the theme before the first paint (the saved choice, else
// prefers-color-scheme); initTheme adopts that attribute, owns the toggle, and,
// while no choice is saved, keeps following the OS preference as it changes (a
// phone or laptop that switches at dusk), since a dashboard is often left open
// for hours. A click is an explicit choice: it is saved and wins from then on.
//
// The browser objects are passed in so the logic runs under node:test with
// stubs; app.ts wires the real ones.

// theme-init.ts reads the same key before the first paint; it cannot import
// this, so test/theme-init.test.ts pins the two together.
export const THEME_KEY = "remote-mic-theme";

export type Theme = "light" | "dark";

// The media query theme-init.ts also consults (its test pins the two), so the
// first paint and the live follow read the preference alike.
export const PREFERS_LIGHT_QUERY = "(prefers-color-scheme: light)";

export interface ThemeRoot {
  getAttribute(name: string): string | null;
  setAttribute(name: string, value: string): void;
}

export interface ThemeToggle {
  title: string;
  setAttribute(name: string, value: string): void;
  addEventListener(type: "click", listener: () => void): void;
}

export interface ThemeStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export interface ThemeMedia {
  readonly matches: boolean;
  // Optional: MediaQueryList gained EventTarget methods only in Safari 14.
  addEventListener?(type: "change", listener: (e: { matches: boolean }) => void): void;
  removeEventListener?(type: "change", listener: (e: { matches: boolean }) => void): void;
}

export interface ThemeEnv {
  root: ThemeRoot;
  toggle: ThemeToggle | null;
  // A getter rather than the object: with site data blocked, Chromium throws on
  // reading localStorage itself, not only on getItem and setItem.
  storage: () => ThemeStorage;
  // The prefers-color-scheme: light query, or null when matchMedia is missing.
  media: ThemeMedia | null;
  // Called once per page when a toggle click cannot be saved, so the operator
  // learns why the choice will not survive a reload.
  onSaveFailed: () => void;
}

function savedTheme(storage: () => ThemeStorage): Theme | null {
  try {
    const v = storage().getItem(THEME_KEY);
    return v === "light" || v === "dark" ? v : null;
  } catch {
    return null;
  }
}

export function initTheme(env: ThemeEnv): void {
  const { root, toggle, media } = env;
  // applyTheme is the one place the theme changes, so the toggle's pressed
  // state (labelled "Dark theme": pressed means dark) never drifts from it. The
  // tooltip spells the state out, since the icon alone reads as either the
  // current theme or the one a click switches to.
  const applyTheme = (theme: Theme): void => {
    const dark = theme === "dark";
    if (root.getAttribute("data-theme") !== theme) root.setAttribute("data-theme", theme);
    toggle?.setAttribute("aria-pressed", String(dark));
    if (toggle) toggle.title = dark ? "Dark theme (on)" : "Dark theme (off)";
  };
  const current = (): Theme => (root.getAttribute("data-theme") === "light" ? "light" : "dark");
  applyTheme(current());

  // Follow the OS only while nothing is saved; a blocked storage reads as
  // nothing saved, the same way theme-init.ts treats it.
  let following = savedTheme(env.storage) === null;
  const onChange = (e: { matches: boolean }): void => {
    if (following) applyTheme(e.matches ? "light" : "dark");
  };
  if (following) {
    try {
      media?.addEventListener?.("change", onChange);
    } catch {
      /* no live follow: the theme still tracks the OS on the next load */
    }
  }

  let saveWarned = false;
  toggle?.addEventListener("click", () => {
    const next: Theme = current() === "light" ? "dark" : "light";
    applyTheme(next);
    if (following) {
      following = false;
      try {
        media?.removeEventListener?.("change", onChange);
      } catch {
        /* the following flag already ignores later changes */
      }
    }
    try {
      env.storage().setItem(THEME_KEY, next);
    } catch {
      // The theme still switches for this visit; say once why it will not stick.
      if (!saveWarned) {
        saveWarned = true;
        env.onSaveFailed();
      }
    }
  });
}
