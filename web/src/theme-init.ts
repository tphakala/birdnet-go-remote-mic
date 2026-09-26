// Applies the saved theme before the first paint. index.html loads this as a
// classic script in <head>: app.js is a module, and module scripts are
// deferred, so a theme applied only there would paint one frame in the
// hardcoded dark theme first. It cannot be inline either, since the CSP allows
// scripts from 'self' only. This file must stay a script (no import or
// export), and it must never throw: the page renders even if it fails.
//
// app.ts initTheme reads the attribute set here and owns the toggle, which
// saves the choice under the same key.
(() => {
  let theme = "dark";
  try {
    const saved = localStorage.getItem("remote-mic-theme");
    if (saved === "light" || saved === "dark") theme = saved;
    // No saved choice yet: follow the OS preference.
    else if (window.matchMedia("(prefers-color-scheme: light)").matches) theme = "light";
  } catch {
    /* storage or matchMedia unavailable: keep the dark default */
  }
  document.documentElement.setAttribute("data-theme", theme);
})();
