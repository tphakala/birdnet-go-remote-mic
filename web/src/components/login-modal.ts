// The access-token prompt. It opens when the store reports that the appliance
// rejected the UI's credentials (or it has none), traps focus like the other
// modals, and closes once a token is accepted. There is deliberately no Escape
// or backdrop dismissal: nothing on the page works without a token.
import { store } from "../lib/store.js";
import { setAppInert, trapFocus } from "../lib/modal.js";

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
    document.getElementById("main-content")?.focus();
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
      submit.disabled = true;
      submit.setAttribute("aria-busy", "true");
      submit.textContent = "Checking...";
      try {
        const result = await store.login(token);
        if (!result.ok) {
          error.textContent = result.message;
          input.select();
        }
      } finally {
        submit.disabled = false;
        submit.removeAttribute("aria-busy");
        submit.textContent = "Unlock";
        // Re-enabling a disabled focused control drops focus to the body; put
        // it back on the field so a retry is one keystroke away.
        if (open) input.focus();
      }
    })();
  });

  store.addEventListener("authrequired", show);
  store.addEventListener("authok", hide);
}
