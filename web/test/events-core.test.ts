// Unit tests for the pure Events-page logic (filtering, faceted counts, the
// onset/clear lifecycle pairing, and the formatters). Run with node:test over the
// compiled output (see the web:test task).

import test from "node:test";
import assert from "node:assert/strict";

import {
  CATEGORIES,
  conditionLifecycles,
  dayKey,
  dayLabel,
  emptyFilter,
  exportJSON,
  facetCounts,
  filterEvents,
  formatDuration,
  isFilterActive,
  isoOrNull,
  oldestCaption,
  resultCountLabel,
  rowSignature,
} from "../src/lib/events-core.js";
import type { Notification } from "../src/lib/types.js";
import { notif } from "./fixtures.js";


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
    notif({ id: 1, kind: "onset", key: "k", uptimeMs: 0 }),
    notif({ id: 2, kind: "clear", key: "k", uptimeMs: 180_000 }),
    notif({ id: 3, kind: "onset", key: "k", uptimeMs: 600_000 }),
    notif({ id: 4, kind: "event" }),
  ];
  const lc = conditionLifecycles(items);
  assert.deepEqual(lc.get(1), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(2), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(3), { state: "ongoing", sinceUptimeMs: 600_000 });
  assert.equal(lc.has(4), false);
});

test("a clear whose onset was trimmed has an unknown duration", () => {
  const lc = conditionLifecycles([notif({ id: 7, kind: "clear", key: "gone" })]);
  assert.deepEqual(lc.get(7), { state: "resolved", durationMs: null });
});

