// FilterChips is a reusable multi-select toggle group: one pressable chip per
// option, each with an optional live count. An empty selection means "all", the
// convention the Events page filters use. The group owns its DOM and reports the
// new selection through onChange; the caller owns the filter state and pushes
// counts and selection back in. The component keeps only a mirror of the current
// selection, used to drive its own pressed and dimmed styling.

import { h, setText } from "../lib/ui.ts";
import type { ToastType } from "./toast.ts";

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
    const labelId = `filter-chips-label-${++chipsSeq}`;
    this.el = h("div", { class: "filter-chips", role: "group", "aria-labelledby": labelId }, h("span", { class: "filter-chips-label", id: labelId }, opts.label));
    for (const o of opts.options) this.addChip(o);
  }

  private addChip(o: ChipOption): void {
    const count = h("span", { class: "filter-chip-count mono" }, "0");
    const btn = h(
      "button",
      { type: "button", class: "filter-chip", "aria-pressed": "false", "data-value": o.value },
      o.tone && h("span", { class: `filter-chip-dot tone-${o.tone}`, "aria-hidden": "true" }),
      h("span", { class: "filter-chip-label" }, o.label),
      count,
    );
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
