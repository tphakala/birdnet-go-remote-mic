// Unit tests for theme-init, the classic script that applies the theme before
// the first paint. It has no exports, so each case stubs the browser globals
// it touches and runs the compiled script afresh. Run with node:test over the
// compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

interface Env {
  // The stored preference; undefined makes storage access throw.
  stored?: string | null;
  // The OS preference; undefined makes matchMedia throw.
  prefersLight?: boolean;
}

let run = 0;

// runThemeInit executes theme-init.js against stubbed globals and returns the
// data-theme it set, or null when it set none.
async function runThemeInit(env: Env): Promise<string | null> {
  let theme: string | null = null;
  const stubs: Record<string, unknown> = {
    localStorage: {
      getItem(key: string): string | null {
        if (env.stored === undefined) throw new Error("storage blocked");
        return key === "remote-mic-theme" ? env.stored : null;
      },
    },
    window: {
      matchMedia(query: string): { matches: boolean } {
        if (env.prefersLight === undefined) throw new Error("no matchMedia");
        return { matches: query === "(prefers-color-scheme: light)" && env.prefersLight };
      },
    },
    document: {
      documentElement: {
        setAttribute(name: string, value: string): void {
          if (name === "data-theme") theme = value;
        },
      },
    },
  };
  const saved = new Map<string, PropertyDescriptor | undefined>();
  for (const [name, value] of Object.entries(stubs)) {
    saved.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  try {
    // A fresh query string re-evaluates the script instead of reusing the
    // cached module from an earlier case.
    run++;
    await import(new URL(`../src/theme-init.js?run=${run}`, import.meta.url).href);
  } finally {
    for (const [name, desc] of saved) {
      if (desc) Object.defineProperty(globalThis, name, desc);
      else Reflect.deleteProperty(globalThis, name);
    }
  }
  return theme;
}

test("a saved choice wins over the OS preference", async () => {
  assert.equal(await runThemeInit({ stored: "light", prefersLight: false }), "light");
  assert.equal(await runThemeInit({ stored: "dark", prefersLight: true }), "dark");
});

test("with no saved choice the OS preference decides", async () => {
  assert.equal(await runThemeInit({ stored: null, prefersLight: true }), "light");
  assert.equal(await runThemeInit({ stored: null, prefersLight: false }), "dark");
});

test("an unrecognized saved value falls back to the OS preference", async () => {
  assert.equal(await runThemeInit({ stored: "sepia", prefersLight: true }), "light");
  assert.equal(await runThemeInit({ stored: "", prefersLight: false }), "dark");
});

test("blocked storage keeps the dark default without throwing", async () => {
  assert.equal(await runThemeInit({ stored: undefined, prefersLight: true }), "dark");
});

test("a missing matchMedia keeps the dark default without throwing", async () => {
  assert.equal(await runThemeInit({ stored: null, prefersLight: undefined }), "dark");
});
