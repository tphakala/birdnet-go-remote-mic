// FilterChips is a reusable multi-select toggle group: one pressable chip per
// option, each with an optional live count. An empty selection means "all", the
// convention the Events page filters use. The group owns its DOM and reports the
// new selection through onChange; the caller owns the filter state and pushes
// counts and selection back in. The component keeps only a mirror of the current
// selection, used to drive its own pressed and dimmed styling.

import { elem, setText } from "../lib/ui.js";
import type { ToastType } from "./toast.js";

// Module counter for unique label ids, so each group's aria-labelledby points at
// its own label (mirrors custom-dropdown's dropdownSeq).
let chipsSeq = 0;

export interface ChipOption {
  value: string;
  label: string;
  // tone tints the chip's leading dot (a severity), omitted for neutral facets.
  tone?: ToastType;
}

export interface FilterChipsOptions {
  // label names the group for assistive tech ("Severity", "Category").
  label: string;
  options: readonly ChipOption[];
  onChange: (selected: Set<string>) => void;
}

interface ChipRefs {
  btn: HTMLButtonElement;
  count: HTMLElement;
}

export class FilterChips {
  public readonly el: HTMLElement;
  private readonly onChange: (selected: Set<string>) => void;
  private readonly chips = new Map<string, ChipRefs>();
  private selected = new Set<string>();

  constructor(opts: FilterChipsOptions) {
    this.onChange = opts.onChange;
    this.el = elem("div", "filter-chips");
    this.el.setAttribute("role", "group");
    const labelEl = elem("span", "filter-chips-label", opts.label);
    labelEl.id = `filter-chips-label-${++chipsSeq}`;
    this.el.setAttribute("aria-labelledby", labelEl.id);
    this.el.append(labelEl);
    for (const o of opts.options) this.addChip(o);
  }

  private addChip(o: ChipOption): void {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "filter-chip";
    btn.setAttribute("aria-pressed", "false");
    btn.dataset.value = o.value;
    if (o.tone) {
      const dot = elem("span", `filter-chip-dot tone-${o.tone}`);
      dot.setAttribute("aria-hidden", "true");
      btn.append(dot);
    }
    btn.append(elem("span", "filter-chip-label", o.label));
    const count = elem("span", "filter-chip-count mono", "0");
    btn.append(count);
    btn.addEventListener("click", () => this.toggle(o.value));
    this.el.append(btn);
    this.chips.set(o.value, { btn, count });
  }

  private toggle(value: string): void {
    const next = new Set(this.selected);
    if (next.has(value)) next.delete(value);
    else next.add(value);
    this.setSelected(next);
    this.onChange(new Set(next));
  }

  // setSelected reflects an externally changed selection (a reset, a tile click)
  // without firing onChange.
  public setSelected(values: ReadonlySet<string>): void {
    this.selected = new Set(values);
    for (const [value, refs] of this.chips) {
      const on = this.selected.has(value);
      refs.btn.setAttribute("aria-pressed", on ? "true" : "false");
      refs.btn.classList.toggle("is-on", on);
    }
  }

  // setCounts updates the per-chip counts. A value the group has not seen yet
  // (a category added server-side) gets a chip appended on the fly. A chip with
  // a zero count that is not selected is dimmed, never removed, so the layout
  // does not jump as events arrive.
  public setCounts(counts: ReadonlyMap<string, number>): void {
    for (const value of counts.keys()) {
      if (!this.chips.has(value)) this.addChip({ value, label: value });
    }
    for (const [value, refs] of this.chips) {
      const n = counts.get(value) ?? 0;
      setText(refs.count, String(n));
      refs.btn.classList.toggle("is-empty", n === 0 && !this.selected.has(value));
    }
  }
}
