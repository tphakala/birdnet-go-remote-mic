// Unit tests for orderChildren (lib/ui.ts): it puts managed nodes in order,
// moves nothing when they already are, and leaves unmanaged children after
// them.
// A minimal stand-in for DOM elements (fake-dom.ts) keeps the test free of a
// browser. Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { orderChildren } from "../src/lib/ui.ts";
import { FakeNode, names, parentWith } from "./fake-dom.ts";

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
