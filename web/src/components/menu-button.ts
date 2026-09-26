// MenuButton turns an existing button into a menu button with a single-choice
// menu (the WAI-ARIA menu button pattern with menuitemradio items): the button
// opens it (click, Enter, Space, ArrowDown to the checked item, ArrowUp to the
// last), arrows, Home and End move between items, Enter or Space picks one, and
// Escape, Tab, a click outside, focus moving elsewhere, or a route change
// closes it. Picking returns focus to the button. The caller owns the value:
// onSelect reports the pick, and set() shows a value, whoever changed it. The
// menu is inserted right after the button, so the caller wraps the button in a
// positioned element (.menu-wrap) the popover anchors to. The key and focus
// rules live in lib/menu-core.ts.
import { closesOnFocusOut, menuKeyAction, openIndex, type FocusTarget } from "../lib/menu-core.js";
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
  private readonly menu: HTMLElement;
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
      // Roving focus: arrows move it; Tab leaves the menu and closes it.
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
      else this.open(openIndex("click", this.checkedIndex(), this.items.length));
    });
    button.addEventListener("keydown", (e) => {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        this.open(openIndex(e.key, this.checkedIndex(), this.items.length));
      }
    });
    this.menu.addEventListener("keydown", (e) => this.onMenuKey(e));
    this.menu.addEventListener("focusout", (e) => {
      if (closesOnFocusOut(this.isOpen(), this.focusTarget(e.relatedTarget))) this.close(false);
    });
  }

  // set shows value as the checked item, without reporting it through onSelect.
  public set(value: string): void {
    this.value = value;
    for (const item of this.items) {
      const on = item.dataset.value === value;
      if (item.getAttribute("aria-checked") !== String(on)) item.setAttribute("aria-checked", String(on));
    }
  }

  private isOpen(): boolean {
    return !this.menu.hidden;
  }

  // checkedIndex is the checked item's index, or -1 when none is checked.
  private checkedIndex(): number {
    return this.items.findIndex((i) => i.dataset.value === this.value);
  }

  private focusTarget(t: EventTarget | null): FocusTarget {
    if (!(t instanceof Node)) return "none";
    if (this.menu.contains(t)) return "menu";
    if (this.button.contains(t)) return "button";
    return "outside";
  }

  // The page-wide listeners live only while the menu is open.
  private readonly onDocClick = (e: MouseEvent): void => {
    const t = e.target;
    if (t instanceof Node && !this.menu.contains(t) && !this.button.contains(t)) this.close(false);
  };
  private readonly onHashChange = (): void => this.close(false);

  private open(focusIndex: number): void {
    this.menu.hidden = false;
    this.button.setAttribute("aria-expanded", "true");
    // Capture phase, so a click that another handler stops still closes the menu.
    document.addEventListener("click", this.onDocClick, true);
    window.addEventListener("hashchange", this.onHashChange);
    this.focusItem(focusIndex);
  }

  // close hides the menu; returnFocus puts focus back on the button (after a
  // pick or Escape), but not after Tab, an outside click, or a focus move,
  // which already took focus somewhere the operator or the page chose. Hiding
  // comes first, so the focusout the hide causes sees a closed menu.
  private close(returnFocus: boolean): void {
    if (!this.isOpen()) return;
    this.menu.hidden = true;
    this.button.setAttribute("aria-expanded", "false");
    document.removeEventListener("click", this.onDocClick, true);
    window.removeEventListener("hashchange", this.onHashChange);
    if (returnFocus) this.button.focus();
  }

  private focusItem(index: number): void {
    this.items[index]?.focus();
  }

  // pick reports the choice before returning focus, so the button already
  // carries its new label when focus lands on it and a screen reader reads that.
  private pick(value: string): void {
    this.set(value);
    this.opts.onSelect(value);
    this.close(true);
  }

  private onMenuKey(e: KeyboardEvent): void {
    const action = menuKeyAction(e.key, this.items.indexOf(document.activeElement as HTMLElement), this.items.length);
    if (action.kind === "focus") {
      e.preventDefault();
      this.focusItem(action.index);
    } else if (action.kind === "close") {
      // Tab is not prevented: the browser moves focus on from the item.
      if (action.returnFocus) e.preventDefault();
      this.close(action.returnFocus);
    }
  }
}
