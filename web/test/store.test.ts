// A harness for AppStore with a fake API and event stream, pinning how the
// store wires its refresh helpers (store-core.ts, tested on their own):
// status, devices and system announce only on change and re-announce after a
// failed read; config and available announce on every read; an older response
// never overwrites a newer one; and polling pauses while the page is hidden.
// Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { AppStore, type StoreDeps } from "../src/lib/store.js";
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
}

const ANNOUNCED = ["status", "devices", "system", "config", "available", "loaderror", "connection"];

function harness(): Harness {
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
      subscribe: () => () => true,
      start: () => {
        starts++;
      },
      stop: () => {},
    },
  };
  const store = new AppStore(deps);
  const events = new Map<string, number>();
  for (const name of ANNOUNCED) {
    store.addEventListener(name, () => events.set(name, (events.get(name) ?? 0) + 1));
  }
  return {
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
