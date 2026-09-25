// A harness for AppStore with a fake API and event stream, pinning how the
// store wires its refresh helpers (store-core.ts, tested on their own):
// status, devices and system announce only on change and re-announce after a
// failed read; config and available announce on every read; an older response
// never overwrites a newer one; polling pauses while the page is hidden; and
// the event stream stops after the hidden-page grace and restarts on showing.
// Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { AppStore, HIDDEN_STREAM_GRACE_MS, type StoreDeps } from "../src/lib/store.js";
import { FakeTimers } from "./fixtures.js";
import type { ApplianceStatus, Config, Device, SystemInfo } from "../src/lib/types.js";

// Outcome is one queued result for an endpoint: a value to resolve with, an
// Error to reject with, or a promise the test settles itself.
type Outcome = unknown;

interface Harness {
  store: AppStore;
  // push queues the next outcome for an endpoint.
  push(endpoint: keyof StoreDeps["api"], outcome: Outcome): void;
  // events counts the store's announcements by name.
  events: Map<string, number>;
  // calls counts the requests made per endpoint.
  calls: Map<string, number>;
  sseStarts: () => number;
  sseStops: () => number;
  // emit delivers a synthesized event-stream event ("connected", ...) to the
  // store's subscription, as the SSE client would.
  emit: (name: string) => void;
  // connection records the detail of every "connection" event, in order.
  connection: boolean[];
}

const ANNOUNCED = ["status", "devices", "system", "config", "available", "loaderror", "connection"];

// harness builds a store over fakes. With timers given, the store schedules
// through them (see FakeTimers); without, it uses the real globals.
function harness(timers?: FakeTimers): Harness {
  const queues = new Map<string, Outcome[]>();
  const calls = new Map<string, number>();
  const next = (name: string) => (): Promise<never> => {
    calls.set(name, (calls.get(name) ?? 0) + 1);
    const q = queues.get(name) ?? [];
    const o = q.length > 0 ? q.shift() : new Error(`${name}: nothing queued`);
    if (o instanceof Promise) return o as Promise<never>;
    return o instanceof Error ? Promise.reject(o) : Promise.resolve(o as never);
  };
  let starts = 0;
  let stops = 0;
  let handler: ((name: string, data: unknown) => void) | null = null;
  const deps: StoreDeps = {
    api: {
      onUnauthorized: null,
      getHealth: next("getHealth"),
      getStatus: next("getStatus"),
      getDevices: next("getDevices"),
      getSystem: next("getSystem"),
      getConfig: next("getConfig"),
      getAvailableDevices: next("getAvailableDevices"),
    },
    sse: {
      subscribe: (h) => {
        handler = h;
        return () => true;
      },
      start: () => {
        starts++;
      },
      stop: () => {
        stops++;
      },
    },
    timers,
  };
  const store = new AppStore(deps);
  const events = new Map<string, number>();
  for (const name of ANNOUNCED) {
    store.addEventListener(name, () => events.set(name, (events.get(name) ?? 0) + 1));
  }
  const connection: boolean[] = [];
  store.addEventListener("connection", (e: Event) => connection.push((e as CustomEvent<boolean>).detail));
  return {
    sseStops: () => stops,
    emit: (name) => handler?.(name, null),
    connection,
    store,
    push(endpoint, outcome) {
      const q = queues.get(endpoint) ?? [];
      q.push(outcome);
      queues.set(endpoint, q);
    },
    events,
    calls,
    sseStarts: () => starts,
  };
}

function status(uptimeSeconds: number): ApplianceStatus {
  return { uptimeSeconds } as unknown as ApplianceStatus;
}

// deferred returns a promise with its resolve exposed, so a test controls the
// order in which overlapping reads land.
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

test("status announces the first read and then only on change", async () => {
  const h = harness();
  h.push("getStatus", status(1));
  h.push("getStatus", status(1));
  h.push("getStatus", status(2));
  assert.equal(await h.store.refreshStatus(), true);
  assert.equal(await h.store.refreshStatus(), true);
  assert.equal(h.events.get("status"), 1);
  await h.store.refreshStatus();
  assert.equal(h.events.get("status"), 2);
  assert.deepEqual(h.store.getState().status, status(2));
});

test("a failed status read re-announces the next read even when unchanged", async () => {
  const h = harness();
  h.push("getStatus", status(1));
  h.push("getStatus", new Error("offline"));
  h.push("getStatus", status(1));
  await h.store.refreshStatus();
  assert.equal(await h.store.refreshStatus(), false);
  // The failure kept the last good state.
  assert.deepEqual(h.store.getState().status, status(1));
  await h.store.refreshStatus();
  assert.equal(h.events.get("status"), 2);
});

test("devices announce on change, reset on failure, and normalize channels", async () => {
  const h = harness();
  const dev = { name: "mic", channels: "bogus" } as unknown as Device;
  h.push("getDevices", [dev]);
  h.push("getDevices", [{ name: "mic", channels: [] }]);
  h.push("getDevices", new Error("offline"));
  h.push("getDevices", [{ name: "mic", channels: [] }]);
  await h.store.refreshDevices();
  assert.deepEqual(h.store.getState().devices[0].channels, []);
  // The normalized payload equals the next one, so no second announcement.
  await h.store.refreshDevices();
  assert.equal(h.events.get("devices"), 1);
  await h.store.refreshDevices();
  await h.store.refreshDevices();
  assert.equal(h.events.get("devices"), 2);
});

test("system announces on change and re-announces after a failure", async () => {
  const h = harness();
  const sys = { hostname: "pi" } as unknown as SystemInfo;
  h.push("getSystem", sys);
  h.push("getSystem", sys);
  h.push("getSystem", new Error("offline"));
  h.push("getSystem", sys);
  await h.store.refreshSystem();
  await h.store.refreshSystem();
  assert.equal(h.events.get("system"), 1);
  assert.equal(await h.store.refreshSystem(), false);
  await h.store.refreshSystem();
  assert.equal(h.events.get("system"), 2);
});

