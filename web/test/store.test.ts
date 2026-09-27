// A harness for AppStore with a fake API and event stream, pinning how the
// store wires its refresh helpers (store-core.ts, tested on their own):
// status, devices, system and available announce only on change; status,
// devices and system re-announce after a failed read that nothing newer
// superseded; config announces on every read; a failed
// initial load announces which views' data is missing; an older response
// never overwrites a newer one; polling pauses while the page is hidden; and
// the event stream stops after the hidden-page grace and restarts on showing.
// Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { AppStore, HIDDEN_STREAM_GRACE_MS, type StoreDeps, type StoreEvents, LEVELS_GRACE_MS, NON_LEVEL_EVENTS } from "../src/lib/store.ts";
import { ApiError, UnreadableResponseError } from "../src/lib/api.ts";
import { getToken, setToken } from "../src/lib/auth.ts";
import { at, deferred, FakeStream, FakeTimers, settle } from "./fixtures.ts";
import type { AvailableDevice, ApplianceStatus, Config, Device, LoadError, SystemInfo, UpdateStatus } from "../src/lib/types.ts";

// Outcome is one queued result for an endpoint: a value to resolve with, an
// Error to reject with, or a promise the test settles itself.
type Outcome = unknown;

interface Harness {
  store: AppStore;
  // push queues the next outcome for an endpoint.
  push(endpoint: keyof StoreDeps["api"], outcome: Outcome): void;
  // events counts the store's announcements by name.
  events: Map<string, number>;
  // last holds the payload of each name's latest announcement.
  last: Map<string, unknown>;
  // calls counts the requests made per endpoint.
  calls: Map<string, number>;
  sseStarts: () => number;
  sseStops: () => number;
  // emit delivers a synthesized event-stream event ("connected", ...) to the
  // store's subscription, as the SSE client would.
  emit: (name: string, data?: unknown) => void;
  // connection records the detail of every "connection" event, in order.
  connection: boolean[];
  // filters records every event filter the store set on the stream.
  filters: (readonly string[] | null)[];
  // unauthorized reports a 401 to the store, as the API client would.
  unauthorized: () => void;
}

const ANNOUNCED: (keyof StoreEvents)[] = ["status", "devices", "system", "config", "available", "loaderror", "connection"];

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
  const stream = new FakeStream();
  const filters: (readonly string[] | null)[] = [];
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
      subscribe: stream.subscribe,
      start: () => {
        starts++;
      },
      stop: () => {
        stops++;
      },
      setEvents: (names) => {
        filters.push(names);
      },
    },
    timers,
  };
  const store = new AppStore(deps);
  const events = new Map<string, number>();
  const last = new Map<string, unknown>();
  for (const name of ANNOUNCED) {
    store.on(name, (payload) => {
      events.set(name, (events.get(name) ?? 0) + 1);
      last.set(name, payload);
    });
  }
  const connection: boolean[] = [];
  store.on("connection", (up) => connection.push(up));
  return {
    unauthorized: () => deps.api.onUnauthorized?.(),
    filters,
    sseStops: () => stops,
    emit: (name, data = null) => stream.deliver(name, data),
    connection,
    store,
    push(endpoint, outcome) {
      const q = queues.get(endpoint) ?? [];
      q.push(outcome);
      queues.set(endpoint, q);
    },
    events,
    last,
    calls,
    sseStarts: () => starts,
  };
}

function status(uptimeSeconds: number): ApplianceStatus {
  return { uptimeSeconds } as unknown as ApplianceStatus;
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
  assert.equal(h.last.get("status"), h.store.getState().status, "the announcement carries the applied status");
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
  assert.deepEqual(at(h.store.getState().devices, 0).channels, []);
  // The normalized payload equals the next one, so no second announcement.
  await h.store.refreshDevices();
  assert.equal(h.events.get("devices"), 1);
  await h.store.refreshDevices();
  await h.store.refreshDevices();
  assert.equal(h.events.get("devices"), 2);
  assert.equal(h.last.get("devices"), h.store.getState().devices);
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
  assert.equal(h.last.get("system"), sys);
});

