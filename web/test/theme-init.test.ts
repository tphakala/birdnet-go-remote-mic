// Unit tests for theme-init, the classic script that applies the theme before
// the first paint. It has no exports, so each case runs the compiled script in
// a fresh node:vm context holding stubs for the browser globals it touches.
// node:vm runs it with classic-script semantics, as the <head> tag does, so an
// import or export added to it fails here instead of only in the browser. Run
// with node:test over the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Script } from "node:vm";

import { PREFERS_LIGHT_QUERY, THEME_KEY } from "../src/lib/theme.js";

// Compiled tests live in web/.test-out/test/, the compiled script in
// web/.test-out/src/, and the sources two levels up; resolve from
// import.meta.url rather than process.cwd().
const THEME_INIT_JS = fileURLToPath(new URL("../src/theme-init.js", import.meta.url).href);
const INDEX_HTML = fileURLToPath(new URL("../../static/index.html", import.meta.url).href);

const script = new Script(readFileSync(THEME_INIT_JS, "utf8"), { filename: "theme-init.js" });

interface Env {
  // The stored preference; undefined blocks storage the way Chromium does
  // with site data blocked, where reading localStorage itself throws.
  stored?: string | null;
  // Blocks storage the other way instead: localStorage exists, getItem throws.
  getItemThrows?: boolean;
  // The OS preference; undefined makes the matchMedia call throw.
  prefersLight?: boolean;
  // Replaces matchMedia with an absent property, or one that returns null.
  media?: "missing" | "null";
}

// How many times the last runThemeInit called matchMedia, so a test can tell
// "never consulted" from "consulted and its throw swallowed".
let mediaCalls = 0;

// runThemeInit executes theme-init.js against stubbed globals and returns the
// data-theme it set, or null when it set none.
function runThemeInit(env: Env): string | null {
  let theme: string | null = null;
  mediaCalls = 0;
  // The context is the script's global object; window refers back to it, as
  // in a browser, so bare and window-qualified names resolve alike.
  const context: Record<string, unknown> = {
    document: {
      documentElement: {
        setAttribute(name: string, value: string): void {
          if (name === "data-theme") theme = value;
        },
      },
    },
  };
  context.window = context;
  if (env.stored === undefined) {
    Object.defineProperty(context, "localStorage", {
      get(): never {
        throw new Error("SecurityError: storage blocked");
      },
    });
  } else {
    const stored = env.stored;
    context.localStorage = {
      getItem(k: string): string | null {
        if (env.getItemThrows) throw new Error("storage blocked");
        // Only the key lib/theme.ts saves the toggle's choice under reads back,
        // so the two cannot drift apart unnoticed (theme-init cannot import it).
        return k === THEME_KEY ? stored : null;
      },
    };
  }
  if (env.media === "null") {
    context.matchMedia = (): null => {
      mediaCalls++;
      return null;
    };
  } else if (env.media !== "missing") {
    context.matchMedia = (query: string): { matches: boolean } => {
      mediaCalls++;
      if (env.prefersLight === undefined) throw new Error("no matchMedia");
      // Likewise only the query the live follow in lib/theme.ts listens on.
      return { matches: query === PREFERS_LIGHT_QUERY && env.prefersLight };
    };
  }
  script.runInNewContext(context);
  return theme;
}

test("a saved choice wins over the OS preference", () => {
  assert.equal(runThemeInit({ stored: "light", prefersLight: false }), "light");
  assert.equal(runThemeInit({ stored: "dark", prefersLight: true }), "dark");
});

test("with no saved choice the OS preference decides", () => {
  assert.equal(runThemeInit({ stored: null, prefersLight: true }), "light");
  assert.equal(runThemeInit({ stored: null, prefersLight: false }), "dark");
});

test("an unrecognized saved value falls back to the OS preference", () => {
  assert.equal(runThemeInit({ stored: "sepia", prefersLight: true }), "light");
  assert.equal(runThemeInit({ stored: "", prefersLight: false }), "dark");
});

test("blocked storage still follows the OS preference without throwing", () => {
  assert.equal(runThemeInit({ stored: undefined, prefersLight: true }), "light");
  assert.equal(runThemeInit({ stored: undefined, prefersLight: false }), "dark");
  assert.equal(runThemeInit({ stored: undefined, prefersLight: undefined }), "dark");
  assert.equal(runThemeInit({ stored: null, getItemThrows: true, prefersLight: true }), "light");
  assert.equal(runThemeInit({ stored: null, getItemThrows: true, prefersLight: false }), "dark");
});

test("a missing matchMedia keeps the dark default without throwing", () => {
  assert.equal(runThemeInit({ stored: null, prefersLight: undefined }), "dark");
  // Pins that the counter sees a call whose throw was swallowed, which the
  // saved-choice test below relies on.
  assert.equal(mediaCalls, 1);
  assert.equal(runThemeInit({ stored: null, media: "missing" }), "dark");
  assert.equal(runThemeInit({ stored: null, media: "null" }), "dark");
});

test("a saved choice applies without consulting matchMedia", () => {
  assert.equal(runThemeInit({ stored: "light", prefersLight: undefined }), "light");
  assert.equal(mediaCalls, 0);
});

test("index.html loads theme-init.js as a blocking classic script in <head>", () => {
  const html = readFileSync(INDEX_HTML, "utf8");
  const head = /<head>([\s\S]*?)<\/head>/.exec(html);
  assert.ok(head, "no <head> in index.html");
  // A commented-out tag loads nothing.
  const live = head[1].replace(/<!--[\s\S]*?-->/g, "");
  const tag = /<script\b([^>]*)\ssrc\s*=\s*["']?(?:\.?\/)?theme-init\.js(?=[?#"'\s>])["']?([^>]*)>/i.exec(live);
  assert.ok(tag, "theme-init.js is not loaded in <head>");
  const attrs = ` ${tag[1]} ${tag[2]} `;
  // A module, deferred or async script may run after the first paint, which
  // is the flash this script exists to prevent; a non-JavaScript type, or
  // nomodule in any browser that supports modules, never runs it at all.
  const type = /\stype\s*=\s*["']?([^"'\s>]+)/i.exec(attrs);
  assert.ok(
    type === null || ["text/javascript", "application/javascript"].includes(type[1].toLowerCase()),
    `theme-init.js tag has type ${type?.[1]}`,
  );
  assert.ok(!/\s(defer|async|nomodule)(?=[\s=/]|$)/i.test(attrs), "theme-init.js tag is deferred, async or nomodule");
});
