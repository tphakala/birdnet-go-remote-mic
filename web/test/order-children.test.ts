// Unit tests for orderChildren (lib/ui.ts): it puts managed nodes in order,
// moves nothing when they already are, and leaves unmanaged children after
// them, and moves a node already in the parent with moveBefore (which keeps
// focus inside it) where the browser has it.
// A minimal stand-in for DOM elements (fake-dom.ts) keeps the test free of a
// browser. Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { orderChildren } from "../src/lib/ui.ts";
import { FakeNode, names, parentWith, withMoveBefore } from "./fake-dom.ts";

test("orderChildren leaves an ordered parent alone", () => {
  const [a, b, c] = [new FakeNode("a"), new FakeNode("b"), new FakeNode("c")];
  const p = parentWith(a, b, c);
  orderChildren(p as unknown as Element, [a, b, c] as unknown as Element[]);
  assert.equal(p.moves, 0, "steady state must move no node");
});

test("orderChildren puts managed nodes in order, placeholder last", () => {
  const [a, b, c, empty] = [new FakeNode("a"), new FakeNode("b"), new FakeNode("c"), new FakeNode("placeholder")];
  const p = parentWith(empty, c, a);
  orderChildren(p as unknown as Element, [a, b, c] as unknown as Element[]);
  assert.deepEqual(names(p), ["a", "b", "c", "placeholder"]);
});

test("orderChildren moves a node already in the parent with moveBefore, not insertBefore", () => {
  const [a, b, c] = [new FakeNode("a"), new FakeNode("b"), new FakeNode("c")];
  const p = withMoveBefore(parentWith(c, b, a));
  orderChildren(p as unknown as Element, [a, b, c] as unknown as Element[]);
  assert.deepEqual(names(p), ["a", "b", "c"]);
  assert.equal(p.moves, 0, "a node in the parent must not be detached and re-inserted");
  assert.ok(p.stateful > 0, "the reorder must go through moveBefore");
});

test("orderChildren inserts a new node with insertBefore even when moveBefore exists", () => {
  const [a, b, fresh] = [new FakeNode("a"), new FakeNode("b"), new FakeNode("fresh")];
  const p = withMoveBefore(parentWith(a, b));
  orderChildren(p as unknown as Element, [a, fresh, b] as unknown as Element[]);
  assert.deepEqual(names(p), ["a", "fresh", "b"]);
  assert.equal(p.moves, 1, "moveBefore throws for a node that is not a child yet");
  assert.equal(p.stateful, 0);
});

test("orderChildren with moveBefore leaves an ordered parent alone and keeps the placeholder last", () => {
  const [a, b, empty] = [new FakeNode("a"), new FakeNode("b"), new FakeNode("placeholder")];
  const steady = withMoveBefore(parentWith(a, b));
  orderChildren(steady as unknown as Element, [a, b] as unknown as Element[]);
  assert.equal(steady.moves + steady.stateful, 0, "steady state must move no node");
  const mixed = withMoveBefore(parentWith(empty, b, a));
  orderChildren(mixed as unknown as Element, [a, b] as unknown as Element[]);
  assert.deepEqual(names(mixed), ["a", "b", "placeholder"]);
});
