// Unit tests for the pure notification-core reconcile logic. Run with Node's
// built-in test runner over the compiled output (see the web:test task): no
// browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import {
  CLOCK_STEP_TOLERANCE_MS,
  DEFAULT_CAPACITY,
  LoadTracker,
  RESYNC_BASE_MS,
  RESYNC_MAX_MS,
  applyLive,
  applySnapshot,
  activeConditions,
  clearAll,
  clockStepped,
  deserialize,
  eventTimeMs,
  initialState,
  isNotification,
  isSnapshot,
  markAllRead,
  pruneToRing,
  requestMidpoint,
  resyncDelay,
  serialize,
  unreadCount,
  uptimeToMs,
} from "../src/lib/notifications-core.js";
import type { Notification, NotificationSnapshot } from "../src/lib/types.js";
import { notif } from "./fixtures.js";

// A fixed browser clock reading, so anchor arithmetic is exact.
const NOW = Date.parse("2026-09-12T14:00:05Z");

function snap(
  over: Partial<NotificationSnapshot> & { notifications: Notification[] },
): NotificationSnapshot {
  const ids = over.notifications.map((n) => n.id);
  return {
    bootId: over.bootId ?? "boot-a",
    serverTime: over.serverTime ?? "2026-09-12T14:00:05Z",
    uptimeMs: over.uptimeMs ?? 5_000,
    capacity: over.capacity ?? 500,
    nextId: over.nextId ?? (ids.length ? Math.max(...ids) + 1 : 1),
    notifications: over.notifications,
  };
}

test("a fresh snapshot seeds items and bootId, and the started entry reads as unread", () => {
  const s = initialState();
  const started = notif({ id: 1, title: "Appliance started" });
  const { bootChanged } = applySnapshot(s, snap({ notifications: [started] }), Date.now());
  assert.equal(bootChanged, false);
  assert.equal(s.bootId, "boot-a");
  assert.equal(s.items.size, 1);
  assert.equal(s.nextId, 2);
  // Initial watermark is 0 and ids start at 1, so one entry means one unread:
  // the badge shows 1 after the first load.
  assert.equal(unreadCount(s), 1);
});

test("a snapshot with a different bootId resets watermark, dismissed, and items", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }), Date.now());
  markAllRead(s);
  clearAll(s);
  assert.equal(unreadCount(s), 0);

  const { bootChanged } = applySnapshot(
    s,
    snap({ bootId: "boot-b", notifications: [notif({ id: 1, bootId: "boot-b" })] }),
    Date.now(),
  );
  assert.equal(bootChanged, true);
  assert.equal(s.readWatermark, 0);
  assert.equal(s.dismissed.size, 0);
  // The old boot's id 2 must not linger: items are reseeded from the new boot.
  assert.equal(s.items.size, 1);
  assert.equal(unreadCount(s), 1);
});

test("a live event already in the snapshot does not double count", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }), Date.now());
  const { gap, isNewError } = applyLive(s, notif({ id: 2, message: "replayed" }), NOW);
  assert.equal(s.items.size, 2);
  assert.equal(gap, false);
  assert.equal(isNewError, false);
});

test("a live frame repeating a held id with new text replaces it in place, with no toast and no gap", () => {
  // The server re-sends an active condition under its original id when it
  // updates the text in place (Center.Update).
  const s = initialState();
  const onset = notif({ id: 2, severity: "error", kind: "onset", key: "dev:mic", title: "Device failed", message: "retrying" });
  applySnapshot(s, snap({ notifications: [notif({ id: 1 }), onset, notif({ id: 3 })] }), Date.now());
  const updated = { ...onset, title: "Device still failing", message: "retry 3 failed" };
  const { gap, isNewError, resync } = applyLive(s, updated, NOW);
  assert.equal(gap, false);
  assert.equal(isNewError, false);
  assert.equal(resync, false);
  assert.equal(s.items.size, 3);
  assert.equal(s.items.get(2)?.title, "Device still failing");
  assert.equal(s.items.get(2)?.message, "retry 3 failed");
  assert.equal(s.nextId, 4);
});

