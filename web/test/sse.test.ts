// Unit tests for the SSE client (lib/sse.ts): the event filter goes in the
// stream URL, changing it reconnects a running stream without reporting a
// disconnect, and it never starts a stopped one; the heartbeat watchdog and
// the reconnect backoff, including a stream the server ends. A fake fetch
// stands in for the network and FakeTimers for the heartbeat and backoff.
// Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { HEARTBEAT_TIMEOUT_MS, RECONNECT_DELAY_MS, SSEClient } from "../src/lib/sse.ts";
import { at, FakeTimers, settle } from "./fixtures.ts";

// Call is one fetch the client made: its URL, and hooks to answer it.
interface Call {
  url: string;
  // stream answers 200 with a body that stays open until the request aborts.
  // Its send writes to the body, and end closes it as a server ending the
  // stream would.
  stream(): { send(text: string): void; end(): void };
  // reply answers with a bare status.
  reply(status: number): void;
}

function harness() {
  const calls: Call[] = [];
  const timers = new FakeTimers();
  let clock = 0;
  const client = new SSEClient("/api/v1/events", {
    fetch: (url, init) =>
      new Promise<Response>((resolve, reject) => {
        const signal = init.signal;
        const abortError = () => new DOMException("aborted", "AbortError");
        signal?.addEventListener("abort", () => reject(abortError()));
        calls.push({
          url,
          stream: () => {
            let ctl: ReadableStreamDefaultController<Uint8Array> | undefined;
            const body = new ReadableStream<Uint8Array>({
              start(controller) {
                ctl = controller;
                // A real fetch errors the body when its request aborts.
                signal?.addEventListener("abort", () => controller.error(abortError()));
              },
            });
            resolve(new Response(body, { status: 200 }));
            return {
              send: (text) => ctl?.enqueue(new TextEncoder().encode(text)),
              end: () => ctl?.close(),
            };
          },
          reply: (status) => resolve(new Response(null, { status })),
        });
      }),
    timers,
    now: () => clock,
  });
  const events: string[] = [];
  const payloads: [string, unknown][] = [];
  client.subscribe((name, data) => {
    events.push(name);
    payloads.push([name, data]);
  });
  return {
    client,
    calls,
    events,
    payloads,
    timers,
    // last is the newest request the client made.
    last: () => at(calls, calls.length - 1),
    setClock: (t: number) => {
      clock = t;
    },
  };
}

test("the event filter goes in the stream URL", async () => {
  const h = harness();
  h.client.setEvents(["notification"]);
  h.client.start();
  assert.equal(h.last().url, "/api/v1/events?events=notification");
  h.client.setEvents(["levels", "notification"]);
  await settle();
  assert.equal(h.last().url, "/api/v1/events?events=levels,notification");
  h.client.setEvents(null);
  await settle();
  assert.equal(h.last().url, "/api/v1/events", "no filter means every event type");
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
  h.last().stream();
  await settle();
  assert.deepEqual(h.events, ["connected"]);
  h.client.stop();
});

test("changing the filter while streaming reconnects once, without a disconnected event", async () => {
  const h = harness();
  h.client.start();
  h.last().stream();
  await settle();
  assert.deepEqual(h.events, ["connected"]);
  h.client.setEvents(["notification"]);
  await settle();
  assert.equal(h.calls.length, 2);
  h.last().stream();
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
  h.last().reply(401);
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
  assert.equal(h.last().url, "/api/v1/events");
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
  h.last().reply(401);
  h.client.setEvents(["notification"]);
  await settle();
  assert.deepEqual(h.events, [], "the stale 401 must not reach the store");
  assert.equal(h.calls.length, 2);
  h.last().stream();
  await settle();
  assert.deepEqual(h.events, ["connected"], "the new stream must still connect");
  h.client.stop();
});

test("the heartbeat watchdog and the reconnect backoff use the injected timers", async () => {
  const h = harness();
  h.client.setEvents(["notification"]);
  h.client.start();
  h.last().stream();
  await settle();
  const [watchdog] = h.timers.pending(HEARTBEAT_TIMEOUT_MS);
  assert.ok(watchdog, "a connected stream must arm the heartbeat watchdog");
  // The watchdog firing on a stream silent for the whole window aborts it,
  // and it then backs off.
  h.setClock(HEARTBEAT_TIMEOUT_MS);
  h.timers.fire(watchdog);
  await settle();
  assert.deepEqual(h.events, ["connected", "disconnected"]);
  const [backoff] = h.timers.pending(RECONNECT_DELAY_MS);
  assert.ok(backoff, "a dropped stream must wait out the backoff");
  h.timers.fire(backoff);
  await settle();
  assert.equal(h.calls.length, 2);
  assert.equal(h.last().url, "/api/v1/events?events=notification", "the reconnect keeps the filter");
  h.client.stop();
});

test("a filter restart arms the heartbeat watchdog before the new stream answers", async () => {
  const h = harness();
  h.client.start();
  h.last().stream();
  await settle();
  h.client.setEvents(["notification"]);
  await settle();
  // The new request hangs (a dead link); the watchdog must still be armed.
  const [watchdog] = h.timers.pending(HEARTBEAT_TIMEOUT_MS);
  assert.ok(watchdog, "a silent restart must arm the watchdog");
  h.setClock(HEARTBEAT_TIMEOUT_MS);
  h.timers.fire(watchdog);
  await settle();
  assert.deepEqual(h.events, ["connected", "disconnected"], "the hung restart must be reported once the watchdog fires");
  h.client.stop();
});