test("config announces on every read", async () => {
  const h = harness();
  const cfg = { devices: [] } as unknown as Config;
  h.push("getConfig", cfg);
  h.push("getConfig", cfg);
  await h.store.refreshConfig();
  await h.store.refreshConfig();
  assert.equal(h.events.get("config"), 2);
  assert.equal(h.last.get("config"), cfg);
});

test("available announces only on change, even after a failure", async () => {
  const h = harness();
  const one = [{ device: "hw:1,0" }] as unknown as AvailableDevice[];
  h.push("getAvailableDevices", []);
  h.push("getAvailableDevices", []);
  h.push("getAvailableDevices", one);
  h.push("getAvailableDevices", new Error("offline"));
  h.push("getAvailableDevices", one);
  await h.store.refreshAvailable();
  await h.store.refreshAvailable();
  assert.equal(h.events.get("available"), 1, "an unchanged list must not announce");
  await h.store.refreshAvailable();
  assert.equal(h.events.get("available"), 2);
  assert.equal(h.last.get("available"), h.store.getState().available);
  assert.equal(await h.store.refreshAvailable(), false);
  await h.store.refreshAvailable();
  // No view swaps the list for a load error, so an unchanged list after a
  // failure has nothing to repair and must not announce.
  assert.equal(h.events.get("available"), 2, "an unchanged list after a failure must not announce");
});

test("a failure after a newer read applied does not re-announce", async () => {
  // The same guard in each refresh that resets its tracker on a failure.
  const cases = [
    { name: "status", endpoint: "getStatus", body: status(1), refresh: (h: Harness) => h.store.refreshStatus() },
    { name: "devices", endpoint: "getDevices", body: [{ name: "mic", channels: [] }], refresh: (h: Harness) => h.store.refreshDevices() },
    { name: "system", endpoint: "getSystem", body: { hostname: "pi" }, refresh: (h: Harness) => h.store.refreshSystem() },
  ] as const;
  for (const c of cases) {
    const h = harness();
    const slow = deferred<unknown>();
    h.push(c.endpoint, slow.promise);
    h.push(c.endpoint, c.body);
    h.push(c.endpoint, c.body);
    const older = c.refresh(h);
    await c.refresh(h);
    assert.equal(h.events.get(c.name), 1, `${c.name}: the newer read announces`);
    // The older read fails after the newer one applied: fresh data is in place.
    slow.reject(new Error("offline"));
    assert.equal(await older, true);
    await c.refresh(h);
    assert.equal(h.events.get(c.name), 1, `${c.name}: a superseded failure must not re-arm the announcement`);
  }
});

test("a login that cannot reach the appliance never shows a response body", async () => {
  const h = harness(new FakeTimers());
  // A non-problem error with a body in its detail, which request() no longer
  // keeps: the message must show the status text alone either way.
  h.push("getStatus", new ApiError(502, "Bad Gateway", { detail: "<html><body>upstream down</body></html>" }));
  try {
    const res = await h.store.login("typed-token");
    assert.equal(res.ok, false);
    assert.equal(res.message, "Could not reach the appliance. Check the connection and try again.");
  } finally {
    setToken(null);
  }
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
  assert.equal(h.last.get("config"), patched, "the applied config is announced");
});

test("applyUpdateStatus wins over a system read already in flight and announces", async () => {
  const h = harness();
  h.push("getSystem", { hostname: "pi" } as unknown as SystemInfo);
  await h.store.refreshSystem();
  const slow = deferred<SystemInfo>();
  h.push("getSystem", slow.promise);
  const read = h.store.refreshSystem();
  const update = { phase: "downloading" } as unknown as UpdateStatus;
  h.store.applyUpdateStatus(update);
  assert.equal(h.events.get("system"), 2, "the applied state announces");
  const announced = h.last.get("system") as SystemInfo;
  assert.equal(announced.update, update, "the announcement carries the update");
  assert.equal(announced, h.store.getState().system);
  slow.resolve({ hostname: "pi", update: { phase: "idle" } } as unknown as SystemInfo);
  await read;
  assert.equal(h.store.getState().system?.update, update, "the older read is dropped");
  assert.equal(h.store.getState().system?.hostname, "pi", "the rest of the snapshot is kept");
  assert.equal(h.events.get("system"), 2);
});

