// Unit tests for initTheme (lib/theme.ts): at load the mode and theme are
// derived (saved choice, else the OS) rather than taken from the attribute, and
// an unchanged theme is not rewritten; onApply reports every change; System
// mode follows OS preference changes live and Light or Dark ignores them;
// setMode saves Light or Dark and clears the key for System; a mode chosen in
// another tab arrives through the storage event; and a save that fails is
// reported once. Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { initTheme, parseMode, THEME_KEY, type Theme, type ThemeController, type ThemeEnv, type ThemeMode, type ThemeStorageChange } from "../src/lib/theme.js";

interface Harness {
  ctl: ThemeController;
  theme: () => string | null;
  // How many times initTheme wrote data-theme on the root (seeding excluded).
  rootWrites: () => number;
  // Every onApply call, in order.
  applied: Array<[ThemeMode, Theme]>;
  // osChange fires the media query's change event, as an OS switch would.
  osChange: (prefersLight: boolean) => void;
  listening: () => boolean;
  // otherTab delivers a storage event, as a write in another tab would.
  otherTab: (e: ThemeStorageChange) => void;
  stored: Map<string, string>;
  // How many setItem and removeItem calls initTheme made.
  writes: () => number;
  saveFailures: () => number;
}

interface Options {
  initial?: string; // data-theme as theme-init.ts left it
  saved?: string; // the stored choice, if any
  // "getter" throws on reading localStorage itself (Chromium with site data
  // blocked); "calls" throws from getItem, setItem and removeItem instead.
  // "remove" throws from removeItem only, so an old saved value still reads.
  blocked?: "getter" | "calls" | "remove";
  // matchMedia missing, or a MediaQueryList without addEventListener.
  media?: "none" | "legacy";
  // The OS preference when initTheme runs (media.matches); false (dark) unless
  // a case needs the OS preferring light at load.
  initialMatches?: boolean;
  // A media query whose addEventListener throws.
  addThrows?: boolean;
  // No storage event wiring (onStorage omitted).
  noStorageEvents?: boolean;
}

function harness(opts: Options = {}): Harness {
  const attrs = new Map<string, string>();
  if (opts.initial !== undefined) attrs.set("data-theme", opts.initial);
  let rootWrites = 0;
  let changeHandler: ((e: { matches: boolean }) => void) | null = null;
  let storageHandler: ((e: ThemeStorageChange) => void) | null = null;
  const stored = new Map<string, string>();
  if (opts.saved !== undefined) stored.set(THEME_KEY, opts.saved);
  let writes = 0;
  let saveFailures = 0;
  const applied: Array<[ThemeMode, Theme]> = [];

  const guard = (): void => {
    if (opts.blocked === "calls") throw new Error("storage blocked");
  };
  const storage = {
    getItem(key: string): string | null {
      guard();
      return stored.get(key) ?? null;
    },
    setItem(key: string, value: string): void {
      guard();
      writes++;
      stored.set(key, value);
    },
    removeItem(key: string): void {
      guard();
      if (opts.blocked === "remove") throw new Error("remove blocked");
      writes++;
      stored.delete(key);
    },
  };
  const matches = opts.initialMatches ?? false;
  let media: ThemeEnv["media"] = null;
  if (opts.media === "legacy") {
    media = { matches };
  } else if (opts.media !== "none") {
    media = {
      matches,
      addEventListener(_type: "change", listener: (e: { matches: boolean }) => void): void {
        if (opts.addThrows) throw new Error("cannot listen");
        changeHandler = listener;
      },
    };
  }

  const ctl = initTheme({
    root: {
      getAttribute: (name) => attrs.get(name) ?? null,
      setAttribute: (name, value) => {
        rootWrites++;
        attrs.set(name, value);
      },
    },
    storage: () => {
      if (opts.blocked === "getter") throw new Error("SecurityError: storage blocked");
      return storage;
    },
    media,
    ...(opts.noStorageEvents ? {} : { onStorage: (l: (e: ThemeStorageChange) => void) => { storageHandler = l; } }),
    onApply: (mode, theme) => {
      applied.push([mode, theme]);
    },
    onSaveFailed: () => {
      saveFailures++;
    },
  });

  return {
    ctl,
    theme: () => attrs.get("data-theme") ?? null,
    rootWrites: () => rootWrites,
    applied,
    osChange: (prefersLight) => changeHandler?.({ matches: prefersLight }),
    listening: () => changeHandler !== null,
    otherTab: (e) => {
      assert.ok(storageHandler, "no storage listener registered");
      storageHandler(e);
    },
    stored,
    writes: () => writes,
    saveFailures: () => saveFailures,
  };
}

