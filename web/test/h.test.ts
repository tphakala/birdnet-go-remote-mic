// Unit tests for the h() element builder (lib/ui.ts): which first argument is
// the attributes, how attribute values are written, which children are
// skipped, and that event handler attributes are refused. The tests run
// without a DOM, so a minimal fake document records what h() does. Run with
// node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { h } from "../src/lib/ui.ts";

// FakeElement records the calls h() makes. A string child is kept as text,
// which a real element turns into a text node.
class FakeElement {
  readonly nodeType = 1;
  readonly tagName: string;
  readonly attrs = new Map<string, string>();
  readonly children: unknown[] = [];
  constructor(tag: string) {
    this.tagName = tag;
  }
  setAttribute(name: string, value: string): void {
    this.attrs.set(name, value);
  }
  append(...nodes: unknown[]): void {
    this.children.push(...nodes);
  }
}

const fakeDocument = { createElement: (tag: string) => new FakeElement(tag) };
Object.defineProperty(globalThis, "document", { value: fakeDocument, configurable: true });

const fake = (el: unknown): FakeElement => {
  assert.ok(el instanceof FakeElement);
  return el;
};

test("attributes are set as written, strings and numbers as text", () => {
  const e = fake(h("pre", { class: "license-text mono", tabindex: 0, "aria-label": "Apache" }));
  assert.equal(e.tagName, "pre");
  assert.deepEqual([...e.attrs], [["class", "license-text mono"], ["tabindex", "0"], ["aria-label", "Apache"]]);
});

test("true sets an empty attribute; false, null and undefined leave it off", () => {
  const e = fake(h("input", { disabled: true, hidden: false, title: null, name: undefined }));
  assert.deepEqual([...e.attrs], [["disabled", ""]]);
});

test("aria state takes the string, so false is written, not dropped", () => {
  const e = fake(h("button", { "aria-pressed": "false" }));
  assert.equal(e.attrs.get("aria-pressed"), "false");
});

test("without attributes the first argument is a child", () => {
  const e = fake(h("li", "Your microphone model."));
  assert.equal(e.attrs.size, 0);
  assert.deepEqual(e.children, ["Your microphone model."]);

  const inner = h("span");
  const outer = fake(h("div", inner, "tail"));
  assert.equal(outer.attrs.size, 0);
  assert.deepEqual(outer.children, [inner, "tail"]);
});

test("children keep their order and skip false, null and undefined", () => {
  const code = h("code", "journalctl");
  const e = fake(h("li", { class: "x" }, "The log: ", false, code, null, undefined, "."));
  assert.deepEqual(e.children, ["The log: ", code, "."]);
});

test("an empty attribute object is still the attributes, not a child", () => {
  const e = fake(h("div", {}, "a"));
  assert.equal(e.attrs.size, 0);
  assert.deepEqual(e.children, ["a"]);
});

test("event handler attributes are refused in any case", () => {
  assert.throws(() => h("button", { onclick: "alert(1)" }), TypeError);
  assert.throws(() => h("img", { OnError: "x" }), TypeError);
});