test("applyUpdateStatus announces only a change", async () => {
  const h = harness();
  h.push("getSystem", { hostname: "pi" } as unknown as SystemInfo);
  await h.store.refreshSystem();
  const update = { phase: "downloading" } as unknown as UpdateStatus;
  h.store.applyUpdateStatus(update);
  h.store.applyUpdateStatus({ ...update });
  assert.equal(h.events.get("system"), 2, "the first read and one change");
});

test("applyUpdateStatus without a system snapshot still drops the older read", async () => {
  const h = harness();
  const slow = deferred<SystemInfo>();
  h.push("getSystem", slow.promise);
  const read = h.store.refreshSystem();
  h.store.applyUpdateStatus({ phase: "downloading" } as unknown as UpdateStatus);
  slow.resolve({ hostname: "stale" } as unknown as SystemInfo);
  await read;
  assert.equal(h.store.getState().system, null);
  assert.equal(h.events.get("system") ?? 0, 0);
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

// pauseStream starts polling, hides the page, and lets the grace run out, so
// the stream is stopped for the hidden page.
function pauseStream(timers: FakeTimers, h: Harness): void {
  pollable(h);
  h.store.startPolling(3000);
  h.emit("connected");
  h.store.setPageHidden(true);
  const [stop] = timers.pending(HIDDEN_STREAM_GRACE_MS);
  assert.ok(stop, "no stop timer armed on hiding");
  timers.fire(stop);
  assert.equal(h.sseStops(), 1);
}

test("startPolling again while hidden restarts the stream with a fresh grace", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  pauseStream(timers, h);
  // A login or restart calls startPolling while polling is already on.
  h.store.startPolling(3000);
  assert.equal(h.sseStarts(), 2);
  // The page is still hidden, so the restarted stream must stop again after a
  // grace rather than run for a tab nobody is looking at.
  const [stop] = timers.pending(HIDDEN_STREAM_GRACE_MS);
  assert.ok(stop, "no fresh grace armed for the restarted stream");
  timers.fire(stop);
  assert.equal(h.sseStops(), 2);
  h.store.stopPolling();
});

test("startPolling again ends a hidden-page pause", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  pauseStream(timers, h);
  h.store.startPolling(3000);
  assert.equal(h.sseStarts(), 2);
  // The stream is meant to be up again, so a token swap ending restarts it
  // (its old-token connection may have been dropped during the swap).
  h.store.beginTokenSwap();
  h.store.endTokenSwap();
  assert.equal(h.sseStarts(), 3);
  h.store.stopPolling();
});

test("stopping polling reports the stream down", () => {
  const h = harness(new FakeTimers());
  h.store.startPolling(60_000);
  h.emit("connected");
  assert.equal(h.store.getState().connected, true);
  h.store.stopPolling();
  assert.equal(h.store.getState().connected, false);
  assert.deepEqual(h.connection, [true, false]);
});

test("a login whose token is refused says so and keeps no token", async () => {
  const h = harness(new FakeTimers());
  h.push("getStatus", new ApiError(401, "unauthorized"));
  const res = await h.store.login("typed-token");
  assert.equal(res.ok, false);
  assert.equal(res.message, "The access token was not accepted. Check it and try again.");
  assert.equal(getToken(), null);
});

test("a login the appliance refuses, or answers unreadably, says which", async () => {
  for (const [outcome, want] of [
    [new ApiError(503, "unavailable", { detail: "still starting", problem: true }), "The appliance refused the sign-in: still starting."],
    [new UnreadableResponseError(200), "The appliance answered, but its reply could not be read. Try again."],
    [new TypeError("Failed to fetch"), "Could not reach the appliance. Check the connection and try again."],
  ] as const) {
    const h = harness(new FakeTimers());
    h.push("getStatus", outcome);
    const res = await h.store.login("typed-token");
    assert.equal(res.ok, false);
    assert.equal(res.message, want);
    assert.equal(getToken(), null, "a failed login keeps no token");
  }
});