test("parseMode reads light and dark, and anything else as System", () => {
  assert.equal(parseMode("light"), "light");
  assert.equal(parseMode("dark"), "dark");
  assert.equal(parseMode(null), "system");
  assert.equal(parseMode("sepia"), "system");
});

test("a load whose derived theme is already shown writes nothing", () => {
  // Every branch of the load derivation: the OS, a saved choice, and the
  // attribute when matchMedia is missing.
  for (const opts of [
    { initial: "dark" },
    { initial: "light", saved: "light" },
    { initial: "light", media: "none" as const },
  ]) {
    const h = harness(opts);
    assert.equal(h.rootWrites(), 0, JSON.stringify(opts));
  }
  // A load that corrects the attribute writes it once.
  assert.equal(harness({ initial: "dark", initialMatches: true }).rootWrites(), 1);
});

test("the load reports the mode and the theme it resolves to", () => {
  assert.deepEqual(harness({ initial: "dark" }).applied, [["system", "dark"]]);
  assert.deepEqual(harness({ initial: "light", saved: "light" }).applied, [["light", "light"]]);
  assert.deepEqual(harness({ initial: "dark", initialMatches: true }).applied, [["system", "light"]]);
  assert.equal(harness({ saved: "dark" }).ctl.mode(), "dark");
});

test("at load a saved choice wins over the OS preference", () => {
  const h = harness({ initial: "dark", saved: "dark", initialMatches: true });
  assert.equal(h.theme(), "dark");
});

test("at load with nothing saved the OS preference corrects a stale attribute", () => {
  // theme-init.js did not run (index.html hardcodes dark), or the OS switched
  // between the two scripts: the OS now prefers light.
  const h = harness({ initial: "dark", initialMatches: true });
  assert.equal(h.theme(), "light");
});

test("at load a saved choice corrects a stale attribute", () => {
  const h = harness({ initial: "dark", saved: "light" });
  assert.equal(h.theme(), "light");
});

test("without matchMedia System keeps the attribute's theme (missing or unknown becomes dark)", () => {
  assert.equal(harness({ media: "none" }).theme(), "dark");
  assert.deepEqual(harness({ initial: "sepia", media: "none" }).applied, [["system", "dark"]]);
  assert.equal(harness({ initial: "light", media: "none" }).theme(), "light");
});

test("System mode follows OS changes live, without saving", () => {
  const h = harness({ initial: "dark" });
  assert.ok(h.listening());
  h.osChange(true);
  assert.equal(h.theme(), "light");
  h.osChange(false);
  assert.equal(h.theme(), "dark");
  assert.deepEqual(h.applied.at(-1), ["system", "dark"]);
  assert.equal(h.stored.has(THEME_KEY), false);
});

test("a saved Light or Dark choice ignores OS changes", () => {
  const h = harness({ initial: "dark", saved: "dark" });
  h.osChange(true);
  assert.equal(h.theme(), "dark");
});

test("an unrecognized saved value counts as System", () => {
  const h = harness({ initial: "dark", saved: "sepia" });
  assert.equal(h.ctl.mode(), "system");
  h.osChange(true);
  assert.equal(h.theme(), "light");
});

test("choosing Light or Dark saves it and stops following the OS", () => {
  const h = harness({ initial: "dark" });
  h.ctl.setMode("light");
  assert.equal(h.theme(), "light");
  assert.equal(h.stored.get(THEME_KEY), "light");
  assert.deepEqual(h.applied.at(-1), ["light", "light"]);
  h.osChange(false);
  assert.equal(h.theme(), "light");
  h.ctl.setMode("dark");
  assert.equal(h.theme(), "dark");
  assert.equal(h.stored.get(THEME_KEY), "dark");
  assert.equal(h.saveFailures(), 0);
});

test("choosing System clears the saved choice and follows the OS again", () => {
  const h = harness({ initial: "dark", saved: "dark", initialMatches: true });
  h.ctl.setMode("system");
  assert.equal(h.stored.has(THEME_KEY), false);
  // The OS preferred light at load, and the listener sees later changes.
  assert.equal(h.theme(), "light");
  assert.deepEqual(h.applied.at(-1), ["system", "light"]);
  h.osChange(false);
  assert.equal(h.theme(), "dark");
});

