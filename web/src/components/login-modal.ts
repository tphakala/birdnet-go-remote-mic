// The access-token prompt. It opens when the store reports that the appliance
// rejected the UI's credentials (or it has none) as a native modal dialog, and
// closes once a token is accepted. There is deliberately no Escape or backdrop
// dismissal: nothing on the page works without a token (HeldDialog).
import { store } from "../lib/store.ts";
import { closeTransientDialogs } from "../lib/modal.ts";
import { HeldDialog } from "../lib/modal-core.ts";
import { focusWorkspace } from "../lib/ui.ts";

export function initLoginModal(): void {
  const dialog = document.getElementById("login-modal");
  const form = document.getElementById("login-form") as HTMLFormElement | null;
  const input = document.getElementById("login-token") as HTMLInputElement | null;
  const error = document.getElementById("login-error");
  const submit = document.getElementById("login-submit") as HTMLButtonElement | null;
  if (!(dialog instanceof HTMLDialogElement) || !form || !input || !error || !submit) return;

  const held = new HeldDialog(dialog);

  const show = (): void => {
    if (held.shown) return;
    // A confirm dialog (e.g. "Allow open access?" from a token save) can be open
    // when a 401 arrives. Cancelling it aborts that half-finished action and
    // leaves this prompt as the only modal.
    closeTransientDialogs();
    error.textContent = "";
    input.value = "";
    held.show();
  };

  const hide = (): void => {
    if (!held.shown) return;
    held.hide();
    // The close returned focus to whatever had it before the prompt. Move it to
    // the workspace instead; if another modal is still open the page is inert,
    // this focus() does nothing, and focus stays in that modal. preventScroll
    // keeps the page where the operator left it: a plain focus() on the tall
    // landmark would jump to the top of <main>.
    focusWorkspace();
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
        if (held.shown) input.focus();
      }
    })();
  });

  store.on("authrequired", show);
  store.on("authok", hide);
}
