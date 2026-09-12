// Unit tests for the pure notification-core reconcile logic. Run with Node's
// built-in test runner over the compiled output (see the web:test task): no
// browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import {
  applyLive,
  applySnapshot,
  activeConditions,
  clearAll,
  deserialize,
  initialState,
  markAllRead,
  serialize,
  unreadCount,
} from "../src/lib/notifications-core.js";
import type { Notification, NotificationSnapshot } from "../src/lib/types.js";

function notif(over: Partial<Notification> & { id: number }): Notification {
  return {
    id: over.id,
    bootId: over.bootId ?? "boot-a",
    time: over.time ?? "2026-09-12T14:00:00Z",
    severity: over.severity ?? "info",
    category: over.category ?? "system",
    kind: over.kind ?? "event",
    key: over.key,
    source: over.source,
    title: over.title ?? "Title",
    message: over.message ?? "Message",
  };
}

function snap(
  over: Partial<NotificationSnapshot> & { notifications: Notification[] },
): NotificationSnapshot {
  const ids = over.notifications.map((n) => n.id);
  return {
    bootId: over.bootId ?? "boot-a",
    serverTime: over.serverTime ?? "2026-09-12T14:00:05Z",
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
  const { gap, isNewError } = applyLive(s, notif({ id: 2, message: "replayed" }));
  assert.equal(s.items.size, 2);
  assert.equal(gap, false);
  assert.equal(isNewError, false);
});

test("a live id gap is reported, and the expected next id is not", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now()); // nextId 2
  assert.equal(applyLive(s, notif({ id: 2 })).gap, false);
  assert.equal(applyLive(s, notif({ id: 5 })).gap, true);
});

test("a live error above the watermark toasts; one below it, and a duplicate, do not", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now());
  markAllRead(s); // watermark 1
  const above = applyLive(s, notif({ id: 2, severity: "error", message: "boom" }));
  assert.equal(above.isNewError, true);
  // Replaying the same error after a reconnect must not toast again.
  const dup = applyLive(s, notif({ id: 2, severity: "error" }));
  assert.equal(dup.isNewError, false);

  const below = initialState();
  applySnapshot(below, snap({ notifications: [notif({ id: 5 })] }), Date.now());
  markAllRead(below); // watermark 5
  const under = applyLive(below, notif({ id: 3, severity: "error" }));
  assert.equal(under.isNewError, false);
});

test("a non-error live event never toasts even when unread", () => {
  const s = initialState();
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })] }), Date.now());
  const warn = applyLive(s, notif({ id: 2, severity: "warning" }));
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

test("a malformed server time leaves the clock offset at zero", () => {
  const s = initialState();
  applySnapshot(s, { bootId: "boot-a", serverTime: "not-a-date", nextId: 2, notifications: [notif({ id: 1 })] }, 10_000);
  assert.equal(s.serverOffsetMs, 0);
});

test("applySnapshot computes a non-zero serverOffsetMs from the server clock skew", () => {
  const s = initialState();
  // Server clock reads 5s behind the browser's nowMs at snapshot time.
  const nowMs = Date.parse("2026-09-12T14:00:05Z");
  applySnapshot(
    s,
    { bootId: "boot-a", serverTime: "2026-09-12T14:00:00Z", nextId: 2, notifications: [notif({ id: 1 })] },
    nowMs,
  );
  assert.equal(s.serverOffsetMs, 5000);
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
  assert.equal(applyLive(s, notif({ id: 2 })).gap, false); // advances nextId to 3
  assert.equal(s.nextId, 3);
  // A snapshot taken before that live event (nextId 2) must not roll nextId back.
  applySnapshot(s, snap({ notifications: [notif({ id: 1 })], nextId: 2 }), Date.now());
  assert.equal(s.nextId, 3);
  assert.equal(applyLive(s, notif({ id: 3 })).gap, false); // next id is not a spurious gap
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
  const r = applyLive(s, notif({ id: 2, severity: "error" }));
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