test("a login whose token is refused during the load says so", async () => {
  const h = harness(new FakeTimers());
  const devices = deferred<Device[]>();
  h.push("getStatus", status(1));
  h.push("getDevices", devices.promise);
  h.push("getSystem", {});
  h.push("getConfig", { devices: [] });
  h.push("getAvailableDevices", []);
  try {
    const login = h.store.login("typed-token");
    await settle();
    // The token is revoked while the load is in flight.
    h.unauthorized();
    devices.resolve([]);
    const res = await login;
    assert.equal(res.ok, false);
    assert.equal(res.message, "The access token was not accepted while loading. Try again.");
    assert.equal(getToken(), null);
  } finally {
    h.store.stopPolling();
    setToken(null);
  }
});

test("a login fetches /status once and applies the verifying read", async () => {
  const h = harness(new FakeTimers());
  try {
    // Only ONE status is queued, as in the boot test below.
    pollable(h);
    const res = await h.store.login("typed-token");
    assert.equal(res.ok, true);
    assert.equal(h.calls.get("getStatus"), 1);
    assert.deepEqual(h.store.getState().status, status(1));
  } finally {
    h.store.stopPolling();
    setToken(null);
  }
});

test("a token-gated boot fetches /status once and applies the verifying read", async () => {
  const h = harness(new FakeTimers());
  setToken("stored-token");
  try {
    h.push("getHealth", { status: "ok", authRequired: true });
    // Only ONE status is queued: a second GET /status would reject with
    // "nothing queued" and fail the core load.
    pollable(h);
    let loadError = false;
    h.store.on("loaderror", () => {
      loadError = true;
    });
    assert.equal(await h.store.start(), true);
    // loadInitial runs detached from start(); let its refreshes settle.
    await settle();
    assert.equal(h.calls.get("getStatus"), 1);
    assert.deepEqual(h.store.getState().status, status(1));
    assert.equal(h.events.get("status"), 1);
    assert.equal(loadError, false);
  } finally {
    h.store.stopPolling();
    setToken(null);
  }
});

// ENDPOINTS are the reads loadInitial makes, with a successful body for each.
const ENDPOINTS = {
  getStatus: status(1),
  getDevices: [],
  getSystem: {},
  getConfig: { devices: [] },
  getAvailableDevices: [],
} as const;

// loadFailing runs loadInitial with the named reads failing and the others
// succeeding, and returns every loaderror it announced.
async function loadFailing(h: Harness, failing: (keyof typeof ENDPOINTS)[]): Promise<LoadError[]> {
  for (const [ep, body] of Object.entries(ENDPOINTS) as [keyof typeof ENDPOINTS, unknown][]) {
    h.push(ep, failing.includes(ep) ? new Error("offline") : body);
  }
  const errors: LoadError[] = [];
  h.store.on("loaderror", (e) => errors.push(e));
  await h.store.loadInitial();
  return errors;
}

test("a failed initial load announces which views' data is missing", async () => {
  const message = "Could not reach the appliance.";
  const cases: { failing: (keyof typeof ENDPOINTS)[]; want: LoadError | null }[] = [
    { failing: [], want: null },
    // The dashboard needs status or devices; one of them alone is enough.
    { failing: ["getStatus"], want: null },
    { failing: ["getDevices"], want: null },
    { failing: ["getStatus", "getDevices"], want: { coreFailed: true, systemFailed: false, configFailed: false, availableFailed: false, message } },
    { failing: ["getSystem"], want: { coreFailed: false, systemFailed: true, configFailed: false, availableFailed: false, message } },
    { failing: ["getConfig"], want: { coreFailed: false, systemFailed: false, configFailed: true, availableFailed: false, message } },
    // The available list is advisory: its failure alone raises nothing, and it
    // rides along in the detail when something else failed.
    { failing: ["getAvailableDevices"], want: null },
    { failing: ["getSystem", "getAvailableDevices"], want: { coreFailed: false, systemFailed: true, configFailed: false, availableFailed: true, message } },
  ];
  for (const c of cases) {
    const got = await loadFailing(harness(), c.failing);
    assert.deepEqual(got, c.want ? [c.want] : [], `failing ${c.failing.join(", ") || "nothing"}`);
  }
});

