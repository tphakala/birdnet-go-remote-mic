// What a modal's close means, with no DOM: the logic behind the native
// <dialog> modals (lib/modal.ts, components/login-modal.ts,
// components/restart-modal.ts). The browser supplies the top layer, the inert
// page, focus containment and Escape; what is left to decide in script is
// whether a close was a confirmation, and keeping a modal open that only the
// app may close. Tests drive both over a fake dialog (test/modal-core.test.ts).

// DialogPort is the slice of HTMLDialogElement the modal logic drives.
export interface DialogPort {
  readonly open: boolean;
  returnValue: string;
  showModal(): void;
  close(returnValue?: string): void;
  addEventListener(type: "cancel" | "close", listener: (ev: { preventDefault(): void }) => void): void;
}

// CONFIRM_VALUE is the returnValue of a confirm dialog's confirming button.
export const CONFIRM_VALUE = "confirm";

// TransientDialogs tracks the confirm dialogs that are open, so a higher
// priority modal can cancel them.
export class TransientDialogs {
  private readonly open = new Set<DialogPort>();

  // ask shows dialog as a modal and resolves once it has closed: true only when
  // it closed with CONFIRM_VALUE, false for every other close (Cancel, Escape,
  // a click outside, closeAll). dispose runs once after the close.
  ask(dialog: DialogPort, dispose: () => void = () => {}): Promise<boolean> {
    return new Promise((resolve) => {
      // A dialog that is already open would get a second close listener (a
      // browser's showModal returns quietly for an open modal), and showModal
      // throws on a disconnected one. Both reject the promise before anything
      // is attached or tracked; the close event is delivered in a later task,
      // so the listener below is in place before it.
      if (dialog.open) throw new Error("ask: the dialog is already open");
      dialog.showModal();
      this.open.add(dialog);
      dialog.addEventListener("close", () => {
        this.open.delete(dialog);
        const confirmed = dialog.returnValue === CONFIRM_VALUE;
        dialog.returnValue = "";
        dispose();
        resolve(confirmed);
      });
    });
  }

  // closeAll closes every open dialog without confirming it. A dialog already
  // confirmed has closed, so it is not touched and its result stays true.
  closeAll(): void {
    for (const dialog of this.open) dialog.close();
  }
}

// HeldDialog keeps a modal open from show() until hide(): the access-token
// prompt and the restart modal, which only the app may close. Three mechanisms
// stand behind that. closedby="none" in the markup stops Escape where the
// browser has the attribute. Elsewhere the cancel event is refused. A browser
// may still ignore that for a repeated Escape without a user activation, so a
// close that hide() did not ask for reopens the dialog, its content untouched
// (the entry animation replays; accepted for a rare fallback).
export class HeldDialog {
  private wanted = false;
  private readonly dialog: DialogPort;

  // Listeners are attached here, so build one HeldDialog per element.
  constructor(dialog: DialogPort) {
    this.dialog = dialog;
    dialog.addEventListener("cancel", (ev) => ev.preventDefault());
    dialog.addEventListener("close", () => {
      if (this.wanted && !dialog.open) dialog.showModal();
    });
  }

  // shown reports whether the app wants the dialog open.
  get shown(): boolean {
    return this.wanted;
  }

  show(): void {
    this.wanted = true;
    if (!this.dialog.open) this.dialog.showModal();
  }

  hide(): void {
    this.wanted = false;
    if (this.dialog.open) this.dialog.close();
  }
}
