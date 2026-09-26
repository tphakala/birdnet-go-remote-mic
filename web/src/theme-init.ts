// Applies the saved (or OS-preferred) theme before the first paint. index.html
// loads this as a classic script in <head>: app.js is a module, and module
// scripts are deferred, so a theme applied only there would paint one frame in
// the hardcoded dark theme first. It cannot be inline either, since the CSP
// allows scripts from 'self' only. This file must stay a script (no import or
// export), and it must never throw: the page renders even if it fails.
//
// lib/theme.ts initTheme derives the theme again the same way (THEME_KEY and
// PREFERS_LIGHT_QUERY; a test pins both), so it corrects an OS change between
// the two scripts or a theme-init.js that never ran, and reads the attribute set
// here only when matchMedia is missing. It owns the mode the header theme menu
// sets (app.ts binds the menu): a Light or Dark choice is saved under the same
// key, System removes it, and it follows later OS changes.
(() => {
  let saved: string | null = null;
  try {
    saved = localStorage.getItem("remote-mic-theme");
  } catch {
    /* storage blocked: treat as no saved choice */
  }
  let theme = "dark";
  if (saved === "light" || saved === "dark") {
    theme = saved;
  } else {
    // No saved choice (or none readable): follow the OS preference.
    try {
      if (window.matchMedia("(prefers-color-scheme: light)").matches) theme = "light";
    } catch {
      /* matchMedia unavailable: keep the dark default */
    }
  }
  document.documentElement.setAttribute("data-theme", theme);
})();