test("a 401 during the initial load leaves the failure to the login prompt", async () => {
  const h = harness();
  const statusRead = deferred<ApplianceStatus>();
  h.push("getStatus", statusRead.promise);
  for (const ep of ["getDevices", "getSystem", "getConfig"] as const) h.push(ep, new Error("offline"));
  h.push("getAvailableDevices", []);
  const errors: LoadError[] = [];
  h.store.on("loaderror", (e) => errors.push(e));
  const load = h.store.loadInitial();
  // The reads are in flight when one is rejected with a 401, which raises the
  // login prompt; the load then finishes with every core read failed.
  h.unauthorized();
  statusRead.reject(new Error("401"));
  await load;
  assert.deepEqual(errors, [], "a pending login must suppress the load error");
});

test("levels drop LEVELS_GRACE_MS after leaving the dashboard", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  // The start-up route is the dashboard.
  h.store.setLevelsWanted(true);
  let dropped = 0;
  h.store.on("levelsdropped", () => dropped++);
  h.emit("levels", { devices: [{ name: "mic", channels: [] }] });
  assert.equal(h.store.getState().levels.size, 1);
  h.store.setLevelsWanted(false);
  assert.deepEqual(h.filters, [], "nothing changes before the grace ends");
  const [grace] = timers.pending(LEVELS_GRACE_MS);
  assert.ok(grace, "leaving the dashboard must arm the grace");
  timers.fire(grace);
  assert.deepEqual(h.filters, [NON_LEVEL_EVENTS], "the stream must drop levels");
  assert.equal(dropped, 1, "the drop must be announced so the meters clear");
  assert.equal(h.store.getState().levels.size, 0);
});

test("returning within the grace keeps the stream untouched", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  // The start-up route is the dashboard.
  h.store.setLevelsWanted(true);
  h.store.setLevelsWanted(false);
  h.store.setLevelsWanted(true);
  assert.equal(timers.pending(LEVELS_GRACE_MS).length, 0, "a return must cancel the grace");
  assert.deepEqual(h.filters, [], "a quick return must not reconnect the stream");
});

test("returning after the drop requests levels again at once", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  // The start-up route is the dashboard.
  h.store.setLevelsWanted(true);
  h.store.setLevelsWanted(false);
  const [grace] = timers.pending(LEVELS_GRACE_MS);
  assert.ok(grace);
  timers.fire(grace);
  h.store.setLevelsWanted(true);
  assert.deepEqual(h.filters, [NON_LEVEL_EVENTS, null], "the return must restore every event type");
  // Staying on the dashboard changes nothing more.
  h.store.setLevelsWanted(true);
  assert.equal(h.filters.length, 2);
});

test("the start-up route on the dashboard changes nothing", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  h.store.setLevelsWanted(true);
  assert.deepEqual(h.filters, [], "a fresh store already streams levels");
  assert.equal(timers.pending(LEVELS_GRACE_MS).length, 0);
});

test("a start-up route off the dashboard filters levels at once", () => {
  const timers = new FakeTimers();
  const h = harness(timers);
  // A deep link to another view: the stream has not opened yet.
  h.store.setLevelsWanted(false);
  assert.deepEqual(h.filters, [NON_LEVEL_EVENTS], "the first connect must already leave levels out");
  assert.equal(timers.pending(LEVELS_GRACE_MS).length, 0, "no grace for levels never shown");
  // Moving to the dashboard brings them in at once.
  h.store.setLevelsWanted(true);
  assert.deepEqual(h.filters, [NON_LEVEL_EVENTS, null]);
});
