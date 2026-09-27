// Text helpers shared by DOM-free cores and views. It imports only DOM-free
// modules, so a core can use it without pulling in the DOM.

import { DEVICE_FIELD_LABELS } from "./device-settings-core.ts";

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

// deviceIdTitle names a device's stable id, as a title or a visible label,
// under the settings form's label for it.
export function deviceIdTitle(id: string): string {
  return `${DEVICE_FIELD_LABELS.device}: ${id}`;
}