test("a live id gap is reported, and the expected next id is not", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now()); // nextId 2
  assert.equal(applyLive(s, notif({ id: 2 }), NOW).gap, false);
  assert.equal(applyLive(s, notif({ id: 5 }), NOW).gap, true);
});

test("a live error above the watermark toasts; one below it, and a duplicate, do not", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now());
  markAllRead(s); // watermark 1
  const above = applyLive(s, notif({ id: 2, severity: "error", message: "boom" }), NOW);
  assert.equal(above.isNewError, true);
  // Replaying the same error after a reconnect must not toast again.
  const dup = applyLive(s, notif({ id: 2, severity: "error" }), NOW);
  assert.equal(dup.isNewError, false);

  const below = initialState();
  applySnapshot(below, snap({ notifications: [notif({ id: 5 })] }), Date.now());
  markAllRead(below); // watermark 5
  const under = applyLive(below, notif({ id: 3, severity: "error" }), NOW);
  assert.equal(under.isNewError, false);
});

test("a non-error live event never toasts even when unread", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now());
  const warn = applyLive(s, notif({ id: 2, severity: "warning" }), NOW);
  assert.equal(warn.isNewError, false);
  assert.equal(unreadCount(s), 2);
});

test("markAllRead zeroes unread", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({ notifications: [notif({ id: 1 }), notif({ id: 2 }), notif({ id: 3 })] }),
    Date.now(),
  );
  assert.equal(unreadCount(s), 3);
  markAllRead(s);
  assert.equal(unreadCount(s), 0);
});

test("clearAll dismisses everything, and a later snapshot prunes dismissed ids that fell out of the ring", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({ notifications: [notif({ id: 1 }), notif({ id: 2 }), notif({ id: 3 })] }),
    Date.now(),
  );
  clearAll(s);
  assert.equal(unreadCount(s), 0);
  assert.deepEqual([...s.dismissed].sort((a, b) => a - b), [1, 2, 3]);

  // ids 1 and 2 have aged out; the ring now starts at id 3 and gained id 4.
  applySnapshot(s, snap({ notifications: [notif({ id: 3 }), notif({ id: 4 })] }), Date.now());
  assert.deepEqual([...s.dismissed].sort((a, b) => a - b), [3]);
  assert.deepEqual([...s.items.keys()].sort((a, b) => a - b), [3, 4]);
  // id 3 stays dismissed; id 4 is the only unread.
  assert.equal(unreadCount(s), 1);
});

test("activeConditions returns onsets whose latest entry for their key is an onset", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({
      notifications: [
        notif({ id: 1, kind: "onset", key: "dev:mic", category: "device", title: "Mic failed" }),
        notif({ id: 2, kind: "onset", key: "host:temp", category: "system", title: "Hot" }),
        notif({ id: 3, kind: "clear", key: "dev:mic", category: "device", title: "Mic back" }),
      ],
    }),
    Date.now(),
  );
  const active = activeConditions(s);
  assert.equal(active.length, 1);
  assert.equal(active[0]?.key, "host:temp");
});

test("serialize/deserialize round-trips the persisted subset", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }), Date.now());
  markAllRead(s);
  s.dismissed.add(2);
  const restored = deserialize(serialize(s));
  assert.equal(restored.bootId, s.bootId);
  assert.equal(restored.readWatermark, s.readWatermark);
  assert.deepEqual([...restored.dismissed].sort((a, b) => a - b), [2]);
  // The persisted subset never carries items; they are rebuilt from the server.
  assert.equal(restored.items.size, 0);
});