test("a mode chosen in another tab applies at once", () => {
  const h = harness({ initial: "dark" });
  h.otherTab({ key: THEME_KEY, newValue: "light" });
  assert.equal(h.theme(), "light");
  assert.equal(h.ctl.mode(), "light");
  h.osChange(false);
  assert.equal(h.theme(), "light", "an explicit choice from another tab stops the follow");
  // The other tab went back to System (removed the key).
  h.otherTab({ key: THEME_KEY, newValue: null });
  assert.equal(h.ctl.mode(), "system");
  h.osChange(false);
  assert.equal(h.theme(), "dark");
  // Nothing is written here: the write was the other tab's.
  assert.equal(h.stored.has(THEME_KEY), false);
});

test("the storage event ignores other keys and repeats, and a full clear means System", () => {
  const h = harness({ initial: "light", saved: "light" });
  const before = h.applied.length;
  h.otherTab({ key: "remote-mic-hide-inactive:x", newValue: "0" });
  h.otherTab({ key: THEME_KEY, newValue: "light" });
  assert.equal(h.applied.length, before, "an unrelated key or the same mode applies nothing");
  h.otherTab({ key: null, newValue: null });
  assert.equal(h.ctl.mode(), "system");
  assert.equal(h.theme(), "dark");
});

test("blocked storage starts in System, still switches, and reports the failed save once", () => {
  for (const blocked of ["getter", "calls"] as const) {
    const h = harness({ initial: "dark", blocked });
    assert.equal(h.ctl.mode(), "system", blocked);
    h.osChange(true);
    assert.equal(h.theme(), "light", blocked);
    h.ctl.setMode("dark");
    assert.equal(h.theme(), "dark", blocked);
    assert.equal(h.saveFailures(), 1, blocked);
    // The choice holds for this visit even though it could not be saved.
    h.osChange(true);
    assert.equal(h.theme(), "dark", blocked);
    h.ctl.setMode("light");
    h.ctl.setMode("system");
    assert.equal(h.saveFailures(), 1, blocked);
  }
});

test("a missing matchMedia, a legacy MediaQueryList, or a listener that cannot be added still switches", () => {
  for (const opts of [{ media: "none" as const }, { media: "legacy" as const }, { addThrows: true }]) {
    const h = harness({ initial: "dark", ...opts });
    assert.equal(h.listening(), false, JSON.stringify(opts));
    h.ctl.setMode("light");
    assert.equal(h.theme(), "light", JSON.stringify(opts));
    assert.equal(h.stored.get(THEME_KEY), "light", JSON.stringify(opts));
  }
});

test("without storage events the theme still applies and follows", () => {
  const h = harness({ initial: "light", noStorageEvents: true, initialMatches: true });
  assert.equal(h.theme(), "light");
  h.osChange(false);
  assert.equal(h.theme(), "dark");
});

test("choosing System with blocked storage reports nothing: it reads back as System anyway", () => {
  for (const blocked of ["getter", "calls"] as const) {
    const h = harness({ initial: "dark", blocked });
    h.ctl.setMode("system");
    assert.equal(h.saveFailures(), 0, blocked);
    h.ctl.setMode("dark");
    assert.equal(h.saveFailures(), 1, blocked);
  }
});

test("an OS change while Light or Dark is chosen is remembered for a later switch to System", () => {
  const h = harness({ initial: "dark", saved: "dark" });
  h.osChange(true);
  assert.equal(h.theme(), "dark");
  h.ctl.setMode("system");
  assert.equal(h.theme(), "light");
});

test("a storage event from another tab never writes storage here", () => {
  const h = harness({ initial: "dark" });
  for (const newValue of ["light", "dark", null, "sepia"]) {
    h.otherTab({ key: THEME_KEY, newValue });
    assert.equal(h.writes(), 0, String(newValue));
  }
  h.otherTab({ key: null, newValue: null });
  assert.equal(h.writes(), 0);
});

test("choosing System warns when a failed removal leaves an older choice that would win on reload", () => {
  const h = harness({ initial: "dark", saved: "dark", blocked: "remove" });
  h.ctl.setMode("system");
  assert.equal(h.stored.get(THEME_KEY), "dark", "the old choice is still stored");
  assert.equal(h.saveFailures(), 1);
});
