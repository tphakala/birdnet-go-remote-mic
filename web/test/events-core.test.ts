// Unit tests for the pure Events-page logic (filtering, faceted counts, the
// onset/clear lifecycle pairing, and the formatters). Run with node:test over the
// compiled output (see the web:test task).

import test from "node:test";
import assert from "node:assert/strict";

import {
  conditionLifecycles,
  emptyFilter,
  exportJSON,
  facetCounts,
  filterEvents,
  formatDuration,
  isFilterActive,
} from "../src/lib/events-core.js";
import type { Notification } from "../src/lib/types.js";

function notif(over: Partial<Notification> & { id: number }): Notification {
  return {
    id: over.id,
    bootId: over.bootId ?? "boot-a",
    time: over.time ?? "2026-09-22T10:00:00Z",
    severity: over.severity ?? "info",
    category: over.category ?? "system",
    kind: over.kind ?? "event",
    key: over.key,
    source: over.source,
    title: over.title ?? "Title",
    message: over.message ?? "Message",
  };
}

const sample: Notification[] = [
  notif({ id: 1, severity: "info", category: "system", title: "Appliance started" }),
  notif({ id: 2, severity: "error", category: "device", source: "usbmic", title: "Device failed", kind: "onset", key: "dev:usbmic" }),
  notif({ id: 3, severity: "warning", category: "audio", source: "usbmic", title: "Silence detected", message: "No signal for 60s" }),
  notif({ id: 4, severity: "info", category: "stream", source: "10.0.0.5", title: "Client connected" }),
];

test("emptyFilter matches everything, newest first", () => {
  const f = emptyFilter();
  assert.equal(isFilterActive(f), false);
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [4, 3, 2, 1]);
});

test("severity and category facets narrow the list", () => {
  const f = emptyFilter();
  f.severities.add("error");
  f.severities.add("warning");
  assert.equal(isFilterActive(f), true);
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [3, 2]);
  f.categories.add("audio");
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [3]);
});

test("source filter matches the exact subject", () => {
  const f = emptyFilter();
  f.source = "usbmic";
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [3, 2]);
});

test("query matches title, message and source case-insensitively", () => {
  const f = emptyFilter();
  f.query = "  SIGNAL ";
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [3]);
  f.query = "10.0.0";
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [4]);
  f.query = "   ";
  assert.equal(isFilterActive(f), false);
});

test("facetCounts ignores a facet's own selection", () => {
  const f = emptyFilter();
  f.severities.add("error");
  const c = facetCounts(sample, f);
  // Severity counts span every severity despite the error selection.
  assert.deepEqual(c.severity, { error: 1, warning: 1, info: 2 });
  // Category counts honour the severity selection.
  assert.equal(c.category.get("device"), 1);
  assert.equal(c.category.get("audio"), 0);
  assert.equal(c.category.get("system"), 0);
});

test("facetCounts appends an unknown category", () => {
  const c = facetCounts([notif({ id: 9, category: "future" as Notification["category"] })], emptyFilter());
  assert.equal(c.category.get("future"), 1);
  assert.equal(c.category.get("device"), 0);
});

test("conditionLifecycles pairs onset and clear by key", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", time: "2026-09-22T10:00:00Z" }),
    notif({ id: 2, kind: "clear", key: "k", time: "2026-09-22T10:03:00Z" }),
    notif({ id: 3, kind: "onset", key: "k", time: "2026-09-22T10:10:00Z" }),
    notif({ id: 4, kind: "event" }),
  ];
  const lc = conditionLifecycles(items);
  assert.deepEqual(lc.get(1), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(2), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(3), { state: "ongoing", sinceMs: Date.parse("2026-09-22T10:10:00Z") });
  assert.equal(lc.has(4), false);
});

test("a clear whose onset was trimmed has an unknown duration", () => {
  const lc = conditionLifecycles([notif({ id: 7, kind: "clear", key: "gone" })]);
  assert.deepEqual(lc.get(7), { state: "resolved", durationMs: null });
});

test("conditionLifecycles is order-independent on input", () => {
  const items = [
    notif({ id: 2, kind: "clear", key: "k", time: "2026-09-22T10:00:30Z" }),
    notif({ id: 1, kind: "onset", key: "k", time: "2026-09-22T10:00:00Z" }),
  ];
  assert.deepEqual(conditionLifecycles(items).get(1), { state: "resolved", durationMs: 30_000 });
});

test("formatDuration picks a compact unit", () => {
  assert.equal(formatDuration(45_000), "45s");
  assert.equal(formatDuration(12 * 60_000), "12m");
  assert.equal(formatDuration(3 * 3_600_000 + 5 * 60_000), "3h 05m");
  assert.equal(formatDuration(50 * 3_600_000), "2d 2h");
  assert.equal(formatDuration(-1), "");
  assert.equal(formatDuration(Number.NaN), "");
});

test("exportJSON writes a chronological log", () => {
  const out = JSON.parse(exportJSON([sample[2], sample[0]], "boot-a", "2026-09-22T11:00:00Z")) as {
    bootId: string;
    exportedAt: string;
    events: Notification[];
  };
  assert.equal(out.bootId, "boot-a");
  assert.equal(out.exportedAt, "2026-09-22T11:00:00Z");
  assert.deepEqual(out.events.map((n) => n.id), [1, 3]);
});