test("deserialize tolerates null, malformed JSON, and wrong-shaped fields", () => {
  assert.equal(deserialize(null).bootId, null);
  assert.equal(deserialize("").bootId, null);
  assert.equal(deserialize("{not json").bootId, null);
  assert.equal(deserialize("42").bootId, null);
  assert.equal(deserialize("null").bootId, null);
  const partial = deserialize(JSON.stringify({ bootId: 123, readWatermark: "nope", dismissed: "x" }));
  assert.equal(partial.bootId, null);
  assert.equal(partial.readWatermark, 0);
  assert.equal(partial.dismissed.size, 0);
  const mixed = deserialize(JSON.stringify({ bootId: "b", dismissed: [1, "two", 3] }));
  assert.equal(mixed.bootId, "b");
  assert.deepEqual([...mixed.dismissed].sort((a, b) => a - b), [1, 3]);
});

test("applySnapshot anchors the server uptime to the browser clock", () => {
  const s = initialState();
  applySnapshot(s, snap({ uptimeMs: 120_000, notifications: [notif({ id: 1, uptimeMs: 30_000 })] }), NOW);
  assert.deepEqual(s.anchor, { browserMs: NOW, uptimeMs: 120_000 });
  // The entry was published 90s of server uptime before the snapshot.
  assert.equal(eventTimeMs(s, notif({ id: 1, uptimeMs: 30_000 })), NOW - 90_000);
  assert.equal(uptimeToMs(s, 120_000), NOW);
});

// The #88 scenario: an RTC-less Pi stamps an entry with a pre-NTP wall clock (the
// epoch here), then NTP steps the clock to the real date. The entry's placement
// comes from its uptime, so it lands where it really happened, not in 1970, and
// the server/browser skew is irrelevant.
test("eventTimeMs ignores the entry's wall-clock time across a server clock step", () => {
  const s = initialState();
  const early = notif({ id: 1, time: "1970-01-01T00:00:20Z", uptimeMs: 20_000 });
  applySnapshot(s, snap({ serverTime: "2026-09-12T14:00:00Z", uptimeMs: 3_620_000, notifications: [early] }), NOW);
  assert.equal(eventTimeMs(s, early), NOW - 3_600_000);
});

test("a re-sync moves the anchor to the latest snapshot", () => {
  const s = initialState();
  applySnapshot(s, snap({ uptimeMs: 10_000, notifications: [notif({ id: 1 })] }), NOW);
  applySnapshot(s, snap({ uptimeMs: 70_000, notifications: [notif({ id: 1 })] }), NOW + 60_500);
  assert.deepEqual(s.anchor, { browserMs: NOW + 60_500, uptimeMs: 70_000 });
});

test("a live event anchors the clock only when no snapshot has", () => {
  const s = initialState();
  applyLive(s, notif({ id: 1, uptimeMs: 4_000 }), NOW);
  assert.deepEqual(s.anchor, { browserMs: NOW, uptimeMs: 4_000 });
  applyLive(s, notif({ id: 2, uptimeMs: 9_000 }), NOW + 5_100);
  assert.deepEqual(s.anchor, { browserMs: NOW, uptimeMs: 4_000 }, "a later live event must not re-anchor");
});

test("uptimeToMs is NaN before any anchor", () => {
  assert.equal(Number.isNaN(uptimeToMs(initialState(), 1_000)), true);
});

test("applySnapshot adopts the reported capacity and ignores an invalid one", () => {
  const s = initialState();
  assert.equal(s.capacity, DEFAULT_CAPACITY);
  applySnapshot(s, snap({ capacity: 200, notifications: [] }), NOW);
  assert.equal(s.capacity, 200);
  applySnapshot(s, snap({ capacity: 0, notifications: [] }), NOW);
  assert.equal(s.capacity, 200);
});

test("applyLive prunes history the server ring has trimmed", () => {
  const s = initialState();
  applySnapshot(s, snap({ capacity: 3, notifications: [notif({ id: 1 }), notif({ id: 2 }), notif({ id: 3 })] }), NOW);
  s.dismissed.add(1);
  applyLive(s, notif({ id: 4 }), NOW);
  // The ring holds the last 3 ids below nextId 5: 2, 3 and 4.
  assert.deepEqual([...s.items.keys()].sort((a, b) => a - b), [2, 3, 4]);
  assert.equal(s.dismissed.has(1), false, "a dismissed id below the floor is pruned");
});

