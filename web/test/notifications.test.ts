// A harness for NotificationStore with a fake snapshot endpoint, event stream,
// connection source and timers, pinning its re-sync wiring: a failed load on
// connect retries with backoff, a 401 defers to the login flow, an applied load
// clears a pending backoff retry but not a pending gap re-sync, the stream
// going down drops a pending retry and arms no new one until it is back, and
// the store announces "change" after everything the views render from. Run
// with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { ApiError } from "../src/lib/api.ts";
import { NotificationStore, type ConnectionSource } from "../src/lib/notifications.ts";
import { prefSaveNotice } from "../src/lib/prefs.ts";
import { resyncDelay } from "../src/lib/notifications-core.ts";
import type { Notification, NotificationSnapshot } from "../src/lib/types.ts";
import type { Router } from "../src/lib/router.ts";
import type { AppStore } from "../src/lib/store.ts";
import { FakeConnection, FakeStream, FakeTimers, notif, settle, snap } from "./fixtures.ts";

// Compile-time checks, never called: the connection seam takes the app store
// and the fake, but not an emitter of another event map.
export function connectionSeamTypes(store: AppStore, router: Router): ConnectionSource[] {
  // @ts-expect-error: the router emits "route", not "connection".
  const wrong: ConnectionSource = router;
  return [store, new FakeConnection(), wrong];
}

interface Harness {
  ns: NotificationStore;
  timers: FakeTimers;
  // push queues the next outcome of GET /notifications: a snapshot to resolve
  // with, or an Error to reject with.
  push(outcome: NotificationSnapshot | Error): void;
  calls: () => number;
  // connect and disconnect announce the "connection" event the app store would.
  connect(): void;
  disconnect(): void;
  // live delivers a "notification" frame, as the event stream would.
  live(n: Notification): void;
  // frame delivers any event-stream frame.
  frame(name: string, data: unknown): void;
  // changes counts the store's "change" announcements.
  changes: () => number;
}

function harness(): Harness {
  const timers = new FakeTimers();
  const queue: (NotificationSnapshot | Error)[] = [];
  let calls = 0;
  const stream = new FakeStream();
  const connection = new FakeConnection();
  const ns = new NotificationStore({
    api: {
      getNotifications: () => {
        calls++;
        const o = queue.shift() ?? new Error("getNotifications: nothing queued");
        return o instanceof Error ? Promise.reject(o) : Promise.resolve(o);
      },
    },
    sse: { subscribe: stream.subscribe },
    connection,
    timers,
  });
  let changes = 0;
  ns.on("change", () => changes++);
  return {
    ns,
    timers,
    push: (o) => queue.push(o),
    calls: () => calls,
    connect: () => connection.set(true),
    disconnect: () => connection.set(false),
    live: (n) => stream.deliver("notification", n),
    frame: (name, data) => stream.deliver(name, data),
    changes: () => changes,
  };
}

test("a failed load on connect retries with backoff until it applies", async () => {
  const h = harness();
  h.push(new Error("offline"));
  h.connect();
  await settle();
  assert.equal(h.calls(), 1);
  assert.equal(h.ns.hasFailed(), true);
  const [retry] = h.timers.pending(resyncDelay(1));
  assert.ok(retry, "no backoff retry scheduled after a failed connect load");
  // The retry fails too: the next one backs off further.
  h.push(new Error("still offline"));
  h.timers.fire(retry);
  await settle();
  assert.equal(h.calls(), 2);
  const [second] = h.timers.pending(resyncDelay(2));
  assert.ok(second, "no longer backoff after a second failure");
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  h.timers.fire(second);
  await settle();
  assert.equal(h.ns.hasLoaded(), true);
  assert.equal(h.ns.hasFailed(), false);
  assert.equal(h.timers.pending().length, 0);
});

test("a 401 on connect defers to the login flow instead of retrying", async () => {
  const h = harness();
  h.push(new ApiError(401, "Unauthorized"));
  h.connect();
  await settle();
  assert.equal(h.ns.hasFailed(), true);
  assert.equal(h.timers.pending().length, 0);
});

test("an applied load clears a pending backoff retry and resets the backoff", async () => {
  const h = harness();
  h.push(new Error("offline"));
  h.connect();
  await settle();
  assert.equal(h.timers.pending(resyncDelay(1)).length, 1);
  // A Retry (or any other load) lands first.
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  assert.equal(await h.ns.load(), true);
  assert.equal(h.timers.pending().length, 0);
  // The attempt count restarted: the next failure waits the first delay again.
  h.push(new Error("offline"));
  h.connect();
  await settle();
  assert.equal(h.timers.pending(resyncDelay(1)).length, 1);
  assert.equal(h.timers.pending(resyncDelay(2)).length, 0);
});