test("config and available announce on every read", async () => {
  const h = harness();
  const cfg = { devices: [] } as unknown as Config;
  h.push("getConfig", cfg);
  h.push("getConfig", cfg);
  h.push("getAvailableDevices", []);
  h.push("getAvailableDevices", []);
  await h.store.refreshConfig();
  await h.store.refreshConfig();
  await h.store.refreshAvailable();
  await h.store.refreshAvailable();
  assert.equal(h.events.get("config"), 2);
  assert.equal(h.events.get("available"), 2);
});

test("an older status response landing late does not overwrite a newer one", async () => {
  const h = harness();
  const slow = deferred<ApplianceStatus>();
  h.push("getStatus", slow.promise);
  h.push("getStatus", status(2));
  const first = h.store.refreshStatus();
  await h.store.refreshStatus();
  slow.resolve(status(1));
  // Dropped, but fresher data is in place, so it still reports success.
  assert.equal(await first, true);
  assert.deepEqual(h.store.getState().status, status(2));
  assert.equal(h.events.get("status"), 1);
});

test("applyConfig wins over a config read already in flight", async () => {
  const h = harness();
  const slow = deferred<Config>();
  h.push("getConfig", slow.promise);
  const read = h.store.refreshConfig();
  const patched = { devices: [], patched: true } as unknown as Config;
  h.store.applyConfig(patched);
  slow.resolve({ devices: [] } as unknown as Config);
  await read;
  assert.equal(h.store.getState().config, patched);
});

test("polling waits while the page is hidden and refreshes at once on showing", async () => {
  const h = harness();
  for (const ep of ["getStatus", "getDevices", "getSystem", "getConfig", "getAvailableDevices"] as const) {
    h.push(ep, ep === "getStatus" ? status(1) : ep === "getSystem" ? {} : ep === "getConfig" ? { devices: [] } : []);
  }
  h.store.setPageHidden(true);
  h.store.startPolling(60_000);
  try {
    // The event stream starts regardless; no poll request goes out while hidden.
    assert.equal(h.sseStarts(), 1);
    assert.equal(h.calls.get("getStatus"), undefined);
    h.store.setPageHidden(false);
    assert.equal(h.calls.get("getStatus"), 1);
    assert.equal(h.calls.get("getConfig"), 1);
  } finally {
    // Always clear the interval, or a failed assertion leaves the test process
    // waiting on it.
    h.store.stopPolling();
  }
  // Hiding and showing again while stopped does nothing.
  h.store.setPageHidden(true);
  h.store.setPageHidden(false);
  assert.equal(h.calls.get("getStatus"), 1);
});

// pollable queues one successful read for every polled endpoint.
function pollable(h: Harness): void {
  for (const ep of ["getStatus", "getDevices", "getSystem", "getConfig", "getAvailableDevices"] as const) {
    h.push(ep, ep === "getStatus" ? status(1) : ep === "getSystem" ? {} : ep === "getConfig" ? { devices: [] } : []);
  }
}

test("hiding the page while polling stops the poll timer, and showing it re-arms one", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  pollable(h);
  h.store.startPolling(3000);
  assert.equal(timers.intervals().length, 1);
  h.store.setPageHidden(true);
  assert.equal(timers.intervals().length, 0);
  h.store.setPageHidden(false);
  assert.equal(timers.intervals().length, 1);
  h.store.stopPolling();
  assert.equal(timers.intervals().length, 0);
});

test("a hidden page stops the stream after the grace, and showing it restarts the stream", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  pollable(h);
  h.store.startPolling(3000);
  h.emit("connected");
  h.store.setPageHidden(true);
  // The stream stays up through the grace.
  assert.equal(h.sseStops(), 0);
  const [stop] = timers.pending(HIDDEN_STREAM_GRACE_MS);
  assert.ok(stop, "no stop timer armed on hiding");
  timers.fire(stop);
  assert.equal(h.sseStops(), 1);
  // The stop is reported as the stream going down, once.
  assert.deepEqual(h.connection, [true, false]);
  assert.equal(h.store.getState().connected, false);
  // A token swap ending while the page is still hidden must not restart it.
  h.store.beginTokenSwap();
  h.store.endTokenSwap();
  assert.equal(h.sseStarts(), 1);
  h.store.setPageHidden(false);
  assert.equal(h.sseStarts(), 2);
  h.store.stopPolling();
});

test("a quick hide and show keeps the stream and leaves no stop timer behind", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  pollable(h);
  h.store.startPolling(3000);
  h.store.setPageHidden(true);
  assert.equal(timers.pending(HIDDEN_STREAM_GRACE_MS).length, 1);
  h.store.setPageHidden(false);
  // The grace was cancelled, so a later hide starts a fresh one rather than
  // inheriting the first hide's deadline.
  assert.equal(timers.pending(HIDDEN_STREAM_GRACE_MS).length, 0);
  assert.equal(h.sseStops(), 0);
  assert.equal(h.sseStarts(), 1);
  h.store.stopPolling();
});

test("stopping polling while hidden cancels the stream stop timer", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  h.store.setPageHidden(true);
  h.store.startPolling(3000);
  assert.equal(timers.pending(HIDDEN_STREAM_GRACE_MS).length, 1);
  // A 401 or a logout stops polling (and the stream) on its own terms.
  h.store.stopPolling();
  assert.equal(timers.pending(HIDDEN_STREAM_GRACE_MS).length, 0);
  assert.equal(h.sseStops(), 1);
});
