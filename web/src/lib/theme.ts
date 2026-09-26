// The theme mode (System, Light or Dark) and the live OS-preference follow.
// theme-init.ts has already applied the theme before the first paint (the saved
// choice, else prefers-color-scheme); initTheme re-derives it the same way and
// owns the mode from then on. In System mode (nothing saved) the theme follows
// the OS preference as it changes (a phone or laptop that switches at dusk),
// since a dashboard is often left open for hours. Light or Dark is an explicit
// choice: it is saved when the browser allows (otherwise it lasts this visit
// and a warning says why) and wins on every later load. Choosing System again
// clears the saved choice, so the OS preference applies once more. A mode
// chosen in another tab reaches this one at once through the storage event.
//
// The browser objects are passed in so the logic runs under node:test with
// stubs; app.ts wires the real ones and the header menu.

// theme-init.ts reads the same key before the first paint; it cannot import
// this, so test/theme-init.test.ts pins the two together.
export const THEME_KEY = "remote-mic-theme";

export type Theme = "light" | "dark";

// ThemeMode is what the operator picks; "system" means nothing is saved and the
// OS preference decides the Theme.
export type ThemeMode = "system" | Theme;

// The media query theme-init.ts also consults (its test pins the two), so the
// first paint and the live follow read the preference alike.
export const PREFERS_LIGHT_QUERY = "(prefers-color-scheme: light)";

export interface ThemeRoot {
  getAttribute(name: string): string | null;
  setAttribute(name: string, value: string): void;
}

export interface ThemeStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export interface ThemeMedia {
  readonly matches: boolean;
  // Optional: MediaQueryList gained EventTarget methods only in Safari 14.
  addEventListener?(type: "change", listener: (e: { matches: boolean }) => void): void;
}

// The part of a StorageEvent the cross-tab sync reads. key is null when another
// tab cleared all of storage.
export interface ThemeStorageChange {
  key: string | null;
  newValue: string | null;
}

export interface ThemeEnv {
  root: ThemeRoot;
  // A getter rather than the object: with site data blocked, Chromium throws on
  // reading localStorage itself, not only on getItem and setItem.
  storage: () => ThemeStorage;
  // The prefers-color-scheme: light query, or null when matchMedia is missing.
  media: ThemeMedia | null;
  // Registers for storage changes made in other tabs (window's storage event).
  onStorage?: (listener: (e: ThemeStorageChange) => void) => void;
  // Called on every applied change (and once at load), so a control can show
  // the mode and the theme it resolves to.
  onApply?: (mode: ThemeMode, theme: Theme) => void;
  // Called once per page when a chosen mode cannot be saved, so the operator
  // learns why the choice will not survive a reload.
  onSaveFailed: () => void;
}

export interface ThemeController {
  mode(): ThemeMode;
  setMode(mode: ThemeMode): void;
}

// parseMode reads a stored value: a saved light or dark choice, anything else
// (nothing saved, or an unrecognized value) is System.
export function parseMode(v: string | null): ThemeMode {
  return v === "light" || v === "dark" ? v : "system";
}

function savedMode(storage: () => ThemeStorage): ThemeMode {
  try {
    return parseMode(storage().getItem(THEME_KEY));
  } catch {
    // Blocked storage reads as nothing saved, the same way theme-init.ts does.
    return "system";
  }
}

export function initTheme(env: ThemeEnv): ThemeController {
  const { root, media } = env;
  let mode = savedMode(env.storage);
  // The OS preference as last reported: the query at load, then each change
  // event, tracked in every mode so a later switch to System starts from the
  // current value. null without matchMedia.
  let prefersLight: boolean | null = media ? media.matches : null;

  // resolve names the theme a mode shows. System follows the OS; without
  // matchMedia it keeps the attribute's theme (dark whether or not theme-init
  // ran, since index.html hardcodes dark), so a load in that case writes nothing.
  const resolve = (m: ThemeMode): Theme => {
    if (m !== "system") return m;
    if (prefersLight !== null) return prefersLight ? "light" : "dark";
    return root.getAttribute("data-theme") === "light" ? "light" : "dark";
  };
  // apply is the one place the theme changes, so a control never drifts from it.
  const apply = (): void => {
    const theme = resolve(mode);
    // Skip an unchanged write: even a same-value setAttribute queues a mutation
    // record, which wakes vu-meter's observer.
    if (root.getAttribute("data-theme") !== theme) root.setAttribute("data-theme", theme);
    env.onApply?.(mode, theme);
  };
  // Derive the load theme the way theme-init.ts did rather than trusting the
  // attribute: normally it is the same value, and this corrects an OS change
  // between the two scripts, or a theme-init.js that never ran.
  apply();

  // The listener stays for the page's life; it acts only in System mode, so a
  // later switch back to System follows the OS again with no re-registration.
  try {
    media?.addEventListener?.("change", (e) => {
      prefersLight = e.matches;
      if (mode === "system") apply();
    });
  } catch {
    /* no live follow: the theme still tracks the OS on the next load */
  }

  // Another tab chose a mode: show it here too. Only this key (or a clear of all
  // storage) matters; the write was the other tab's, so nothing is saved here.
  env.onStorage?.((e) => {
    if (e.key !== null && e.key !== THEME_KEY) return;
    const next = parseMode(e.newValue);
    if (next === mode) return;
    mode = next;
    apply();
  });

  let saveWarned = false;
  return {
    mode: () => mode,
    setMode(next: ThemeMode): void {
      mode = next;
      apply();
      try {
        if (next === "system") env.storage().removeItem(THEME_KEY);
        else env.storage().setItem(THEME_KEY, next);
      } catch {
        // The mode still applies for this visit; say once why it will not stick.
        if (!saveWarned) {
          saveWarned = true;
          env.onSaveFailed();
        }
      }
    },
  };
}