// Dismissed ids restored from storage before the first snapshot name entries the
// client has not fetched yet. Pruning must not treat "not held" as "trimmed", or
// those entries come back into the bell once the snapshot lands.
test("pruneToRing keeps a dismissed id above the floor that has not arrived yet", () => {
  // Restored before the first snapshot: 8 is below the floor-to-be, 11 at it,
  // 12 above it. The live frames for 11 and 12 were dropped in transit (a gap,
  // which also schedules a re-sync that has not landed yet).
  const s = deserialize(JSON.stringify({ bootId: "boot-a", readWatermark: 0, dismissed: [8, 11, 12] }));
  s.capacity = 5;
  // Live 6..15 leave nextId 16, so the floor is 16 - 5 = 11.
  for (let id = 6; id <= 15; id++) {
    if (id !== 11 && id !== 12) applyLive(s, notif({ id }), NOW);
  }
  assert.deepEqual([...s.dismissed].sort((a, b) => a - b), [11, 12], "8 is pruned; the floor id and above keep their dismissal");
});

test("pruneToRing keeps the dismissal of a pinned onset below the floor", () => {
  const s = initialState();
  const onset = notif({ id: 1, kind: "onset", key: "dev:mic", severity: "error" });
  applySnapshot(s, snap({ capacity: 2, notifications: [onset, notif({ id: 2 })] }), NOW);
  s.dismissed.add(1);
  applyLive(s, notif({ id: 3 }), NOW);
  applyLive(s, notif({ id: 4 }), NOW);
  assert.equal(s.dismissed.has(1), true, "the onset is still held, so its dismissal stays");
});

test("applyLive keeps a trimmed onset whose condition is still active", () => {
  const s = initialState();
  const onset = notif({ id: 1, kind: "onset", key: "dev:mic", severity: "error" });
  applySnapshot(s, snap({ capacity: 2, notifications: [onset, notif({ id: 2 })] }), NOW);
  applyLive(s, notif({ id: 3 }), NOW);
  applyLive(s, notif({ id: 4 }), NOW);
  assert.deepEqual([...s.items.keys()].sort((a, b) => a - b), [1, 3, 4]);
  // Once cleared, the onset is no longer pinned and ages out like any entry.
  applyLive(s, notif({ id: 5, kind: "clear", key: "dev:mic" }), NOW);
  assert.deepEqual([...s.items.keys()].sort((a, b) => a - b), [4, 5]);
});

test("pruneToRing leaves a state within its bound untouched", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }), NOW);
  pruneToRing(s);
  assert.equal(s.items.size, 2);
});

test("snapshot pruning drops ids aged out of the ring even when a pinned active condition holds a low id", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({
      notifications: [
        notif({ id: 1, kind: "onset", key: "dev:mic", severity: "error", title: "Mic failed" }),
        notif({ id: 2 }),
        notif({ id: 3 }),
        notif({ id: 4 }),
        notif({ id: 5 }),
      ],
      nextId: 6,
    }),
    Date.now(),
  );
  assert.deepEqual([...s.items.keys()].sort((a, b) => a - b), [1, 2, 3, 4, 5]);

  // Reconnect: the ring advanced and ids 2,3 aged out, but the active onset
  // (id 1) is still pinned into the snapshot at its low id. Pruning must still
  // drop 2 and 3 even though they sit above the oldest id present (1).
  applySnapshot(
    s,
    snap({
      notifications: [
        notif({ id: 1, kind: "onset", key: "dev:mic", severity: "error", title: "Mic failed" }),
        notif({ id: 4 }),
        notif({ id: 5 }),
        notif({ id: 6 }),
      ],
      nextId: 7,
    }),
    Date.now(),
  );
  assert.deepEqual([...s.items.keys()].sort((a, b) => a - b), [1, 4, 5, 6]);
});

