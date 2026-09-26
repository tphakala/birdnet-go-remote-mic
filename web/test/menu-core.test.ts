// Unit tests for the MenuButton rules and wiring (lib/menu-core.ts): where each key moves
// focus in an open menu, which item opening starts on, and when focus leaving
// the menu closes it. Run with node:test over the compiled output (see
// web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { closesOnFocusOut, MenuController, menuKeyAction, openIndex, typeaheadIndex } from "../src/lib/menu-core.js";

test("arrows move focus and wrap at both ends", () => {
  assert.deepEqual(menuKeyAction("ArrowDown", 0, 3), { kind: "focus", index: 1 });
  assert.deepEqual(menuKeyAction("ArrowDown", 2, 3), { kind: "focus", index: 0 });
  assert.deepEqual(menuKeyAction("ArrowUp", 0, 3), { kind: "focus", index: 2 });
  assert.deepEqual(menuKeyAction("ArrowUp", 2, 3), { kind: "focus", index: 1 });
  // Focus on no item: ArrowDown starts at the first, ArrowUp at the last.
  assert.deepEqual(menuKeyAction("ArrowDown", -1, 3), { kind: "focus", index: 0 });
  assert.deepEqual(menuKeyAction("ArrowUp", -1, 3), { kind: "focus", index: 2 });
});

test("Home and End jump to the first and last item", () => {
  assert.deepEqual(menuKeyAction("Home", 2, 3), { kind: "focus", index: 0 });
  assert.deepEqual(menuKeyAction("End", 0, 3), { kind: "focus", index: 2 });
});

test("an empty menu moves focus nowhere", () => {
  for (const key of ["ArrowDown", "ArrowUp", "Home", "End"]) {
    assert.deepEqual(menuKeyAction(key, -1, 0), { kind: "none" }, key);
  }
});

test("Escape closes and returns focus; Tab closes and leaves focus to the browser", () => {
  assert.deepEqual(menuKeyAction("Escape", 1, 3), { kind: "close", returnFocus: true });
  assert.deepEqual(menuKeyAction("Tab", 1, 3), { kind: "close", returnFocus: false });
});

test("other keys, Enter and Space included, are left to the focused item", () => {
  for (const key of ["Enter", " ", "a", "ArrowLeft"]) {
    assert.deepEqual(menuKeyAction(key, 1, 3), { kind: "none" }, key);
  }
});

test("opening starts on the checked item, the first when none, the last for ArrowUp", () => {
  assert.equal(openIndex(false, 1, 3), 1);
  assert.equal(openIndex(false, -1, 3), 0);
  assert.equal(openIndex(true, 0, 3), 2);
  assert.equal(openIndex(true, -1, 0), 0);
});

test("focus moving elsewhere on the page closes an open menu", () => {
  assert.equal(closesOnFocusOut(true, "outside"), true);
});

test("focus staying inside, moving to the button, or leaving the window keeps it open", () => {
  assert.equal(closesOnFocusOut(true, "menu"), false);
  assert.equal(closesOnFocusOut(true, "button"), false);
  assert.equal(closesOnFocusOut(true, "none"), false);
});

test("a focusout on a closed menu (the blur its own hide causes) is ignored", () => {
  for (const related of ["outside", "none", "button", "menu"] as const) {
    assert.equal(closesOnFocusOut(false, related), false, related);
  }
});

test("typeaheadIndex moves to the next item starting with the key, wrapping", () => {
  const labels = ["System", "Light", "Dark", "Slate"];
  assert.equal(typeaheadIndex("s", -1, labels), 0);
  assert.equal(typeaheadIndex("S", 0, labels), 3);
  assert.equal(typeaheadIndex("s", 3, labels), 0);
  assert.equal(typeaheadIndex("d", 0, labels), 2);
  assert.equal(typeaheadIndex("x", 1, labels), -1);
});

test("menuKeyAction uses typeahead only for a printable key with labels", () => {
  const labels = ["System", "Light", "Dark"];
  assert.deepEqual(menuKeyAction("l", 0, 3, labels), { kind: "focus", index: 1 });
  assert.deepEqual(menuKeyAction("q", 0, 3, labels), { kind: "none" });
  // Space activates the focused item; it is not a typeahead key.
  assert.deepEqual(menuKeyAction(" ", 0, 3, labels), { kind: "none" });
  // Without labels a letter does nothing, as before.
  assert.deepEqual(menuKeyAction("l", 0, 3), { kind: "none" });
});