test("conditionLifecycles is order-independent on input", () => {
  const items = [
    notif({ id: 2, kind: "clear", key: "k", uptimeMs: 30_000 }),
    notif({ id: 1, kind: "onset", key: "k", uptimeMs: 0 }),
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

interface ExportedLog {
  bootId: string;
  exportedAt: string;
  clockAnchor: { browserTime: string | null; uptimeMs: number } | null;
  events: Array<Notification & { anchoredTime: string | null }>;
}

test("exportJSON writes a chronological log", () => {
  const out = JSON.parse(exportJSON([sample[2], sample[0]], "boot-a", "2026-09-22T11:00:00Z")) as ExportedLog;
  assert.equal(out.bootId, "boot-a");
  assert.equal(out.exportedAt, "2026-09-22T11:00:00Z");
  assert.deepEqual(out.events.map((n) => n.id), [1, 3]);
  // No anchor yet: the export says so rather than inventing a time.
  assert.equal(out.clockAnchor, null);
  assert.equal(out.events[0].anchoredTime, null);
});

test("exportJSON carries the clock anchor and a corrected time per entry", () => {
  // The browser read 12:00:00Z when the appliance had been up 100 s. An entry
  // raised at uptime 40 s therefore happened at 11:59:00Z on the browser clock,
  // whatever wall time the appliance stamped it with (here a pre-NTP 1970 one).
  const anchor = { browserMs: Date.parse("2026-09-22T12:00:00Z"), uptimeMs: 100_000 };
  const early = notif({ id: 1, uptimeMs: 40_000, time: "1970-01-01T00:00:40Z" });
  const out = JSON.parse(exportJSON([early], "boot-a", "2026-09-22T12:01:00Z", anchor)) as ExportedLog;
  assert.deepEqual(out.clockAnchor, { browserTime: "2026-09-22T12:00:00.000Z", uptimeMs: 100_000 });
  assert.equal(out.events[0].anchoredTime, "2026-09-22T11:59:00.000Z");
  // The raw wall-clock time is kept as the appliance sent it.
  assert.equal(out.events[0].time, "1970-01-01T00:00:40Z");
});

// withTZ runs fn with the process time zone set to tz, restoring it after.
function withTZ(tz: string, fn: () => void): void {
  const prev = process.env.TZ;
  process.env.TZ = tz;
  try {
    fn();
  } finally {
    if (prev === undefined) delete process.env.TZ;
    else process.env.TZ = prev;
  }
}

test("dayKey buckets by the local calendar day", () => {
  withTZ("Europe/Helsinki", () => {
    // 21:30Z is 00:30 the next day in Helsinki (UTC+3 in summer).
    assert.equal(dayKey(Date.parse("2026-09-21T21:30:00Z")), "2026-09-22");
    assert.equal(dayKey(Date.parse("2026-09-21T20:59:59Z")), "2026-09-21");
  });
});

test("dayLabel rolls over at local midnight", () => {
  withTZ("Europe/Helsinki", () => {
    const midnight = Date.parse("2026-09-21T21:00:00Z"); // 2026-09-22 00:00 local
    assert.equal(dayLabel(midnight, midnight), "Today");
    assert.equal(dayLabel(midnight - 1000, midnight), "Yesterday");
    assert.equal(dayLabel(midnight - 1000, midnight - 2000), "Today");
    // Two days back falls through to the weekday and date.
    const twoBack = dayLabel(midnight - 25 * 3_600_000, midnight);
    assert.ok(twoBack !== "Today" && twoBack !== "Yesterday", twoBack);
  });
});

test("dayLabel finds yesterday across a 23-hour spring-forward day", () => {
  withTZ("Europe/Helsinki", () => {
    // Clocks go from 03:00 to 04:00 on 2026-03-29, so that day is 23 hours.
    // Now is 00:30 on 03-30; now minus 24 h lands on 03-28, two days back, so
    // subtracting a fixed day would miss yesterday.
    const now = Date.parse("2026-03-29T21:30:00Z");
    const noonOn29 = Date.parse("2026-03-29T09:00:00Z");
    assert.equal(dayLabel(noonOn29, now), "Yesterday");
  });
});

test("dayLabel finds yesterday across a 25-hour fall-back day", () => {
  withTZ("Europe/Helsinki", () => {
    // Clocks go from 04:00 back to 03:00 on 2026-10-25, so that day is 25 hours.
    // Now is 23:30 on 10-25; now minus 24 h is still 10-25, so subtracting a
    // fixed day would label late 10-24 as neither today nor yesterday.
    const now = Date.parse("2026-10-25T21:30:00Z");
    const lateOn24 = Date.parse("2026-10-24T20:45:00Z"); // 23:45 local
    assert.equal(dayLabel(lateOn24, now), "Yesterday");
    assert.equal(dayLabel(Date.parse("2026-10-24T21:15:00Z"), now), "Today"); // 00:15 on 10-25
  });
});

test("oldestCaption shows the time alone for today and prefixes an older day", () => {
  withTZ("UTC", () => {
    const now = Date.parse("2026-09-22T12:00:00Z");
    // The pin controls only dayKey, which picks the day prefix. The time comes
    // from a formatter built at module load in the zone the process started
    // in, and its form is locale-dependent, so only its leading digit is
    // asserted. Today: "Oldest" and the clock time, no day label.
    const today = oldestCaption(Date.parse("2026-09-22T08:05:00Z"), now);
    assert.ok(/^Oldest \d/.test(today), today);
    const yesterday = oldestCaption(Date.parse("2026-09-21T08:05:00Z"), now);
    assert.ok(/^Oldest Yesterday \d/.test(yesterday), yesterday);
    assert.equal(oldestCaption(Number.NaN, now), "");
  });
});

test("rowSignature changes with the text, so an entry updated in place is redrawn", () => {
  const n = notif({ id: 7, kind: "onset", key: "dev:mic", title: "Device failed", message: "retrying" });
  const lc = { state: "ongoing", sinceUptimeMs: 1_000 } as const;
  const base = rowSignature(n, true, lc);
  assert.notEqual(rowSignature({ ...n, title: "Device still failing" }, true, lc), base);
  assert.notEqual(rowSignature({ ...n, message: "retry 3 failed" }, true, lc), base);
  // A separator inside the text cannot make two different pairs collide.
  assert.notEqual(rowSignature({ title: "a|b", message: "c" }, false, undefined), rowSignature({ title: "a", message: "b|c" }, false, undefined));
});

test("rowSignature is stable for unchanged text and state, and follows unread and lifecycle", () => {
  const n = notif({ id: 7, title: "Device failed", message: "retrying" });
  const ongoing = { state: "ongoing", sinceUptimeMs: 1_000 } as const;
  // A later re-send of the same text (a copy, not the same object) keeps the row.
  assert.equal(rowSignature({ ...n }, true, ongoing), rowSignature(n, true, { ...ongoing }));
  assert.equal(rowSignature(n, false, undefined), rowSignature({ ...n }, false, undefined));
  // The ongoing badge's start is restamped in place, not part of the signature.
  assert.equal(rowSignature(n, true, { state: "ongoing", sinceUptimeMs: 5_000 }), rowSignature(n, true, ongoing));
  assert.notEqual(rowSignature(n, false, ongoing), rowSignature(n, true, ongoing));
  assert.notEqual(rowSignature(n, true, { state: "resolved", durationMs: 60_000 }), rowSignature(n, true, ongoing));
  assert.notEqual(rowSignature(n, true, { state: "resolved", durationMs: null }), rowSignature(n, true, { state: "resolved", durationMs: 60_000 }));
  assert.notEqual(rowSignature(n, true, undefined), rowSignature(n, true, ongoing));
});

test("isoOrNull renders an ISO instant and null for an unknown one", () => {
  assert.equal(isoOrNull(Date.parse("2026-09-22T12:00:00Z")), "2026-09-22T12:00:00.000Z");
  assert.equal(isoOrNull(Number.NaN), null);
});

test("resultCountLabel pluralizes and shows N of M only while filtering", () => {
  assert.equal(resultCountLabel(0, 0, false, true), "");
  assert.equal(resultCountLabel(1, 1, false, false), "1 event");
  assert.equal(resultCountLabel(3, 3, false, false), "3 events");
  assert.equal(resultCountLabel(2, 5, true, false), "Showing 2 of 5 events");
  assert.equal(resultCountLabel(0, 1, true, false), "Showing 0 of 1 event");
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
    notif({ id: 1, kind: "onset", key: "k1", uptimeMs: 0 }),
    notif({ id: 2, kind: "onset", key: "k2", uptimeMs: 60_000 }),
    notif({ id: 3, kind: "clear", key: "k1", uptimeMs: 120_000 }),
    notif({ id: 4, kind: "clear", key: "k2", uptimeMs: 300_000 }),
  ];
  const lc = conditionLifecycles(items);
  assert.deepEqual(lc.get(1), { state: "resolved", durationMs: 120_000 });
  assert.deepEqual(lc.get(3), { state: "resolved", durationMs: 120_000 });
  assert.deepEqual(lc.get(2), { state: "resolved", durationMs: 240_000 });
  assert.deepEqual(lc.get(4), { state: "resolved", durationMs: 240_000 });
});

test("conditionLifecycles pairs two cycles on one key separately", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", uptimeMs: 0 }),
    notif({ id: 2, kind: "clear", key: "k", uptimeMs: 180_000 }),
    notif({ id: 3, kind: "onset", key: "k", uptimeMs: 600_000 }),
    notif({ id: 4, kind: "clear", key: "k", uptimeMs: 900_000 }),
  ];
  const lc = conditionLifecycles(items);
  assert.deepEqual(lc.get(1), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(2), { state: "resolved", durationMs: 180_000 });
  assert.deepEqual(lc.get(3), { state: "resolved", durationMs: 300_000 });
  assert.deepEqual(lc.get(4), { state: "resolved", durationMs: 300_000 });
});

