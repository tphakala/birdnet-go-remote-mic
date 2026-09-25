// A harness for NotificationStore with a fake snapshot endpoint, event stream,
// connection source and timers, pinning its re-sync wiring: a failed load on
// connect retries with backoff, a 401 defers to the login flow, an applied load
// clears a pending backoff retry but not a pending gap re-sync, and the stream
// going down drops a pending retry. Run with node:test over the compiled output
// (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { ApiError } from "../src/lib/api.js";
import { NotificationStore } from "../src/lib/notifications.js";
import { resyncDelay } from "../src/lib/notifications-core.js";
import type { Notification, NotificationSnapshot } from "../src/lib/types.js";
import { FakeTimers, notif } from "./fixtures.js";

interface Harness {
  ns: NotificationStore;
  timers: FakeTimers;
  // push queues the next outcome of GET /notifications: a snapshot to resolve
  // with, or an Error to reject with.
  push(outcome: NotificationSnapshot | Error): void;
  calls: () => number;
  // connect and disconnect dispatch the app store's "connection" event.
  connect(): void;
  disconnect(): void;
  // live delivers a "notification" frame, as the event stream would.
  live(n: Notification): void;
}

function harness(): Harness {
  const timers = new FakeTimers();
  const queue: (NotificationSnapshot | Error)[] = [];
  let calls = 0;
  let handler: ((name: string, data: unknown) => void) | null = null;
  const connection = new EventTarget();
  const ns = new NotificationStore({
    api: {
      getNotifications: () => {
        calls++;
        const o = queue.shift() ?? new Error("getNotifications: nothing queued");
        return o instanceof Error ? Promise.reject(o) : Promise.resolve(o);
      },
    },
    sse: {
      subscribe: (h) => {
        handler = h;
        return () => true;
      },
    },
    connection,
    timers,
  });
  return {
    ns,
    timers,
    push: (o) => queue.push(o),
    calls: () => calls,
    connect: () => connection.dispatchEvent(new CustomEvent("connection", { detail: true })),
    disconnect: () => connection.dispatchEvent(new CustomEvent("connection", { detail: false })),
    live: (n) => handler?.("notification", n),
  };
}

function snap(notifications: Notification[]): NotificationSnapshot {
  return {
    bootId: "boot-a",
    serverTime: "2026-09-12T14:00:05Z",
    uptimeMs: 5_000,
    capacity: 500,
    nextId: notifications.length ? Math.max(...notifications.map((n) => n.id)) + 1 : 1,
    notifications,
  };
}

// settle lets the store's awaited fetch and its follow-up run to completion.
function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
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
  h.push(snap([notif({ id: 1 })]));
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
  h.push(snap([notif({ id: 1 })]));
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
  h.push(snap([notif({ id: 1 })]));
  h.connect();
  await settle();
  // Ids 2 to 4 were dropped: the gap schedules a re-sync.
  h.live(notif({ id: 5 }));
  const pending = h.timers.pending();
  assert.equal(pending.length, 1);
  // A load that started before the gap may not hold the dropped events, so
  // the re-sync must survive it.
  h.push(snap([notif({ id: 1 })]));
  await h.ns.load();
  assert.deepEqual(h.timers.pending(), pending);
});

test("a gap absorbed into a pending backoff retry survives an applied load", async () => {
  const h = harness();
  h.push(snap([notif({ id: 1 })]));
  h.connect();
  await settle();
  h.push(new Error("offline"));
  h.connect();
  await settle();
  const pending = h.timers.pending(resyncDelay(1));
  assert.equal(pending.length, 1);
  h.live(notif({ id: 5 }));
  h.push(snap([notif({ id: 1 })]));
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
