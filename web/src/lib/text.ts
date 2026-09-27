// Text helpers shared by DOM-free cores and views. A leaf module, so a core
// can use it without pulling in the DOM.

// sentence turns a backend message (a Go error string: lowercase, often no
// final stop) into a sentence of its own: first letter capitalised, and a
// period added unless it already ends in one (a trailing colon becomes the
// period). Empty stays empty.
export function sentence(msg: string | undefined): string {
  const t = (msg ?? "").trim().replace(/:$/, "");
  if (!t) return "";
  const s = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.!?]$/.test(s) ? s : `${s}.`;
}

// deviceIdTitle is the hover text that names a device's stable id.
export function deviceIdTitle(id: string): string {
  return `Device ID: ${id}`;
}
