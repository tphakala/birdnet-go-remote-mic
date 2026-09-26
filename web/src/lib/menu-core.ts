// Pure decisions for the MenuButton component (components/menu-button.ts):
// where a key moves focus in an open menu, which item opening starts on,
// whether focus leaving the menu closes it, and the MenuController that
// sequences them. No DOM here: the component hands the controller a small set
// of ports, so node:test covers the wiring (what opens and closes the menu,
// and where focus goes) as well as the rules.

export type MenuKeyAction =
  | { kind: "focus"; index: number }
  | { kind: "close"; returnFocus: boolean }
  | { kind: "none" };

// Where focus went when it left the menu: nowhere (the window lost focus),
// still inside the menu, the menu's own button, or anywhere else on the page.
export type FocusTarget = "none" | "menu" | "button" | "outside";

// wrap maps any index onto 0..count-1, so moving past either end wraps around.
function wrap(index: number, count: number): number {
  return ((index % count) + count) % count;
}

// typeaheadIndex is the item a printable key moves focus to: the next item
// after current whose label starts with the key, ignoring case and wrapping
// around, or -1 when none does.
export function typeaheadIndex(key: string, current: number, labels: readonly string[]): number {
  const k = key.toLocaleLowerCase();
  for (let step = 1; step <= labels.length; step++) {
    const i = wrap(current + step, labels.length);
    if (labels[i].trimStart().toLocaleLowerCase().startsWith(k)) return i;
  }
  return -1;
}

// menuKeyAction decides what a key pressed inside an open menu of count items
// does, with focus on item current (-1 when on none). Enter and Space are not
// handled: they activate the focused item's own button. With labels, a single
// printable character moves focus to the next item starting with it.
export function menuKeyAction(key: string, current: number, count: number, labels: readonly string[] = []): MenuKeyAction {
  if (key.length === 1 && key !== " " && labels.length === count) {
    const index = typeaheadIndex(key, current, labels);
    return index >= 0 ? { kind: "focus", index } : { kind: "none" };
  }
  switch (key) {
    case "ArrowDown":
      return count > 0 ? { kind: "focus", index: wrap(current + 1, count) } : { kind: "none" };
    case "ArrowUp":
      // From no item, ArrowUp starts at the last, as ArrowDown starts at the first.
      if (count === 0) return { kind: "none" };
      return { kind: "focus", index: current < 0 ? count - 1 : wrap(current - 1, count) };
    case "Home":
      return count > 0 ? { kind: "focus", index: 0 } : { kind: "none" };
    case "End":
      return count > 0 ? { kind: "focus", index: count - 1 } : { kind: "none" };
    case "Escape":
      return { kind: "close", returnFocus: true };
    // Tab and Shift+Tab leave the menu: close it and let the browser move focus
    // on from the item, to the next control or back to the button.
    case "Tab":
      return { kind: "close", returnFocus: false };
    default:
      return { kind: "none" };
  }
}

// openIndex picks the item opening focuses: the last when fromEnd (ArrowUp),
// else the checked one (checked is -1 when none is), else the first.
export function openIndex(fromEnd: boolean, checked: number, count: number): number {
  if (fromEnd) return Math.max(0, count - 1);
  return Math.max(0, checked);
}

// closesOnFocusOut decides whether focus leaving an item closes the menu: only
// while it is open (hiding the menu blurs the focused item, which must not
// count) and only when focus landed elsewhere on the page, such as a route
// change or a dialog taking focus. A window blur keeps it open, as the
// notification panel does, and the button's own click toggles it.
export function closesOnFocusOut(open: boolean, related: FocusTarget): boolean {
  return open && related === "outside";
}

// MenuPorts is the DOM side of a menu, supplied by the component.
export interface MenuPorts {
  // setOpen shows or hides the menu, sets the button's aria-expanded, and adds
  // or removes the page-wide listeners that live only while it is open.
  setOpen(open: boolean): void;
  // setChecked marks item index as the chosen one (-1 for none).
  setChecked(index: number): void;
  focusItem(index: number): void;
  focusButton(): void;
}

export interface MenuItem {
  value: string;
  label: string;
}

// MenuController owns a single-choice menu's state and sequencing: the
// component forwards its events here and applies the result through the
// ports. The caller owns the value: onSelect reports a pick, and set() shows a
// value, whoever changed it.
export class MenuController {
  private open = false;
  private value = "";
  private readonly labels: string[];

  constructor(
    private readonly ports: MenuPorts,
    private readonly items: readonly MenuItem[],
    private readonly onSelect: (value: string) => void,
  ) {
    this.labels = items.map((i) => i.label);
  }

  public isOpen(): boolean {
    return this.open;
  }

  // set shows value as the checked item, without reporting it through onSelect.
  public set(value: string): void {
    this.value = value;
    this.ports.setChecked(this.checkedIndex());
  }

  // buttonClick toggles the menu, opening on the checked item.
  public buttonClick(): void {
    if (this.open) this.close(false);
    else this.show(openIndex(false, this.checkedIndex(), this.items.length));
  }

  // buttonKey handles a key on the button and reports whether it did, so the
  // component prevents the default only then. ArrowDown opens on the checked
  // item and ArrowUp on the last; Enter and Space arrive as a click.
  public buttonKey(key: string): boolean {
    if (key !== "ArrowDown" && key !== "ArrowUp") return false;
    this.show(openIndex(key === "ArrowUp", this.checkedIndex(), this.items.length));
    return true;
  }

  // menuKey handles a key with focus on item current (-1 when on none) and
  // reports whether to prevent the default. Tab closes without preventing it,
  // so the browser moves focus on from the item.
  public menuKey(key: string, current: number): boolean {
    const action = menuKeyAction(key, current, this.items.length, this.labels);
    if (action.kind === "focus") {
      this.ports.focusItem(action.index);
      return true;
    }
    if (action.kind === "close") {
      this.close(action.returnFocus);
      return action.returnFocus;
    }
    return false;
  }

  // focusMoved reports focus leaving an item (focusout) or arriving anywhere
  // on the page (focusin) while the menu is open. A dialog inerts the page
  // before it takes focus, so the item blurs with no related target and
  // focusout alone would leave the menu open behind it; focusin catches that.
  public focusMoved(target: FocusTarget): void {
    if (closesOnFocusOut(this.open, target)) this.close(false);
  }

  // outsideClick and routeChange close without taking focus back: the click
  // or the new view already put it somewhere the operator or the page chose.
  public outsideClick(): void {
    this.close(false);
  }

  public routeChange(): void {
    this.close(false);
  }

  // pick reports the choice before returning focus, so the button already
  // carries its new label when focus lands on it and a screen reader reads that.
  public pick(value: string): void {
    this.set(value);
    this.onSelect(value);
    this.close(true);
  }

  // checkedIndex is the checked item's index, or -1 when none is checked.
  private checkedIndex(): number {
    return this.items.findIndex((i) => i.value === this.value);
  }

  private show(focusIndex: number): void {
    if (!this.open) {
      this.open = true;
      this.ports.setOpen(true);
    }
    this.ports.focusItem(focusIndex);
  }

  // close hides the menu; returnFocus puts focus back on the button (after a
  // pick or Escape). The state flips first, so the focusout the hide causes
  // sees a closed menu.
  private close(returnFocus: boolean): void {
    if (!this.open) return;
    this.open = false;
    this.ports.setOpen(false);
    if (returnFocus) this.ports.focusButton();
  }
}
