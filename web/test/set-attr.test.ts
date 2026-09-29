// Unit tests for setAttr (lib/ui.ts): a diffed attribute write where an empty
// value means the attribute is absent, so an empty title never masks the tooltip
// of an ancestor. A minimal stand-in for an element keeps the test free of a
// browser. Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { setAttr } from "../src/lib/ui.ts";

// FakeEl keeps attributes in a map and counts writes; getAttribute returns null
// for an absent attribute, as the DOM does.
class FakeEl {
  attrs = new Map<string, string>();
  writes = 0;
  removes = 0;

  getAttribute(name: string): string | null {
    return this.attrs.get(name) ?? null;
  }
  hasAttribute(name: string): boolean {
    return this.attrs.has(name);
  }
  setAttribute(name: string, value: string): void {
    this.writes++;
    this.attrs.set(name, value);
  }
  removeAttribute(name: string): void {
    this.removes++;
    this.attrs.delete(name);
  }
}

function el(): FakeEl {
  return new FakeEl();
}

test("setAttr writes a changed value once and skips a repeat", () => {
  const e = el();
  setAttr(e as unknown as Element, "title", "a");
  setAttr(e as unknown as Element, "title", "a");
  assert.equal(e.getAttribute("title"), "a");
  assert.equal(e.writes, 1);
  setAttr(e as unknown as Element, "title", "b");
  assert.equal(e.getAttribute("title"), "b");
  assert.equal(e.writes, 2);
});

test("setAttr treats an empty value as absent", () => {
  const e = el();
  setAttr(e as unknown as Element, "title", "");
  assert.equal(e.hasAttribute("title"), false, "an absent attribute stays absent");
  assert.equal(e.writes, 0);
  assert.equal(e.removes, 0);

  setAttr(e as unknown as Element, "title", "tip");
  setAttr(e as unknown as Element, "title", "");
  assert.equal(e.hasAttribute("title"), false, "a present attribute is removed");
  assert.equal(e.removes, 1);
});
