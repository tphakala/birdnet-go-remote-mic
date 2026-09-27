// Unit tests for orderChildren (lib/ui.ts): it puts managed nodes in order,
// moves nothing when they already are, and leaves unmanaged children after
// them.
// A minimal stand-in for DOM elements keeps the test free of a browser. Run
// with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { orderChildren } from "../src/lib/ui.ts";

// FakeParent keeps an ordered child list and counts insertBefore calls; its
// children expose nextElementSibling as the DOM does.
class FakeParent {
  kids: FakeNode[] = [];
  moves = 0;

  get firstElementChild(): FakeNode | null {
    return this.kids[0] ?? null;
  }

  insertBefore(node: FakeNode, target: FakeNode | null): void {
    this.moves++;
    this.kids = this.kids.filter((k) => k !== node);
    const i = target ? this.kids.indexOf(target) : this.kids.length;
    this.kids.splice(i, 0, node);
    node.parent = this;
  }
}

class FakeNode {
  parent: FakeParent | null = null;
  readonly name: string;

  constructor(name: string) {
    this.name = name;
  }

  get nextElementSibling(): FakeNode | null {
    const kids = this.parent?.kids ?? [];
    return kids[kids.indexOf(this) + 1] ?? null;
  }
}

function parentWith(...nodes: FakeNode[]): FakeParent {
  const p = new FakeParent();
  for (const n of nodes) {
    p.kids.push(n);
    n.parent = p;
  }
  return p;
}

const names = (p: FakeParent): string[] => p.kids.map((k) => k.name);

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
