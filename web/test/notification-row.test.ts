// Unit tests for renderNotificationRow (components/notification-row.ts) with a
// time the Date type cannot hold: the row must render with an unknown time
// instead of throwing from the date formatters. The tests run without a DOM,
// so a minimal fake document records what h() builds (see h.test.ts). Run with
// node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { renderNotificationRow } from "../src/components/notification-row.ts";
import { anchorToMs } from "../src/lib/notifications-core.ts";
import { notif } from "./fixtures.ts";

// FakeElement records the calls h() makes. A string child is kept as text,
// which a real element turns into a text node.
class FakeElement {
  readonly nodeType = 1;
  readonly tagName: string;
  readonly attrs = new Map<string, string>();
  readonly children: unknown[] = [];
  innerHTML = "";
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

Object.defineProperty(globalThis, "document", { value: { createElement: (tag: string) => new FakeElement(tag) }, configurable: true });

// find walks the built tree for the first element with this tag and, when
// given, class.
function find(root: unknown, tag: string, cls?: string): FakeElement | undefined {
  if (!(root instanceof FakeElement)) return undefined;
  if (root.tagName === tag && (cls === undefined || (root.attrs.get("class") ?? "").split(" ").includes(cls))) return root;
  for (const c of root.children) {
    const hit = find(c, tag, cls);
    if (hit) return hit;
  }
  return undefined;
}

const nowMs = Date.parse("2026-09-22T12:00:00Z");
const toMs = (uptimeMs: number): number => anchorToMs({ browserMs: nowMs, uptimeMs: 0 }, uptimeMs);

test("a compact row past the Date range renders with an unknown time instead of throwing", () => {
  const row = renderNotificationRow(notif({ id: 1, uptimeMs: 9e15 }), { nowMs, toMs });
  const time = find(row, "time", "notif-row-time");
  assert.ok(time);
  assert.equal(time.attrs.get("title"), "Unknown time");
  assert.equal(time.attrs.has("datetime"), false);
});

test("a full row past the Date range renders with an unknown time instead of throwing", () => {
  const row = renderNotificationRow(notif({ id: 1, uptimeMs: 9e15 }), { nowMs, toMs, full: true });
  const abs = find(row, "span", "ev-abs-time");
  assert.ok(abs);
  assert.deepEqual(abs.children, []);
  const time = find(row, "time", "notif-row-time");
  assert.equal(time?.attrs.get("title"), "Unknown time");
});
