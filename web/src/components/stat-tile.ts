// StatTile is a reusable summary tile: a label, a large value, and a short
// caption, optionally toned (a severity) and optionally pressable. A pressable
// tile renders as a toggle button so it can double as a filter shortcut; a plain
// tile is a static div. set() writes the value and caption only on change
// (setText), so a tile can be re-synced on every store change without churning the
// DOM.

import { elem, setText } from "../lib/ui.js";

export type TileTone = "error" | "warn" | "info" | "neutral";

export interface StatTileOptions {
  label: string;
  tone?: TileTone;
  onClick?: () => void;
}

export class StatTile {
  public readonly el: HTMLElement;
  private readonly valueEl: HTMLElement;
  private readonly captionEl: HTMLElement;
  private tone: TileTone;

  constructor(opts: StatTileOptions) {
    const tone = opts.tone ?? "neutral";
    this.tone = tone;
    if (opts.onClick) {
      const b = document.createElement("button");
      b.type = "button";
      b.setAttribute("aria-pressed", "false");
      b.addEventListener("click", opts.onClick);
      this.el = b;
    } else {
      this.el = document.createElement("div");
    }
    this.el.className = `stat-tile tone-${tone}${opts.onClick ? " is-action" : ""}`;
    this.el.append(elem("span", "stat-tile-label", opts.label));
    this.valueEl = elem("span", "stat-tile-value mono", "-");
    this.captionEl = elem("span", "stat-tile-caption");
    this.el.append(this.valueEl, this.captionEl);
  }

  public set(value: string, caption = ""): void {
    setText(this.valueEl, value);
    setText(this.captionEl, caption);
  }

  // setTone changes the tile's tone, for a count whose meaning flips with its
  // value (zero active issues is good news, not an error). Writes only on change.
  public setTone(tone: TileTone): void {
    if (tone === this.tone) return;
    this.el.classList.replace(`tone-${this.tone}`, `tone-${tone}`);
    this.tone = tone;
  }

  // setPressed reflects whether the filter this tile toggles is active.
  public setPressed(on: boolean): void {
    if (!this.el.hasAttribute("aria-pressed")) return;
    this.el.setAttribute("aria-pressed", on ? "true" : "false");
    this.el.classList.toggle("is-on", on);
  }

  // setAlert toggles the attention treatment (a lit border) for a non-zero
  // count that needs an operator, such as active issues.
  public setAlert(on: boolean): void {
    this.el.classList.toggle("is-alert", on);
  }
}
