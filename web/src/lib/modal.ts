// Shared modal primitives: a generic confirm dialog and the way to cancel the
// open ones. Modals are native <dialog> elements opened with showModal(), so the
// browser makes the page inert, keeps focus inside and returns it to the
// invoker; what a close means is in modal-core.ts.
import { CONFIRM_VALUE, TransientDialogs } from "./modal-core.ts";
import { button, h } from "./ui.ts";

// Monotonic counter giving each confirmDialog invocation unique element ids, so
// two dialogs cannot collide on aria-labelledby/aria-describedby targets.
let dialogSeq = 0;

// The confirm dialogs that are open. A higher-priority modal (the access-token
// prompt on a 401) cancels them through closeTransientDialogs() before showing.
const transient = new TransientDialogs();

// closeTransientDialogs cancels every open confirm dialog (resolving it false).
// The login prompt and the update modal call this before they open: if a
// confirm ("Allow open access?", "Discard changes?") is up when the session
// loses its credentials, cancelling it aborts that half-finished action, which
// is the right outcome, and leaves the other modal as the only one.
export function closeTransientDialogs(): void {
  transient.closeAll();
}

export interface ConfirmOptions {
  title: string;
  body: string;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
}

// confirmDialog shows a modal asking the user to confirm an action. It defaults
// to Cancel (focused) and closes on Escape or, where the browser supports
// closedby, a click outside it; the browser returns focus to the trigger.
// Resolves true only if the user confirms. It resolves in the dialog's close
// event, a task after the click, which no caller depends on.
export function confirmDialog(opts: ConfirmOptions): Promise<boolean> {
  const uid = ++dialogSeq;
  const titleId = `confirm-title-${uid}`;
  const descId = `confirm-desc-${uid}`;

  // Both buttons submit the form method="dialog", which closes the dialog with
  // the clicked button's value as its returnValue.
  const cancel = button({ variant: "secondary", label: opts.cancelLabel ?? "Cancel", type: "submit" });
  cancel.value = "cancel";
  cancel.autofocus = true;
  const confirm = button({ variant: opts.danger ? "danger" : "primary", label: opts.confirmLabel ?? "Confirm", type: "submit" });
  confirm.value = CONFIRM_VALUE;
  const dialog = h(
    "dialog",
    { class: "modal-dialog", closedby: "any", "aria-labelledby": titleId, "aria-describedby": descId },
    h(
      "div",
      { class: "modal-card" },
      h("h3", { class: "modal-title", id: titleId }, opts.title),
      h("p", { class: "modal-text", id: descId }, opts.body),
      h("form", { method: "dialog", class: "settings-actions" }, cancel, confirm),
    ),
  );
  document.body.appendChild(dialog);
  return transient.ask(dialog, () => dialog.remove());
}
