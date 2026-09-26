// Per-browser preferences (the theme, hidden meter channels, the Events slash
// shortcut, read and dismissed notifications) live in localStorage, which a
// browser with site data blocked or storage full can refuse. The UI keeps
// working either way, but a choice that quietly resets on reload looks like a
// bug, so the first failed save of a choice the operator just made raises one
// notice covering them all, and later failures stay quiet. Automatic writes (a
// notification snapshot or live event) and a System theme choice never report.

export class OnceNotice {
  private fired = false;
  private handler: (() => void) | null = null;

  // setHandler installs what report shows; app.ts wires the toast.
  public setHandler(handler: () => void): void {
    this.handler = handler;
  }

  // report shows the notice the first time it is called with a handler set;
  // a report before then is not counted, so an early failure is not swallowed.
  public report(): void {
    if (this.fired || !this.handler) return;
    this.fired = true;
    this.handler();
  }
}

export const prefSaveNotice = new OnceNotice();
