// Clear all is an operator choice, so a failed write of it raises the shared
// "preferences not saved" notice. It is its own file because the notice fires
// once per page (per process here): node:test runs each file in its own
// process, so the notice starts unfired, and no earlier report can mask this
// one. Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { NotificationStore } from "../src/lib/notifications.ts";
import { prefSaveNotice } from "../src/lib/prefs.ts";
import { FakeConnection, FakeTimers, notif, settle, snap } from "./fixtures.ts";

test("a failed clear-all write reports the notice", async () => {
  // Every write fails, as in a browser with site data blocked (set here rather
  // than relying on node having no localStorage).
  const g = globalThis as unknown as { localStorage?: unknown };
  const saved = g.localStorage;
  g.localStorage = { getItem: () => null, setItem: () => { throw new Error("blocked"); } };
  try {
    let shown = 0;
    prefSaveNotice.setHandler(() => shown++);
    const connection = new FakeConnection();
    const ns = new NotificationStore({
      api: {
        getNotifications: () => Promise.resolve(snap({ notifications: [notif({ id: 1 }), notif({ id: 2 })] })),
      },
      sse: { subscribe: () => () => true },
      connection,
      timers: new FakeTimers(),
    });
    connection.set(true);
    await settle();
    // The snapshot's write failed too, silently.
    assert.equal(shown, 0);
    ns.clearAll();
    assert.equal(shown, 1);
  } finally {
    g.localStorage = saved;
  }
});