test("nextId is monotonic within a boot so a stale snapshot does not cause a spurious gap", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })], nextId: 2 }), Date.now());
  assert.equal(applyLive(s, notif({ id: 2 }), NOW).gap, false); // advances nextId to 3
  assert.equal(s.nextId, 3);
  // A snapshot taken before that live event (nextId 2) must not roll nextId back.
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })], nextId: 2 }), Date.now());
  assert.equal(s.nextId, 3);
  assert.equal(applyLive(s, notif({ id: 3 }), NOW).gap, false); // next id is not a spurious gap
});

test("clearAll keeps active conditions visible and zeroes unread", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({
      notifications: [
        notif({ id: 1, kind: "onset", key: "dev:mic", severity: "error", title: "Mic failed" }),
        notif({ id: 2, title: "history" }),
        notif({ id: 3, title: "history" }),
      ],
      nextId: 4,
    }),
    Date.now(),
  );
  clearAll(s);
  assert.equal(s.dismissed.has(1), false); // active onset stays visible
  assert.equal(s.dismissed.has(2), true);
  assert.equal(s.dismissed.has(3), true);
  assert.deepEqual(activeConditions(s).map((n) => n.id), [1]);
  assert.equal(unreadCount(s), 0);
});

test("a dismissed id does not toast even as a fresh live error above the watermark", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now()); // watermark 0
  s.dismissed.add(2); // id 2 was dismissed before its live frame arrives
  const r = applyLive(s, notif({ id: 2, severity: "error" }), NOW);
  assert.equal(r.isNewError, false);
});

test("activeConditions returns multiple active onsets ascending by id regardless of input order", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({
      notifications: [
        notif({ id: 3, kind: "onset", key: "host:temp", title: "Hot" }),
        notif({ id: 1, kind: "onset", key: "dev:mic", title: "Mic failed" }),
      ],
      nextId: 4,
    }),
    Date.now(),
  );
  assert.deepEqual(activeConditions(s).map((n) => n.id), [1, 3]);
});

test("activeConditions ignores a keyless onset", () => {
  const s = initialState();
  applySnapshot(
    s,
    snap({
      notifications: [
        notif({ id: 1, kind: "onset", title: "no key" }),
        notif({ id: 2, kind: "onset", key: "dev:mic", title: "Mic failed" }),
      ],
      nextId: 3,
    }),
    Date.now(),
  );
  assert.deepEqual(activeConditions(s).map((n) => n.id), [2]);
});

test("serialize persists only the read-state subset, never items", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }), Date.now());
  markAllRead(s);
  s.dismissed.add(2);
  const raw = JSON.parse(serialize(s)) as Record<string, unknown>;
  assert.deepEqual(Object.keys(raw).sort(), ["bootId", "dismissed", "readWatermark"]);
});

test("applyLive signals resync and leaves state untouched on a bootId mismatch", () => {
  const s = initialState();
  applySnapshot(s, snap({ bootId: "boot-a", notifications: [notif({ id: 1 })] }), Date.now());
  const sizeBefore = s.items.size;
  // A restart: the new boot sends id 1, which would collide with the old id 1.
  const r = applyLive(s, notif({ id: 1, bootId: "boot-b", severity: "error", title: "new boot" }), NOW);
  assert.equal(r.resync, true);
  assert.equal(r.gap, false);
  assert.equal(r.isNewError, false);
  assert.equal(s.items.size, sizeBefore); // state untouched; the shell reloads
  assert.equal(s.bootId, "boot-a");
});

test("applyLive adopts the boot identity before the first snapshot instead of resyncing", () => {
  const s = initialState();
  const r = applyLive(s, notif({ id: 1, bootId: "boot-x" }), NOW);
  assert.equal(r.resync, false);
  assert.equal(s.items.size, 1);
  assert.equal(s.bootId, "boot-x");
});

