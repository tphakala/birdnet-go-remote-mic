// The access-token prompt. It opens when the store reports that the appliance
// rejected the UI's credentials (or it has none), traps focus like the other
// modals, and closes once a token is accepted. There is deliberately no Escape
// or backdrop dismissal: nothing on the page works without a token.
import { store } from "../lib/store.js";
import { closeTransientDialogs, setAppInert, trapFocus } from "../lib/modal.js";

export function initLoginModal(): void {
  const overlay = document.getElementById("login-modal");
  const form = document.getElementById("login-form") as HTMLFormElement | null;
  const input = document.getElementById("login-token") as HTMLInputElement | null;
  const error = document.getElementById("login-error");
  const submit = document.getElementById("login-submit") as HTMLButtonElement | null;
  if (!overlay || !form || !input || !error || !submit) return;

  let release: (() => void) | null = null;
  let open = false;

  const show = (): void => {
    if (open) return;
    // A confirm dialog (e.g. "Allow open access?" from a token save) can be open
    // when a 401 arrives. Two modals would compete for focus and both inert the
    // background, and this prompt's own hide() could later hand focus to a still
    // -inert page. Close any transient dialog first so this is the only modal.
    closeTransientDialogs();
    open = true;
    error.textContent = "";
    input.value = "";
    overlay.classList.add("open");
    setAppInert(true);
    release = trapFocus(overlay);
    input.focus();
  };

  const hide = (): void => {
    if (!open) return;
    open = false;
    overlay.classList.remove("open");
    release?.();
    release = null;
    setAppInert(false);
    // Only pull focus into the workspace when the background is actually
    // interactive again. If another modal still holds .app-container inert
    // (depth > 0), focusing an inert descendant would silently fail and strand
    // focus, so leave it for that modal to place. preventScroll keeps the page
    // where the operator left it: a plain focus() on the tall landmark would jump
    // to the top of <main>.
    const app = document.querySelector<HTMLElement>(".app-container");
    if (!app?.hasAttribute("inert")) document.getElementById("main-content")?.focus({ preventScroll: true });
  };

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    void (async () => {
      const token = input.value.trim();
      if (!token) {
        error.textContent = "Enter the access token.";
        input.focus();
        return;
      }
      // Capture the button's own label rather than hard-coding "Unlock", so the
      // busy text is restored to whatever the markup actually uses.
      const origLabel = submit.textContent ?? "Unlock";
      submit.disabled = true;
      submit.setAttribute("aria-busy", "true");
      submit.textContent = "Checking...";
      try {
        const result = await store.login(token);
        if (!result.ok) {
          error.textContent = result.message;
          input.select();
        }
      } catch {
        // store.login resolves its own errors to {ok:false}; this guards an
        // unexpected throw so the prompt reports a failure rather than hanging on
        // "Checking...".
        error.textContent = "Could not reach the appliance.";
        input.select();
      } finally {
        submit.disabled = false;
        submit.removeAttribute("aria-busy");
        submit.textContent = origLabel;
        // Disabling the focused submit button dropped focus to the body; put it
        // back on the field so a retry is one keystroke away.
        if (open) input.focus();
      }
    })();
  });

  store.addEventListener("authrequired", show);
  store.addEventListener("authok", hide);
}
