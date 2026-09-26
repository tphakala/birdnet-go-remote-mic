// Unit tests for the storage side of lib/prefs.ts: isLocalStorageEvent, the
// guard every storage listener uses to ignore sessionStorage changes, and the
// boolean preference read and write, including a browser where storage
// itself throws. Run with node:test over the compiled
// output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { isLocalStorageEvent, prefSaveNotice, readBoolPref, writeBoolPref } from "../src/lib/prefs.js";

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

const gs = globalThis as unknown as { localStorage?: unknown };

// withStorage runs fn with a stub localStorage backed by items, or one whose
// every call throws when items is "throws", then removes the stub.
function withStorage(items: Map<string, string> | "throws", fn: () => void): void {
  const fail = (): never => {
    throw new Error("SecurityError: storage blocked");
  };
  gs.localStorage =
    items === "throws"
      ? { getItem: fail, setItem: fail }
      : { getItem: (k: string) => items.get(k) ?? null, setItem: (k: string, v: string) => void items.set(k, v) };
  try {
    fn();
  } finally {
    delete gs.localStorage;
  }
}

test("a boolean preference round-trips as 1 and 0 and falls back when unset", () => {
  const items = new Map<string, string>();
  withStorage(items, () => {
    assert.equal(readBoolPref("k", true), true);
    writeBoolPref("k", false);
    assert.equal(items.get("k"), "0");
    assert.equal(readBoolPref("k", true), false);
    writeBoolPref("k", true);
    assert.equal(readBoolPref("k", false), true);
  });
});

test("blocked storage reads the default and reports a failed save once", () => {
  let shown = 0;
  prefSaveNotice.setHandler(() => shown++);
  withStorage("throws", () => {
    assert.equal(readBoolPref("k", true), true);
    assert.equal(readBoolPref("k", false), false);
    writeBoolPref("k", true);
    writeBoolPref("k", false);
  });
  assert.equal(shown, 1);
});
