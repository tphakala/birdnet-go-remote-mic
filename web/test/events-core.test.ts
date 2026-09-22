// Unit tests for the pure Events-page logic (filtering, faceted counts, the
// onset/clear lifecycle pairing, and the formatters). Run with node:test over the
// compiled output (see the web:test task).

import test from "node:test";
import assert from "node:assert/strict";

import {
  CATEGORIES,
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

test("facetCounts honours a category selection when counting severities", () => {
  const f = emptyFilter();
  f.categories.add("device");
  const c = facetCounts(sample, f);
  // Only the device entry (id 2, an error) survives the category filter, so the
  // severity counts collapse onto it.
  assert.deepEqual(c.severity, { error: 1, warning: 0, info: 0 });
});

test("facetCounts under a source selection counts both facets exactly", () => {
  const f = emptyFilter();
  f.source = "usbmic";
  const c = facetCounts(sample, f);
  // usbmic carries the device error (id 2) and the audio warning (id 3).
  assert.deepEqual(c.severity, { error: 1, warning: 1, info: 0 });
  assert.deepEqual(
    [...c.category.entries()],
    [["device", 1], ["audio", 1], ["stream", 0], ["system", 0], ["config", 0]],
  );
});

test("conditionLifecycles pairs interleaved keys independently", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k1", time: "2026-09-22T10:00:00Z" }),
    notif({ id: 2, kind: "onset", key: "k2", time: "2026-09-22T10:01:00Z" }),
    notif({ id: 3, kind: "clear", key: "k1", time: "2026-09-22T10:02:00Z" }),
    notif({ id: 4, kind: "clear", key: "k2", time: "2026-09-22T10:05:00Z" }),
  ];
  const lc = conditionLifecycles(items);
  assert.deepEqual(lc.get(1), { state: "resolved", durationMs: 120_000 });
  assert.deepEqual(lc.get(3), { state: "resolved", durationMs: 120_000 });
  assert.deepEqual(lc.get(2), { state: "resolved", durationMs: 240_000 });
  assert.deepEqual(lc.get(4), { state: "resolved", durationMs: 240_000 });
});

test("conditionLifecycles pairs two cycles on one key separately", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", time: "2026-09-22T10:00:00Z" }),
    notif({ id: 2, kind: "clear", key: "k", time: "2026-09-22T10:03:00Z" }),
    notif({ id: 3, kind: "onset", key: "k", time: "2026-09-22T10:10:00Z" }),
    notif({ id: 4, kind: "clear", key: "k", time: "2026-09-22T10:15:00Z" }),
  ];
  const lc = conditionLifecycles(items);
  assert.deepEqual(lc.get(1), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(2), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(3), { state: "resolved", durationMs: 300_000 });
  assert.deepEqual(lc.get(4), { state: "resolved", durationMs: 300_000 });
});

test("conditionLifecycles clamps a clear stamped before its onset to zero", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", time: "2026-09-22T10:05:00Z" }),
    notif({ id: 2, kind: "clear", key: "k", time: "2026-09-22T10:00:00Z" }),
  ];
  assert.deepEqual(conditionLifecycles(items).get(2), { state: "resolved", durationMs: 0 });
});

test("conditionLifecycles reports null duration for an unparseable time", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", time: "not-a-date" }),
    notif({ id: 2, kind: "clear", key: "k", time: "2026-09-22T10:00:00Z" }),
  ];
  assert.deepEqual(conditionLifecycles(items).get(2), { state: "resolved", durationMs: null });
});

test("conditionLifecycles ignores a keyless onset", () => {
  const lc = conditionLifecycles([notif({ id: 1, kind: "onset" })]);
  assert.equal(lc.has(1), false);
  assert.equal(lc.size, 0);
});

test("isFilterActive is true for a source-only or category-only filter", () => {
  const bySource = emptyFilter();
  bySource.source = "usbmic";
  assert.equal(isFilterActive(bySource), true);
  const byCategory = emptyFilter();
  byCategory.categories.add("device");
  assert.equal(isFilterActive(byCategory), true);
});

test("query matches the title alone", () => {
  const f = emptyFilter();
  f.query = "appliance";
  assert.deepEqual(filterEvents(sample, f).map((n) => n.id), [1]);
});

test("filterEvents returns newest first regardless of input order", () => {
  const shuffled = [sample[1], sample[3], sample[0], sample[2]];
  assert.deepEqual(filterEvents(shuffled, emptyFilter()).map((n) => n.id), [4, 3, 2, 1]);
});

test("formatDuration crosses unit boundaries cleanly", () => {
  assert.equal(formatDuration(59_999), "59s");
  assert.equal(formatDuration(60_000), "1m");
  assert.equal(formatDuration(3_600_000), "1h 00m");
  assert.equal(formatDuration(86_400_000), "1d 0h");
  assert.equal(formatDuration(Number.POSITIVE_INFINITY), "");
});

test("exportJSON leaves its input untouched and carries whole events", () => {
  const input = [sample[1], sample[0]];
  const out = JSON.parse(exportJSON(input, "boot-a", "2026-09-22T11:00:00Z")) as {
    events: Notification[];
  };
  // exportJSON sorts a copy, so the caller's array order is unchanged.
  assert.deepEqual(input.map((n) => n.id), [2, 1]);
  // Oldest first, and each entry is the whole event object (sample[1] has every
  // field populated, so it round-trips exactly).
  assert.deepEqual(out.events.map((n) => n.id), [1, 2]);
  assert.deepEqual(out.events[1], sample[1]);
});

test("facetCounts appends an unknown category after the fixed order", () => {
  const c = facetCounts([notif({ id: 9, category: "future" as Notification["category"] })], emptyFilter());
  assert.deepEqual([...c.category.keys()], [...CATEGORIES, "future"]);
});
