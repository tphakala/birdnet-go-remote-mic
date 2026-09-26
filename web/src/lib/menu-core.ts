// Pure decisions for the MenuButton component (components/menu-button.ts):
// where a key moves focus in an open menu, which item opening starts on, and
// whether focus leaving the menu closes it. No DOM here, so node:test covers
// the keyboard and focus rules the component wires to real events.

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

// menuKeyAction decides what a key pressed inside an open menu of count items
// does, with focus on item current (-1 when on none). Enter and Space are not
// handled: they activate the focused item's own button.
export function menuKeyAction(key: string, current: number, count: number): MenuKeyAction {
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

// openIndex picks the item opening focuses: the last for ArrowUp, else the
// checked one (checked is -1 when none is), else the first.
export function openIndex(key: string, checked: number, count: number): number {
  if (key === "ArrowUp") return Math.max(0, count - 1);
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
