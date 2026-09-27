// Unit tests for the SSE client's event filter (lib/sse.ts): the filter goes
// in the stream URL, changing it reconnects a running stream without
// reporting a disconnect, and it never starts a stopped one. A fake fetch
// stands in for the network and FakeTimers for the heartbeat and backoff.
// Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { HEARTBEAT_TIMEOUT_MS, RECONNECT_DELAY_MS, SSEClient } from "../src/lib/sse.ts";
import { FakeTimers, settle } from "./fixtures.ts";

// Call is one fetch the client made: its URL, and hooks to answer it.
interface Call {
  url: string;
  // stream answers 200 with a body that stays open until the request aborts.
  stream(): void;
  // reply answers with a bare status.
  reply(status: number): void;
}

function harness() {
  const calls: Call[] = [];
  const timers = new FakeTimers();
  const client = new SSEClient("/api/v1/events", {
    fetch: (url, init) =>
      new Promise<Response>((resolve, reject) => {
        const signal = init.signal;
        const abortError = () => new DOMException("aborted", "AbortError");
        signal?.addEventListener("abort", () => reject(abortError()));
        calls.push({
          url,
          stream: () => {
            const body = new ReadableStream<Uint8Array>({
              start(controller) {
                // A real fetch errors the body when its request aborts.
                signal?.addEventListener("abort", () => controller.error(abortError()));
              },
            });
            resolve(new Response(body, { status: 200 }));
          },
          reply: (status) => resolve(new Response(null, { status })),
        });
      }),
    timers,
  });
  const events: string[] = [];
  client.subscribe((name) => events.push(name));
  return { client, calls, events, timers };
}

test("the event filter goes in the stream URL", async () => {
  const h = harness();
  h.client.setEvents(["notification"]);
  h.client.start();
  assert.equal(h.calls.at(-1)?.url, "/api/v1/events?events=notification");
  h.client.setEvents(["levels", "notification"]);
  await settle();
  assert.equal(h.calls.at(-1)?.url, "/api/v1/events?events=levels%2Cnotification");
  h.client.setEvents(null);
  await settle();
  assert.equal(h.calls.at(-1)?.url, "/api/v1/events", "no filter means every event type");
  h.client.stop();
});

test("setting the same filter does not reconnect", async () => {
  const h = harness();
  h.client.start();
  h.client.setEvents(["notification"]);
  await settle();
  assert.equal(h.calls.length, 2);
  h.client.setEvents(["notification"]);
  await settle();
  assert.equal(h.calls.length, 2, "an unchanged filter must not reconnect");
  h.client.stop();
});

test("changing the filter mid-connect reconnects without a disconnected event", async () => {
  const h = harness();
  h.client.start();
  // The first connect is still waiting for its response.
  h.client.setEvents(["notification"]);
  await settle();
  assert.equal(h.calls.length, 2);
  assert.deepEqual(h.events, [], "a deliberate reconnect must not report a disconnect");
  h.calls.at(-1)?.stream();
  await settle();
  assert.deepEqual(h.events, ["connected"]);
  h.client.stop();
});

test("changing the filter while streaming reconnects once, without a disconnected event", async () => {
  const h = harness();
  h.client.start();
  h.calls.at(-1)?.stream();
  await settle();
  assert.deepEqual(h.events, ["connected"]);
  h.client.setEvents(["notification"]);
  await settle();
  assert.equal(h.calls.length, 2);
  h.calls.at(-1)?.stream();
  await settle();
  assert.deepEqual(h.events, ["connected", "connected"], "the new stream connects once, with no disconnect between");
  h.client.stop();
});

test("changing the filter while stopped does not start the stream", async () => {
  const h = harness();
  h.client.setEvents(["notification"]);
  assert.equal(h.calls.length, 0, "setEvents before start must not connect");
  h.client.start();
  // The appliance rejects the token, which stops the stream.
  h.calls.at(-1)?.reply(401);
  await settle();
  assert.deepEqual(h.events, ["unauthorized"]);
  h.client.setEvents(null);
  await settle();
  assert.equal(h.calls.length, 1, "a filter change must not restart a stream stopped by a 401");
});

test("an empty filter list means every event type", async () => {
  const h = harness();
  h.client.setEvents([]);
  h.client.start();
  assert.equal(h.calls.at(-1)?.url, "/api/v1/events");
  h.client.setEvents(null);
  await settle();
  assert.equal(h.calls.length, 1, "an empty list and null are the same filter");
  h.client.stop();
});

test("a restart while a 401 is on its way does not stop the new stream", async () => {
  const h = harness();
  h.client.start();
  // The first request is answered 401, but before its continuation runs the
  // filter changes and the stream restarts.
  h.calls.at(-1)?.reply(401);
  h.client.setEvents(["notification"]);
  await settle();
  assert.deepEqual(h.events, [], "the stale 401 must not reach the store");
  assert.equal(h.calls.length, 2);
  h.calls.at(-1)?.stream();
  await settle();
  assert.deepEqual(h.events, ["connected"], "the new stream must still connect");
  h.client.stop();
});

test("the heartbeat watchdog and the reconnect backoff use the injected timers", async () => {
  const h = harness();
  h.client.setEvents(["notification"]);
  h.client.start();
  h.calls.at(-1)?.stream();
  await settle();
  const [watchdog] = h.timers.pending(HEARTBEAT_TIMEOUT_MS);
  assert.ok(watchdog, "a connected stream must arm the heartbeat watchdog");
  // The watchdog firing aborts the silent stream, which then backs off.
  h.timers.fire(watchdog);
  await settle();
  assert.deepEqual(h.events, ["connected", "disconnected"]);
  const [backoff] = h.timers.pending(RECONNECT_DELAY_MS);
  assert.ok(backoff, "a dropped stream must wait out the backoff");
  h.timers.fire(backoff);
  await settle();
  assert.equal(h.calls.length, 2);
  assert.equal(h.calls.at(-1)?.url, "/api/v1/events?events=notification", "the reconnect keeps the filter");
  h.client.stop();
});

test("a filter restart arms the heartbeat watchdog before the new stream answers", async () => {
  const h = harness();
  h.client.start();
  h.calls.at(-1)?.stream();
  await settle();
  h.client.setEvents(["notification"]);
  await settle();
  // The new request hangs (a dead link); the watchdog must still be armed.
  const [watchdog] = h.timers.pending(HEARTBEAT_TIMEOUT_MS);
  assert.ok(watchdog, "a silent restart must arm the watchdog");
  h.timers.fire(watchdog);
  await settle();
  assert.deepEqual(h.events, ["connected", "disconnected"], "the hung restart must be reported once the watchdog fires");
  h.client.stop();
});