test("a 401 on a filter restart clears the watchdog the restart armed", async () => {
  const h = harness();
  h.client.start();
  h.last().stream();
  await settle();
  h.client.setEvents(["notification"]);
  await settle();
  assert.equal(h.timers.pending(HEARTBEAT_TIMEOUT_MS).length, 1);
  h.last().reply(401);
  await settle();
  assert.deepEqual(h.events, ["connected", "unauthorized"]);
  assert.deepEqual(h.timers.pending(), [], "a stream stopped by a 401 must leave no watchdog to fire later");
});

test("a stream the server ends is reported down and reconnects", async () => {
  const h = harness();
  h.client.start();
  const body = h.last().stream();
  await settle();
  body?.end();
  await settle();
  assert.deepEqual(h.events, ["connected", "disconnected"], "listeners must learn the stream is down");
  const [backoff] = h.timers.pending(RECONNECT_DELAY_MS);
  assert.ok(backoff, "an ended stream must wait out the backoff");
  h.timers.fire(backoff);
  await settle();
  assert.equal(h.calls.length, 2);
  h.client.stop();
});

test("a stream that ends before any data backs off further; data resets the backoff", async () => {
  const h = harness();
  h.client.start();
  // Answered and closed at once, twice: the second wait is longer.
  for (const wait of [RECONNECT_DELAY_MS, RECONNECT_DELAY_MS * 2]) {
    h.last().stream().end();
    await settle();
    const [backoff] = h.timers.pending(wait);
    assert.ok(backoff, `an empty stream must wait ${wait} ms before reconnecting`);
    h.timers.fire(backoff);
    await settle();
  }
  // This stream carries a heartbeat before it ends, so the next wait is the
  // shortest again.
  const body = h.last().stream();
  await settle();
  body?.send("event: heartbeat\ndata: {}\n\n");
  await settle();
  body?.end();
  await settle();
  assert.equal(h.timers.pending(RECONNECT_DELAY_MS).length, 1, "a stream that carried data must reset the backoff");
  h.client.stop();
});

test("data inside the window keeps the stream and costs no timer calls", async () => {
  const h = harness();
  h.client.start();
  const body = h.last().stream();
  await settle();
  const armed = h.timers.all.length;
  for (let t = 100; t <= 1000; t += 100) {
    h.setClock(t);
    body.send(`event: levels\ndata: {"t":${t}}\n\n`);
    await settle();
  }
  assert.equal(h.timers.all.length, armed, "a message must not clear and set a timer");
  // At the end of the window the stream was last heard from 1 s ago: the
  // watchdog re-arms for what is left instead of dropping the stream.
  h.setClock(HEARTBEAT_TIMEOUT_MS);
  h.timers.fire(at(h.timers.pending(HEARTBEAT_TIMEOUT_MS), 0));
  await settle();
  assert.deepEqual(h.events.filter((e) => e !== "levels"), ["connected"]);
  assert.equal(h.timers.pending(1000).length, 1, "the watchdog must re-arm for the rest of the window");
  h.client.stop();
});

test("a named data frame is dispatched by name; a reserved name or bad JSON is not", async () => {
  const h = harness();
  h.client.start();
  const body = h.last().stream();
  await settle();
  const warn = console.warn;
  const error = console.error;
  console.warn = () => {};
  console.error = () => {};
  try {
    body.send('event: levels\ndata: {"a":1}\n\n');
    body.send('event: connected\ndata: {"b":2}\n\n');
    body.send("event: notification\ndata: {not json\n\n");
    body.send('event: notification\ndata: {"c":3}\n\n');
    await settle();
  } finally {
    console.warn = warn;
    console.error = error;
  }
  assert.deepEqual(h.payloads.slice(1), [
    ["levels", { a: 1 }],
    ["notification", { c: 3 }],
  ]);
  h.client.stop();
});

test("stopping a stream reports no disconnect and leaves no timer", async () => {
  const h = harness();
  h.client.start();
  h.last().stream();
  await settle();
  h.client.stop();
  await settle();
  assert.deepEqual(h.events, ["connected"]);
  assert.deepEqual(h.timers.pending(), [], "no watchdog or backoff after a stop");
});

test("an open comment alone does not reset the backoff", async () => {
  const h = harness();
  h.client.start();
  // Answered with the open comment and closed, twice: the second wait is
  // longer, as for a stream that sent nothing.
  for (const wait of [RECONNECT_DELAY_MS, RECONNECT_DELAY_MS * 2]) {
    const body = h.last().stream();
    await settle();
    body.send(": open\n\n");
    await settle();
    body.end();
    await settle();
    const [backoff] = h.timers.pending(wait);
    assert.ok(backoff, `a stream with only a comment must wait ${wait} ms`);
    h.timers.fire(backoff);
    await settle();
  }
  h.client.stop();
});

test("a handler that stops the stream ends the chunk's dispatch", async () => {
  const h = harness();
  h.client.subscribe((name) => {
    if (name === "levels") h.client.stop();
  });
  h.client.start();
  const body = h.last().stream();
  await settle();
  body.send('event: levels\ndata: {}\n\nevent: notification\ndata: {}\n\n');
  await settle();
  assert.deepEqual(h.events, ["connected", "levels"], "nothing after the stop may be dispatched");
  assert.deepEqual(h.timers.pending(), [], "no watchdog armed on a stopped client");
});
