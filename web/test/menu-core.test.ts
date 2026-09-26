// Unit tests for the MenuButton rules (lib/menu-core.ts): where each key moves
// focus in an open menu, which item opening starts on, and when focus leaving
// the menu closes it. Run with node:test over the compiled output (see
// web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { closesOnFocusOut, menuKeyAction, openIndex } from "../src/lib/menu-core.js";

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
