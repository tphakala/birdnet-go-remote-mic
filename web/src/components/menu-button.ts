// MenuButton turns an existing button into a menu button with a single-choice
// menu (the WAI-ARIA menu button pattern with menuitemradio items): the button
// opens it (click, Enter, Space, ArrowDown to the checked item, ArrowUp to the
// last), arrows, Home and End move between items, a letter moves to the next
// item starting with it, Enter or Space picks one, and Escape, Tab, a click
// outside, focus moving elsewhere, or a route change closes it. Picking
// returns focus to the button. The caller owns the value: onSelect reports the
// pick, and set() shows a value, whoever changed it. The menu is inserted
// right after the button, so the caller wraps the button in a positioned
// element (.menu-wrap) the popover anchors to. The state, key and focus rules
// live in lib/menu-core.ts (MenuController); this file is the DOM side.
import { MenuController, type FocusTarget, type MenuItem } from "../lib/menu-core.js";
import { elem, iconSpan } from "../lib/ui.js";

export interface MenuChoice extends MenuItem {
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
  private readonly ctl: MenuController;

  constructor(private readonly button: HTMLElement, opts: MenuButtonOptions) {
    const uid = ++menuSeq;
    this.menu = elem("div", "menu-popover");
    this.menu.id = `menu-${uid}`;
    this.menu.setAttribute("role", "menu");
    this.menu.setAttribute("aria-label", opts.label);
    this.menu.hidden = true;
    this.ctl = new MenuController(
      {
        setOpen: (open) => this.setOpen(open),
        setChecked: (index) => {
          this.items.forEach((item, i) => {
            const on = String(i === index);
            if (item.getAttribute("aria-checked") !== on) item.setAttribute("aria-checked", on);
          });
        },
        focusItem: (index) => this.items[index]?.focus(),
        focusButton: () => this.button.focus(),
      },
      opts.choices,
      opts.onSelect,
    );
    this.items = opts.choices.map((c) => {
      const item = elem("button", "menu-item");
      item.setAttribute("type", "button");
      item.setAttribute("role", "menuitemradio");
      item.setAttribute("aria-checked", "false");
      // Roving focus: arrows move it; Tab leaves the menu and closes it.
      item.tabIndex = -1;
      if (c.icon) item.appendChild(iconSpan(c.icon, "menu-item-icon"));
      item.appendChild(elem("span", "menu-item-label", c.label));
      item.appendChild(iconSpan(ICON_CHECK, "menu-item-check"));
      item.addEventListener("click", () => this.ctl.pick(c.value));
      this.menu.appendChild(item);
      return item;
    });
    button.setAttribute("aria-haspopup", "menu");
    button.setAttribute("aria-expanded", "false");
    button.setAttribute("aria-controls", this.menu.id);
    button.insertAdjacentElement("afterend", this.menu);

    button.addEventListener("click", () => this.ctl.buttonClick());
    button.addEventListener("keydown", (e) => {
      if (this.ctl.buttonKey(e.key)) e.preventDefault();
    });
    this.menu.addEventListener("keydown", (e) => {
      const modified = e.ctrlKey || e.metaKey || e.altKey;
      if (this.ctl.menuKey(e.key, this.items.indexOf(document.activeElement as HTMLElement), modified)) e.preventDefault();
    });
    this.menu.addEventListener("focusout", (e) => this.ctl.focusMoved(this.focusTarget(e.relatedTarget)));
  }

  // set shows value as the checked item, without reporting it through onSelect.
  public set(value: string): void {
    this.ctl.set(value);
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
    if (t instanceof Node && !this.menu.contains(t) && !this.button.contains(t)) this.ctl.outsideClick();
  };
  private readonly onHashChange = (): void => this.ctl.routeChange();
  private readonly onFocusIn = (e: FocusEvent): void => this.ctl.focusMoved(this.focusTarget(e.target));

  private setOpen(open: boolean): void {
    this.menu.hidden = !open;
    this.button.setAttribute("aria-expanded", String(open));
    if (open) {
      // Capture phase, so a click that another handler stops still closes the menu.
      document.addEventListener("click", this.onDocClick, true);
      window.addEventListener("hashchange", this.onHashChange);
      document.addEventListener("focusin", this.onFocusIn);
    } else {
      document.removeEventListener("click", this.onDocClick, true);
      window.removeEventListener("hashchange", this.onHashChange);
      document.removeEventListener("focusin", this.onFocusIn);
    }
  }
}
