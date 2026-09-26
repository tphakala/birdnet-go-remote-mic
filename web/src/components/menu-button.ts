// MenuButton turns an existing button into a menu button with a single-choice
// menu (the WAI-ARIA menu button pattern with menuitemradio items): the button
// opens it (click, Enter, Space, ArrowDown to the checked item, ArrowUp to the
// last), arrows, Home and End move between items, Enter or Space picks one,
// and Escape, Tab or a click outside closes it. Picking returns focus to the
// button. The caller owns the value: onSelect reports the pick, and set()
// shows a value, whoever changed it.
import { elem, iconSpan } from "../lib/ui.js";

export interface MenuChoice {
  value: string;
  label: string;
  // Static, trusted inline SVG markup (an ICON_* constant), shown before the label.
  icon?: string;
}

export interface MenuButtonOptions {
  // The accessible name of the menu itself (the button keeps its own label).
  label: string;
  choices: MenuChoice[];
  onSelect: (value: string) => void;
}

// A check mark for the chosen item; aria-checked carries the state, so it is
// decorative.
const ICON_CHECK = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"></path></svg>`;

let menuSeq = 0;

export class MenuButton {
  public readonly menu: HTMLElement;
  private readonly items: HTMLElement[];
  private value = "";

  constructor(private readonly button: HTMLElement, private readonly opts: MenuButtonOptions) {
    const uid = ++menuSeq;
    this.menu = elem("div", "menu-popover");
    this.menu.id = `menu-${uid}`;
    this.menu.setAttribute("role", "menu");
    this.menu.setAttribute("aria-label", opts.label);
    this.menu.hidden = true;
    this.items = opts.choices.map((c) => {
      const item = elem("button", "menu-item");
      item.setAttribute("type", "button");
      item.setAttribute("role", "menuitemradio");
      item.setAttribute("aria-checked", "false");
      // Roving focus: arrows move it; Tab leaves the menu (and closes it).
      item.tabIndex = -1;
      item.dataset.value = c.value;
      if (c.icon) item.appendChild(iconSpan(c.icon, "menu-item-icon"));
      item.appendChild(elem("span", "menu-item-label", c.label));
      item.appendChild(iconSpan(ICON_CHECK, "menu-item-check"));
      item.addEventListener("click", () => this.pick(c.value));
      this.menu.appendChild(item);
      return item;
    });
    button.setAttribute("aria-haspopup", "menu");
    button.setAttribute("aria-expanded", "false");
    button.setAttribute("aria-controls", this.menu.id);
    button.insertAdjacentElement("afterend", this.menu);

    button.addEventListener("click", () => {
      if (this.isOpen()) this.close(false);
      else this.open(this.checkedIndex());
    });
    button.addEventListener("keydown", (e) => {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        this.open(e.key === "ArrowUp" ? this.items.length - 1 : this.checkedIndex());
      }
    });
    this.menu.addEventListener("keydown", (e) => this.onMenuKey(e));
    // Capture phase, so a click that another handler stops still closes the menu.
    document.addEventListener("click", (e) => {
      const t = e.target;
      if (this.isOpen() && t instanceof Node && !this.menu.contains(t) && !button.contains(t)) this.close(false);
    }, true);
  }

  // set shows value as the checked item, without reporting it through onSelect.
  public set(value: string): void {
    this.value = value;
    for (const item of this.items) {
      const on = item.dataset.value === value;
      if (item.getAttribute("aria-checked") !== String(on)) item.setAttribute("aria-checked", String(on));
    }
  }

  public isOpen(): boolean {
    return !this.menu.hidden;
  }

  private checkedIndex(): number {
    return Math.max(0, this.items.findIndex((i) => i.dataset.value === this.value));
  }

  private open(focusIndex: number): void {
    this.menu.hidden = false;
    this.button.setAttribute("aria-expanded", "true");
    this.focusItem(focusIndex);
  }

  // close hides the menu; returnFocus puts focus back on the button (after a
  // pick or Escape), but not after Tab or an outside click, which already
  // moved it somewhere the operator chose.
  private close(returnFocus: boolean): void {
    if (!this.isOpen()) return;
    this.menu.hidden = true;
    this.button.setAttribute("aria-expanded", "false");
    if (returnFocus) this.button.focus();
  }

  private focusItem(index: number): void {
    const n = this.items.length;
    if (n === 0) return;
    this.items[((index % n) + n) % n].focus();
  }

  private pick(value: string): void {
    this.set(value);
    this.close(true);
    this.opts.onSelect(value);
  }

  private onMenuKey(e: KeyboardEvent): void {
    const current = this.items.indexOf(document.activeElement as HTMLElement);
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        this.focusItem(current + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        this.focusItem(current - 1);
        break;
      case "Home":
        e.preventDefault();
        this.focusItem(0);
        break;
      case "End":
        e.preventDefault();
        this.focusItem(this.items.length - 1);
        break;
      case "Escape":
        e.preventDefault();
        this.close(true);
        break;
      case "Tab":
        this.close(false);
        break;
      // Enter and Space activate the focused item's button, whose click handler
      // picks it; nothing to do here.
    }
  }
}