test("conditionLifecycles clamps a clear stamped before its onset to zero", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", uptimeMs: 300_000 }),
    notif({ id: 2, kind: "clear", key: "k", uptimeMs: 0 }),
  ];
  assert.deepEqual(conditionLifecycles(items).get(2), { state: "resolved", durationMs: 0 });
});

test("conditionLifecycles reports null duration for a non-finite uptime", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", uptimeMs: Number.NaN }),
    notif({ id: 2, kind: "clear", key: "k", uptimeMs: 60_000 }),
  ];
  assert.deepEqual(conditionLifecycles(items).get(2), { state: "resolved", durationMs: null });
});

// The #88 regression: the server's wall clock stepped from the epoch to the real
// date between onset and clear (an RTC-less Pi syncing NTP), but the monotonic
// uptime shows two minutes passed, and that is what the duration must read.
test("conditionLifecycles ignores a wall-clock step between onset and clear", () => {
  const items = [
    notif({ id: 1, kind: "onset", key: "k", time: "1970-01-01T00:00:40Z", uptimeMs: 40_000 }),
    notif({ id: 2, kind: "clear", key: "k", time: "2026-09-22T10:00:00Z", uptimeMs: 160_000 }),
  ];
  assert.deepEqual(conditionLifecycles(items).get(2), { state: "resolved", durationMs: 120_000 });
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
  // field populated, so it round-trips exactly) plus its anchored time.
  assert.deepEqual(out.events.map((n) => n.id), [1, 2]);
  assert.deepEqual(out.events[1], { ...sample[1], anchoredTime: null });
});

test("facetCounts appends an unknown category after the fixed order", () => {
  const c = facetCounts([notif({ id: 9, category: "future" as Notification["category"] })], emptyFilter());
  assert.deepEqual([...c.category.keys()], [...CATEGORIES, "future"]);
});