// A recording MenuPorts: every call in order, so a test can pin the sequence
// the component applies to the DOM.
function harness(value = "light"): { ctl: MenuController; calls: string[]; picked: string[] } {
  const calls: string[] = [];
  const picked: string[] = [];
  const ctl = new MenuController(
    {
      setOpen: (open) => calls.push(`open:${open}`),
      setChecked: (i) => calls.push(`checked:${i}`),
      focusItem: (i) => calls.push(`item:${i}`),
      focusButton: () => calls.push("button"),
    },
    [
      { value: "system", label: "System" },
      { value: "light", label: "Light" },
      { value: "dark", label: "Dark" },
    ],
    (v) => {
      picked.push(v);
      calls.push(`select:${v}`);
    },
  );
  ctl.set(value);
  calls.length = 0;
  return { ctl, calls, picked };
}

test("MenuController opens on the checked item and toggles on the button", () => {
  const { ctl, calls } = harness();
  ctl.buttonClick();
  assert.equal(ctl.isOpen(), true);
  ctl.buttonClick();
  assert.equal(ctl.isOpen(), false);
  assert.deepEqual(calls, ["open:true", "item:1", "open:false"]);
});

test("MenuController opens from the arrows and ignores other keys on the button", () => {
  const { ctl, calls } = harness();
  assert.equal(ctl.buttonKey("Enter"), false);
  assert.equal(ctl.buttonKey("ArrowUp"), true);
  assert.deepEqual(calls, ["open:true", "item:2"]);
});

test("MenuController reports a pick before returning focus to the button", () => {
  const { ctl, calls, picked } = harness();
  ctl.buttonClick();
  calls.length = 0;
  ctl.pick("dark");
  assert.deepEqual(picked, ["dark"]);
  assert.deepEqual(calls, ["checked:2", "select:dark", "open:false", "button"]);
});

test("MenuController closes on Escape with focus back, on Tab without", () => {
  const { ctl, calls } = harness();
  ctl.buttonClick();
  calls.length = 0;
  assert.equal(ctl.menuKey("Escape", 1), true);
  assert.deepEqual(calls, ["open:false", "button"]);

  ctl.buttonClick();
  calls.length = 0;
  // Tab is not prevented, so the browser moves focus on from the item.
  assert.equal(ctl.menuKey("Tab", 1), false);
  assert.deepEqual(calls, ["open:false"]);
});

test("MenuController moves focus with arrows and typeahead", () => {
  const { ctl, calls } = harness();
  ctl.buttonClick();
  calls.length = 0;
  assert.equal(ctl.menuKey("ArrowDown", 1), true);
  assert.equal(ctl.menuKey("s", 2), true);
  assert.equal(ctl.menuKey("z", 0), false);
  assert.deepEqual(calls, ["item:2", "item:0"]);
  assert.equal(ctl.isOpen(), true);
});

test("MenuController closes without taking focus on an outside move, click or route change", () => {
  for (const close of [
    (c: MenuController) => c.focusMoved("outside"),
    (c: MenuController) => c.outsideClick(),
    (c: MenuController) => c.routeChange(),
  ]) {
    const { ctl, calls } = harness();
    ctl.buttonClick();
    calls.length = 0;
    close(ctl);
    assert.equal(ctl.isOpen(), false);
    assert.deepEqual(calls, ["open:false"]);
  }
});

test("MenuController stays open when focus stays in the menu, on the button, or leaves the window", () => {
  const { ctl, calls } = harness();
  ctl.buttonClick();
  calls.length = 0;
  ctl.focusMoved("menu");
  ctl.focusMoved("button");
  ctl.focusMoved("none");
  assert.equal(ctl.isOpen(), true);
  assert.deepEqual(calls, []);
});

test("MenuController close events on a closed menu do nothing", () => {
  const { ctl, calls } = harness();
  ctl.outsideClick();
  ctl.routeChange();
  ctl.focusMoved("outside");
  assert.equal(ctl.menuKey("Escape", -1), true);
  assert.deepEqual(calls, []);
});
