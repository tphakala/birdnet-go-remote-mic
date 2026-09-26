// Unit tests for initTheme (lib/theme.ts): the toggle keeps aria-pressed and
// its tooltip in step with data-theme, the theme follows OS preference changes
// live only while no choice is saved (here or, by the next OS change, in another
// tab), a click stops the follow and saves, and a save that fails is reported
// once. Run with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { initTheme, THEME_KEY, type ThemeEnv } from "../src/lib/theme.js";

interface Harness {
  theme: () => string | null;
  pressed: () => string | null;
  title: () => string;
  click: () => void;
  // osChange fires the media query's change event, as an OS switch would.
  osChange: (prefersLight: boolean) => void;
  listening: () => boolean;
  stored: Map<string, string>;
  saveFailures: () => number;
}

interface Options {
  initial?: string; // data-theme as theme-init.ts left it
  saved?: string; // the stored choice, if any
  // "getter" throws on reading localStorage itself (Chromium with site data
  // blocked); "calls" throws from getItem and setItem instead.
  blocked?: "getter" | "calls";
  // matchMedia missing; no addEventListener; or a listener that cannot be
  // removed, so only the follow flag stops a click from being overridden.
  media?: "none" | "legacy" | "sticky";
  noToggle?: boolean;
}

function harness(opts: Options = {}): Harness {
  const attrs = new Map<string, string>();
  if (opts.initial !== undefined) attrs.set("data-theme", opts.initial);
  const toggleAttrs = new Map<string, string>();
  let clickHandler: (() => void) | null = null;
  let changeHandler: ((e: { matches: boolean }) => void) | null = null;
  const stored = new Map<string, string>();
  if (opts.saved !== undefined) stored.set(THEME_KEY, opts.saved);
  let saveFailures = 0;

  const toggle = {
    title: "",
    setAttribute(name: string, value: string): void {
      toggleAttrs.set(name, value);
    },
    addEventListener(_type: "click", listener: () => void): void {
      clickHandler = listener;
    },
  };
  const storage = {
    getItem(key: string): string | null {
      if (opts.blocked === "calls") throw new Error("storage blocked");
      return stored.get(key) ?? null;
    },
    setItem(key: string, value: string): void {
      if (opts.blocked === "calls") throw new Error("storage blocked");
      stored.set(key, value);
    },
  };
  let media: ThemeEnv["media"] = null;
  if (opts.media === "legacy") {
    media = { matches: false };
  } else if (opts.media === "sticky") {
    media = {
      matches: false,
      addEventListener(_type: "change", listener: (e: { matches: boolean }) => void): void {
        changeHandler = listener;
      },
      removeEventListener(): void {
        throw new Error("cannot remove");
      },
    };
  } else if (opts.media !== "none") {
    media = {
      matches: false,
      addEventListener(_type: "change", listener: (e: { matches: boolean }) => void): void {
        changeHandler = listener;
      },
      removeEventListener(_type: "change", listener: (e: { matches: boolean }) => void): void {
        if (changeHandler === listener) changeHandler = null;
      },
    };
  }

  initTheme({
    root: {
      getAttribute: (name) => attrs.get(name) ?? null,
      setAttribute: (name, value) => {
        attrs.set(name, value);
      },
    },
    toggle: opts.noToggle ? null : toggle,
    storage: () => {
      if (opts.blocked === "getter") throw new Error("SecurityError: storage blocked");
      return storage;
    },
    media,
    onSaveFailed: () => {
      saveFailures++;
    },
  });

  return {
    theme: () => attrs.get("data-theme") ?? null,
    pressed: () => toggleAttrs.get("aria-pressed") ?? null,
    title: () => toggle.title,
    click: () => {
      assert.ok(clickHandler, "no click handler registered");
      clickHandler();
    },
    osChange: (prefersLight) => changeHandler?.({ matches: prefersLight }),
    listening: () => changeHandler !== null,
    stored,
    saveFailures: () => saveFailures,
  };
}

test("the toggle adopts the theme theme-init applied", () => {
  const light = harness({ initial: "light", saved: "light" });
  assert.equal(light.theme(), "light");
  assert.equal(light.pressed(), "false");
  assert.equal(light.title(), "Dark theme (off)");

  const dark = harness({ initial: "dark" });
  assert.equal(dark.theme(), "dark");
  assert.equal(dark.pressed(), "true");
  assert.equal(dark.title(), "Dark theme (on)");
});

