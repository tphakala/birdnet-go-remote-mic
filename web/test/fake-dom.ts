// A minimal stand-in for DOM elements, shared by the tests of orderChildren
// and KeyedList so they run in node:test without a browser. Not a test file
// itself (web:test runs test/*.test.ts only).

// FakeParent keeps an ordered child list and counts insertBefore calls
// (moves) and detaching removals (removes); its children expose
// nextElementSibling as the DOM does.
export class FakeParent {
  kids: FakeNode[] = [];
  moves = 0;
  removes = 0;

  get firstElementChild(): FakeNode | null {
    return this.kids[0] ?? null;
  }

  // stateful counts moves made with moveBefore, which a browser does without
  // detaching the node; it is set only by withMoveBefore.
  stateful = 0;
  moveBefore?: (node: FakeNode, target: FakeNode | null) => void;

  insertBefore(node: FakeNode, target: FakeNode | null): void {
    this.moves++;
    this.place(node, target);
  }

  place(node: FakeNode, target: FakeNode | null): void {
    this.kids = this.kids.filter((k) => k !== node);
    const i = target ? this.kids.indexOf(target) : this.kids.length;
    this.kids.splice(i, 0, node);
    node.parent = this;
  }
}

export class FakeNode {
  parent: FakeParent | null = null;
  readonly name: string;

  constructor(name: string) {
    this.name = name;
  }

  get parentNode(): FakeParent | null {
    return this.parent;
  }

  get nextElementSibling(): FakeNode | null {
    const kids = this.parent?.kids ?? [];
    return kids[kids.indexOf(this) + 1] ?? null;
  }

  // remove detaches the node; like Element.remove it does nothing when the
  // node has no parent.
  remove(): void {
    const p = this.parent;
    if (!p) return;
    p.kids = p.kids.filter((k) => k !== this);
    p.removes++;
    this.parent = null;
  }

  // replaceWith puts other where this node is, without counting a move.
  replaceWith(other: FakeNode): void {
    const p = this.parent;
    if (!p) return;
    p.kids[p.kids.indexOf(this)] = other;
    other.parent = p;
    this.parent = null;
  }
}

export function parentWith(...nodes: FakeNode[]): FakeParent {
  const p = new FakeParent();
  for (const n of nodes) {
    p.kids.push(n);
    n.parent = p;
  }
  return p;
}

// withMoveBefore gives p a state-preserving moveBefore, as a browser with
// Element.moveBefore has; it counts those moves in p.stateful, not p.moves.
export function withMoveBefore(p: FakeParent): FakeParent {
  p.moveBefore = (node, target) => {
    if (node.parent !== p) throw new Error("moveBefore of a node that is not a child");
    p.stateful++;
    p.place(node, target);
  };
  return p;
}

export const names = (p: FakeParent): string[] => p.kids.map((k) => k.name);
