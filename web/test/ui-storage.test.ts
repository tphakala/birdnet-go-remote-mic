// Unit tests for isLocalStorageEvent (lib/ui.ts), the guard both storage
// listeners use to ignore sessionStorage changes, including a browser where
// reading localStorage itself throws. Run with node:test over the compiled
// output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { isLocalStorageEvent } from "../src/lib/ui.js";

const g = globalThis as unknown as { window?: unknown };

// withWindow runs fn with a stub window whose localStorage getter returns ls,
// or throws when ls is "throws", then removes the stub.
function withWindow(ls: object | "throws", fn: () => void): void {
  g.window = {
    get localStorage(): object {
      if (ls === "throws") throw new Error("SecurityError: storage blocked");
      return ls;
    },
  };
  try {
    fn();
  } finally {
    delete g.window;
  }
}

const event = (storageArea: object | null, key: string | null = "k"): StorageEvent =>
  ({ key, storageArea }) as unknown as StorageEvent;

test("a localStorage change or clear counts; a sessionStorage change does not", () => {
  const local = {};
  const session = {};
  withWindow(local, () => {
    // A change and a clear (key null) in another tab both carry this
    // window's localStorage as their area.
    assert.equal(isLocalStorageEvent(event(local)), true);
    assert.equal(isLocalStorageEvent(event(local, null)), true);
    assert.equal(isLocalStorageEvent(event(session)), false);
    // An event built without an area is let through.
    assert.equal(isLocalStorageEvent(event(null)), true);
  });
});

test("storage that cannot be read is treated as not ours", () => {
  withWindow("throws", () => {
    assert.equal(isLocalStorageEvent(event({})), false);
  });
});