test("a pre-snapshot live event does not merge into a later different-boot snapshot", () => {
  const s = initialState();
  applyLive(s, notif({ id: 1, bootId: "boot-a" }), NOW); // adopts boot-a
  const { bootChanged } = applySnapshot(
    s,
    snap({ bootId: "boot-b", notifications: [notif({ id: 1, bootId: "boot-b", title: "fresh" })] }),
    Date.now(),
  );
  assert.equal(bootChanged, true);
  assert.equal(s.items.size, 1);
  assert.equal([...s.items.values()][0]?.title, "fresh"); // the boot-a event was dropped
});

test("isNotification accepts a well-formed notification and rejects malformed ones", () => {
  assert.equal(isNotification(notif({ id: 1 })), true);
  assert.equal(isNotification(null), false);
  assert.equal(isNotification("nope"), false);
  assert.equal(
    isNotification({ id: "x", bootId: "b", time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t", message: "m" }),
    false,
  ); // non-numeric id
  assert.equal(
    isNotification({ id: 1, bootId: "b", time: "t", uptimeMs: 0, severity: "critical", category: "system", kind: "event", title: "t", message: "m" }),
    false,
  ); // off-enum severity
  assert.equal(
    isNotification({ id: 1, time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t", message: "m" }),
    false,
  ); // missing bootId
  assert.equal(
    isNotification({ id: 1, bootId: "b", time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t" }),
    false,
  ); // missing message
  assert.equal(
    isNotification({ id: 1.5, bootId: "b", time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t", message: "m" }),
    false,
  ); // fractional id
  assert.equal(
    isNotification({ id: 0, bootId: "b", time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t", message: "m" }),
    false,
  ); // id below 1
  assert.equal(
    isNotification({ id: 1, bootId: "b", time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t", message: "m", key: 5 }),
    false,
  ); // non-string key
  assert.equal(
    isNotification({ id: 1, bootId: "b", time: "t", uptimeMs: 0, severity: "info", category: "system", kind: "event", title: "t", message: "m", key: "dev:mic", source: "mic0" }),
    true,
  ); // valid with string key and source
  const base = { id: 1, bootId: "b", time: "t", severity: "info", category: "system", kind: "event", title: "t", message: "m" };
  assert.equal(isNotification(base), false); // missing uptimeMs
  assert.equal(isNotification({ ...base, uptimeMs: -1 }), false); // negative uptime
  assert.equal(isNotification({ ...base, uptimeMs: Number.NaN }), false); // non-finite uptime
  assert.equal(isNotification({ ...base, uptimeMs: "5" }), false); // non-numeric uptime
});

test("activeConditions keeps the highest id per key whatever the map order", () => {
  const s = initialState();
  // Inserted newest first, so a first-seen or last-seen rule would pick wrong.
  s.items.set(4, notif({ id: 4, kind: "onset", key: "k" }));
  s.items.set(3, notif({ id: 3, kind: "clear", key: "k" }));
  s.items.set(2, notif({ id: 2, kind: "onset", key: "k" }));
  s.items.set(6, notif({ id: 6, kind: "clear", key: "j" }));
  s.items.set(5, notif({ id: 5, kind: "onset", key: "j" }));
  assert.deepEqual(activeConditions(s).map((n) => n.id), [4]);
});

test("isSnapshot accepts a well-formed snapshot and rejects anything else", () => {
  const good = snap({ notifications: [notif({ id: 1 })] });
  assert.equal(isSnapshot(good), true);
  assert.equal(isSnapshot("<html>captive portal</html>"), false);
  assert.equal(isSnapshot(null), false);
  assert.equal(isSnapshot({ ...good, bootId: 7 }), false);
  assert.equal(isSnapshot({ ...good, serverTime: undefined }), false);
  assert.equal(isSnapshot({ ...good, uptimeMs: Number.NaN }), false);
  assert.equal(isSnapshot({ ...good, nextId: "2" }), false);
  assert.equal(isSnapshot({ ...good, notifications: "none" }), false);
  // A malformed entry, or one from another boot, poisons the whole snapshot.
  assert.equal(isSnapshot({ ...good, notifications: [{ id: 1 }] }), false);
  assert.equal(isSnapshot({ ...good, notifications: [notif({ id: 1, bootId: "boot-b" })] }), false);
});

test("requestMidpoint anchors halfway through the request", () => {
  assert.equal(requestMidpoint(1_000, 1_400), 1_200);
  assert.equal(requestMidpoint(1_000, 1_000), 1_000);
});

test("a snapshot anchored at the request midpoint maps entries by half the round trip less", () => {
  // The server read uptime 5 s during a request sent at NOW and answered 2 s
  // later; the midpoint places uptime 5 s at NOW + 1 s rather than NOW + 2 s.
  const s = initialState();
  applySnapshot(s, snap({ uptimeMs: 5_000, notifications: [] }), requestMidpoint(NOW, NOW + 2_000));
  assert.equal(uptimeToMs(s, 5_000), NOW + 1_000);
});

test("clockStepped ignores drift within the tolerance and flags a step either way", () => {
  const ref = { wallMs: NOW, monoMs: 50_000 };
  // Both clocks advanced a minute together.
  assert.equal(clockStepped(ref, NOW + 60_000, 110_000), false);
  // Jitter up to the tolerance is not a step.
  assert.equal(clockStepped(ref, NOW + 60_000 + CLOCK_STEP_TOLERANCE_MS, 110_000), false);
  // The wall clock jumped forward an hour (or a suspend that paused the
  // monotonic clock), or stepped back a minute.
  assert.equal(clockStepped(ref, NOW + 3_660_000, 110_000), true);
  assert.equal(clockStepped(ref, NOW, 110_000), true);
});

test("resyncDelay backs off exponentially up to the cap", () => {
  assert.equal(resyncDelay(1), RESYNC_BASE_MS);
  assert.equal(resyncDelay(2), RESYNC_BASE_MS * 2);
  assert.equal(resyncDelay(3), RESYNC_BASE_MS * 4);
  assert.equal(resyncDelay(0), RESYNC_BASE_MS);
  assert.equal(resyncDelay(50), RESYNC_MAX_MS);
  assert.equal(resyncDelay(1_000_000), RESYNC_MAX_MS);
});

test("LoadTracker starts neither loaded nor failed", () => {
  const t = new LoadTracker();
  assert.equal(t.hasLoaded(), false);
  assert.equal(t.hasFailed(), false);
});

test("LoadTracker: a failure is reported, and a later success clears it", () => {
  const t = new LoadTracker();
  const a = t.begin();
  assert.equal(t.fail(a), true);
  assert.equal(t.hasFailed(), true);
  assert.equal(t.hasLoaded(), false);
  const b = t.begin();
  assert.equal(t.canApply(b), true);
  t.applied(b);
  assert.equal(t.hasLoaded(), true);
  assert.equal(t.hasFailed(), false);
  // A repeat failure after the success reports again (the page learns of it).
  const c = t.begin();
  assert.equal(t.fail(c), true);
  assert.equal(t.fail(c), true);
  assert.equal(t.hasFailed(), true);
  assert.equal(t.hasLoaded(), true);
});

test("LoadTracker: a newer load that fails does not discard an older valid one", () => {
  const t = new LoadTracker();
  const older = t.begin();
  const newer = t.begin();
  assert.equal(t.fail(newer), true);
  // The older load's snapshot arrives after the newer failed: still applied.
  assert.equal(t.canApply(older), true);
  t.applied(older);
  assert.equal(t.hasFailed(), false);
  assert.equal(t.hasLoaded(), true);
});

test("LoadTracker: an older load cannot overwrite or fail a newer applied one", () => {
  const t = new LoadTracker();
  const older = t.begin();
  const newer = t.begin();
  t.applied(newer);
  assert.equal(t.canApply(older), false);
  // Its failure is stale too: the newer success is the fresher truth.
  assert.equal(t.fail(older), false);
  assert.equal(t.hasFailed(), false);
});

test("LoadTracker: a load merely started later does not block an earlier one", () => {
  const t = new LoadTracker();
  const first = t.begin();
  t.begin(); // still in flight
  assert.equal(t.canApply(first), true);
});