test("an applied load keeps a pending gap re-sync", async () => {
  const h = harness();
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  h.connect();
  await settle();
  // Ids 2 to 4 were dropped: the gap schedules a re-sync.
  h.live(notif({ id: 5 }));
  const pending = h.timers.pending();
  assert.equal(pending.length, 1);
  // A load that started before the gap may not hold the dropped events, so
  // the re-sync must survive it.
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  await h.ns.load();
  assert.deepEqual(h.timers.pending(), pending);
});

test("a gap absorbed into a pending backoff retry survives an applied load", async () => {
  const h = harness();
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  h.connect();
  await settle();
  h.push(new Error("offline"));
  h.connect();
  await settle();
  const pending = h.timers.pending(resyncDelay(1));
  assert.equal(pending.length, 1);
  h.live(notif({ id: 5 }));
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  await h.ns.load();
  assert.deepEqual(h.timers.pending(), pending);
});

test("the stream going down drops a pending retry", async () => {
  const h = harness();
  h.push(new Error("offline"));
  h.connect();
  await settle();
  assert.equal(h.timers.pending().length, 1);
  h.disconnect();
  assert.equal(h.timers.pending().length, 0);
});

test("a load failing after the stream went down arms no retry until it is back", async () => {
  const h = harness();
  h.push(new Error("offline"));
  h.connect();
  // The stream goes down (a hidden page past its grace) while the connect-time
  // load is still in flight; that load then fails.
  h.disconnect();
  await settle();
  assert.equal(h.calls(), 1);
  assert.equal(h.ns.hasFailed(), true);
  assert.equal(h.timers.pending().length, 0);
  // Nothing else asks for the snapshot while the stream is down.
  await settle();
  assert.equal(h.calls(), 1);
  // Connecting again re-syncs at once.
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  h.connect();
  await settle();
  assert.equal(h.calls(), 2);
  assert.equal(h.ns.hasLoaded(), true);
});

test("change fires after a snapshot, a live event, mark-all-read, clear-all and a failed load", async () => {
  const h = harness();
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  h.connect();
  await settle();
  assert.equal(h.changes(), 1, "an applied snapshot must announce");
  h.live(notif({ id: 2 }));
  assert.equal(h.changes(), 2, "a live event must announce");
  h.ns.markAllRead();
  assert.equal(h.changes(), 3, "mark-all-read must announce");
  h.ns.clearAll();
  assert.equal(h.changes(), 4, "clear-all must announce");
  // An applied snapshot announces even when it holds nothing new.
  h.push(snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }));
  await h.ns.load();
  assert.equal(h.changes(), 5);
  // A failed load announces, so a page can offer Retry.
  h.push(new Error("offline"));
  await h.ns.load();
  assert.equal(h.changes(), 6, "a failed load must announce");
});

test("change does not fire for a frame the store does not apply", async () => {
  const h = harness();
  h.push(snap({ notifications: [notif({ id: 1 })] }));
  h.connect();
  await settle();
  const before = h.changes();
  // A frame of another type is ignored even when its data would be a valid
  // notification.
  h.frame("heartbeat", notif({ id: 2 }));
  // A malformed notification from the current boot is rejected outright; it
  // must not be folded in, nor mistaken for another boot and re-synced.
  h.frame("notification", { id: 2, bootId: "boot-a" });
  assert.equal(h.changes(), before, "a heartbeat or a malformed frame must not announce");
  assert.equal(h.timers.pending().length, 0, "a malformed frame must not schedule a re-sync");
  // A frame from another boot is not folded in; the re-sync it schedules
  // announces instead.
  h.live(notif({ id: 2, bootId: "boot-b" }));
  assert.equal(h.changes(), before, "a frame from another boot must not announce");
  assert.equal(h.timers.pending().length, 1, "a frame from another boot schedules a re-sync");
});

test("an empty first snapshot announces", async () => {
  const h = harness();
  h.push(snap({ notifications: [] }));
  h.connect();
  await settle();
  // The Events page leaves its loading state on this announcement, even with
  // nothing in the log.
  assert.equal(h.changes(), 1);
  assert.equal(h.ns.hasLoaded(), true);
});

// This test sets the page-wide notice handler, so it must stay the last in the
// file that can report (web/test/notifications-clear.test.ts covers clear-all in
// a fresh process).
test("a failed automatic save stays silent; a failed mark-all-read reports once", async () => {
  // Every write fails, as in a browser with site data blocked (set here rather
  // than relying on node having no localStorage).
  const g = globalThis as unknown as { localStorage?: unknown };
  const saved = g.localStorage;
  g.localStorage = { getItem: () => null, setItem: () => { throw new Error("blocked"); } };
  try {
    let shown = 0;
    prefSaveNotice.setHandler(() => shown++);
    const h = harness();
    h.push(snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] }));
    h.connect();
    await settle();
    // The snapshot's write (it records the boot id) fails, unprompted.
    assert.equal(shown, 0, "a snapshot write is not the operator's choice");
    h.ns.markAllRead();
    assert.equal(shown, 1);
    h.ns.clearAll();
    assert.equal(shown, 1, "the notice shows once per page");
  } finally {
    g.localStorage = saved;
  }
});