test("a missing or unknown data-theme is treated as dark", () => {
  assert.equal(harness({}).theme(), "dark");
  assert.equal(harness({ initial: "sepia" }).pressed(), "true");
});

test("with no saved choice the theme follows OS changes live", () => {
  const h = harness({ initial: "dark" });
  assert.ok(h.listening());
  h.osChange(true);
  assert.equal(h.theme(), "light");
  assert.equal(h.pressed(), "false");
  h.osChange(false);
  assert.equal(h.theme(), "dark");
  assert.equal(h.pressed(), "true");
  // Following never persists: only a click is a choice.
  assert.equal(h.stored.has(THEME_KEY), false);
});

test("a saved choice ignores OS changes", () => {
  const h = harness({ initial: "dark", saved: "dark" });
  assert.equal(h.listening(), false);
  h.osChange(true);
  assert.equal(h.theme(), "dark");
});

test("an unrecognized saved value counts as no choice", () => {
  const h = harness({ initial: "dark", saved: "sepia" });
  h.osChange(true);
  assert.equal(h.theme(), "light");
});

test("a click saves the choice and stops following the OS", () => {
  const h = harness({ initial: "dark" });
  h.click();
  assert.equal(h.theme(), "light");
  assert.equal(h.pressed(), "false");
  assert.equal(h.stored.get(THEME_KEY), "light");
  assert.equal(h.listening(), false);
  h.osChange(false);
  assert.equal(h.theme(), "light");
  h.click();
  assert.equal(h.theme(), "dark");
  assert.equal(h.stored.get(THEME_KEY), "dark");
  assert.equal(h.saveFailures(), 0);
});

test("a choice saved by another tab wins at the next OS change", () => {
  const h = harness({ initial: "dark" });
  // Another tab clicks and saves light while this one is following the OS.
  h.stored.set(THEME_KEY, "light");
  h.osChange(false);
  assert.equal(h.theme(), "light");
  assert.equal(h.pressed(), "false");
  assert.equal(h.listening(), false);

  // Same when the listener cannot be removed: the flag alone ends the follow.
  const sticky = harness({ initial: "dark", media: "sticky" });
  sticky.stored.set(THEME_KEY, "light");
  sticky.osChange(false);
  assert.equal(sticky.theme(), "light");
  assert.ok(sticky.listening(), "the stub keeps its listener");
  sticky.stored.set(THEME_KEY, "dark");
  sticky.osChange(true);
  assert.equal(sticky.theme(), "light");
});

test("an unrecognized value saved by another tab keeps the follow", () => {
  const h = harness({ initial: "dark" });
  h.stored.set(THEME_KEY, "sepia");
  h.osChange(true);
  assert.equal(h.theme(), "light");
  assert.ok(h.listening());
});

test("a click wins over OS changes even when the listener cannot be removed", () => {
  const h = harness({ initial: "dark", media: "sticky" });
  h.click();
  assert.equal(h.theme(), "light");
  assert.ok(h.listening(), "the stub keeps its listener");
  h.osChange(false);
  assert.equal(h.theme(), "light");
});

test("blocked storage follows the OS, switches on click, and reports the failed save once", () => {
  for (const blocked of ["getter", "calls"] as const) {
    const h = harness({ initial: "dark", blocked });
    h.osChange(true);
    assert.equal(h.theme(), "light", blocked);
    h.click();
    assert.equal(h.theme(), "dark", blocked);
    assert.equal(h.saveFailures(), 1, blocked);
    // The click was an explicit choice even though it could not be saved.
    h.osChange(true);
    assert.equal(h.theme(), "dark", blocked);
    h.click();
    h.click();
    assert.equal(h.saveFailures(), 1, blocked);
  }
});

test("a missing matchMedia or legacy MediaQueryList still toggles", () => {
  for (const media of ["none", "legacy"] as const) {
    const h = harness({ initial: "dark", media });
    h.click();
    assert.equal(h.theme(), "light", media);
    assert.equal(h.stored.get(THEME_KEY), "light", media);
  }
});

test("a page without the toggle still applies and follows the theme", () => {
  const h = harness({ initial: "light", noToggle: true });
  assert.equal(h.theme(), "light");
  h.osChange(false);
  assert.equal(h.theme(), "dark");
});
