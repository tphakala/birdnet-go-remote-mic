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

// Per-device meter display preference: "hide inactive channels". It is a per-
// viewer view option, not appliance config, so it lives in localStorage keyed by
// the stable device id rather than in the saved config.

// HIDE_INACTIVE_PREFIX starts every "hide inactive channels" storage key; the
// device id follows it.
export const HIDE_INACTIVE_PREFIX = "remote-mic-hide-inactive:";

export function hideInactiveKey(deviceId: string): string {
  return `${HIDE_INACTIVE_PREFIX}${deviceId}`;
}

// hideInactivePrefDevice returns the device id a storage key belongs to when it
// is a "hide inactive channels" key, else null.
export function hideInactivePrefDevice(key: string | null): string | null {
  return key !== null && key.startsWith(HIDE_INACTIVE_PREFIX) ? key.slice(HIDE_INACTIVE_PREFIX.length) : null;
}

// parseBoolPref reads a stored boolean preference ("1" on, "0" off) with the
// default for anything else, including a removed key (null).
export function parseBoolPref(v: string | null, fallback: boolean): boolean {
  return v === "1" ? true : v === "0" ? false : fallback;
}

// readBoolPref and writeBoolPref are wrapped because localStorage can throw
// (site data blocked, storage full): a failed read falls back to the default,
// and a failed write does not persist and raises prefSaveNotice once per page.
export function readBoolPref(key: string, fallback: boolean): boolean {
  try {
    return parseBoolPref(localStorage.getItem(key), fallback);
  } catch {
    return fallback;
  }
}

export function writeBoolPref(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? "1" : "0");
  } catch {
    prefSaveNotice.report();
  }
}

// isLocalStorageEvent reports whether a storage event is a localStorage change
// (a clear in another tab arrives with key null and this window's
// localStorage as its area), not a sessionStorage one. A null area only occurs
// on an event built without one, which is let through. Reading localStorage
// can itself throw where site data is blocked.
export function isLocalStorageEvent(e: StorageEvent): boolean {
  try {
    return e.storageArea === null || e.storageArea === window.localStorage;
  } catch {
    return false;
  }
}
