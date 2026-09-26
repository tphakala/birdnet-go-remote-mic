// WCAG contrast regression tests for the design tokens in web/static/styles.css.
//
// Why this exists: html-validate (task web:a11y) checks markup rules but cannot
// compute a colour ratio, and it only ever parses index.html, so every control
// the TypeScript renders at runtime is invisible to it. Contrast was therefore
// verified by hand, one surface at a time, which is how the light-theme primary
// button shipped at 5.2:1 and how a scatter of one-off ":root[data-theme=light]
// .thing { color: #xxx }" overrides accumulated: each fixed the surface someone
// happened to look at, and the next new usage of the same token failed again.
//
// These tests read the real stylesheet, resolve the token graph the way a
// browser would, and assert the pairs the UI actually paints. A token edit that
// drops a pair below AA now fails the build instead of shipping.
//
// Scope and limits, stated plainly: this checks TOKEN pairs, not rendered DOM.
// It cannot know that some new rule put --text-faint on --bg-surface-active. It
// is a regression net over the pairs listed in PAIRS below, so a new coloured
// surface needs a new entry here. A full rendered sweep needs a browser and
// belongs in a separate harness.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// Compiled output lives at web/.test-out/test/, so the stylesheet is two levels
// up in web/static/. Resolving from import.meta.url rather than process.cwd()
// keeps the test correct whatever directory the runner is invoked from.
const STYLES = fileURLToPath(new URL("../../static/styles.css", import.meta.url).href);

interface RGBA {
  r: number;
  g: number;
  b: number;
  a: number;
}

// WCAG 2.1 SC 1.4.3: 4.5:1 for body text, 3:1 for large text (>=24px, or
// >=18.66px bold). Everything asserted here is small text, so AA is the bar.
const AA = 4.5;
// SC 1.4.11 non-text contrast, for a glyph or control boundary carrying meaning.
const AA_NON_TEXT = 3;
// Secondary and muted text is the UI's small print: 12-13px labels, hints,
// subtitles and the 11px meter scale. AA's 4.5:1 is a floor for text of any
// size, and text this small and this thin read as faint at exactly that ratio,
// so these tokens keep a margin above it. test/legibility.test.ts holds the
// matching size and weight floor.
const SMALL_TEXT = 5.5;

function parseColor(raw: string): RGBA | null {
  const v = raw.trim();

  const hex = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(v);
  if (hex) {
    const h = hex[1];
    const wide = h.length === 3 ? h.split("").map((c) => c + c).join("") : h;
    return {
      r: parseInt(wide.slice(0, 2), 16),
      g: parseInt(wide.slice(2, 4), 16),
      b: parseInt(wide.slice(4, 6), 16),
      a: 1,
    };
  }

  const fn = /^rgba?\(([^)]+)\)$/i.exec(v);
  if (fn) {
    const parts = fn[1].split(",").map((s) => Number(s.trim()));
    if (parts.length < 3 || parts.some((n) => Number.isNaN(n))) return null;
    return { r: parts[0], g: parts[1], b: parts[2], a: parts.length > 3 ? parts[3] : 1 };
  }

  return null;
}

// Source-over compositing, the same operation a browser performs when a
// translucent tint sits on an opaque ground.
function over(fg: RGBA, bg: RGBA): RGBA {
  return {
    r: fg.r * fg.a + bg.r * (1 - fg.a),
    g: fg.g * fg.a + bg.g * (1 - fg.a),
    b: fg.b * fg.a + bg.b * (1 - fg.a),
    a: 1,
  };
}

function luminance(c: RGBA): number {
  const channel = (v: number): number => {
    const s = v / 255;
    return s <= 0.04045 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(c.r) + 0.7152 * channel(c.g) + 0.0722 * channel(c.b);
}

function contrast(a: RGBA, b: RGBA): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

// Comments are stripped before any block is located. A CSS comment may contain
// a brace (this file's own token comments quote a rule to explain themselves),
// and a naive scan for the block's closing brace would stop inside one and
// silently return a truncated set of tokens.
function stripComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, "");
}

// Pull the custom-property declarations out of one selector's block. With
// comments gone the token blocks contain no nested braces, so reading to the
// first closing brace is enough and avoids carrying a CSS parser.
function blockDeclarations(source: string, selector: RegExp): Map<string, string> {
  const css = stripComments(source);
  const open = selector.exec(css);
  if (!open) throw new Error(`token block not found: ${selector}`);
  const start = open.index + open[0].length;
  const end = css.indexOf("}", start);
  if (end < 0) throw new Error(`unterminated token block: ${selector}`);

  const out = new Map<string, string>();
  const decl = /--([\w-]+)\s*:\s*([^;]+);/g;
  const body = css.slice(start, end);
  let m: RegExpExecArray | null;
  while ((m = decl.exec(body)) !== null) out.set(`--${m[1]}`, m[2].trim());
  return out;
}

// The light theme is the base :root block with the light block layered over it,
// which is exactly the cascade a browser applies for :root[data-theme="light"].
function tokensFor(css: string, theme: "dark" | "light"): Map<string, string> {
  const base = blockDeclarations(css, /^:root\s*\{/m);
  if (theme === "dark") return base;
  const light = blockDeclarations(css, /^:root\[data-theme="light"\]\s*\{/m);
  return new Map([...base, ...light]);
}

// Resolve a token through any chain of var() indirection. The depth cap turns a
// circular definition into a clear failure rather than a stack overflow.
function colorOf(tokens: Map<string, string>, name: string): RGBA {
  let value = tokens.get(name);
  if (value === undefined) throw new Error(`undefined token: ${name}`);

  for (let depth = 0; depth < 10; depth++) {
    const direct = parseColor(value);
    if (direct) return direct;

    const ref = /^var\(\s*(--[\w-]+)\s*(?:,\s*([^)]+))?\)$/.exec(value.trim());
    if (!ref) break;

    const next = tokens.get(ref[1]) ?? ref[2];
    if (next === undefined) throw new Error(`undefined token: ${ref[1]} (via ${name})`);
    value = next;
  }
  throw new Error(`token ${name} is not a resolvable colour: ${tokens.get(name)}`);
}

// Compose an opaque ground with any translucent tints painted on top of it, in
// paint order. Mirrors how the severity surfaces layer a --sev-soft wash over
// an opaque card, and how a *-bg token sits on a card surface.
function ground(tokens: Map<string, string>, base: string, ...tints: string[]): RGBA {
  let result = colorOf(tokens, base);
  for (const tint of tints) result = over(colorOf(tokens, tint), result);
  return result;
}

interface Pair {
  what: string;
  fg: string;
  /** Opaque ground first, then any translucent tints in paint order. */
  bg: [string, ...string[]];
  min: number;
}

// Every foreground/background pair the UI actually paints in small text. Each
// entry names the surface it guards so a failure says what to go and look at.
const PAIRS: Pair[] = [
  // Filled primary CTA. The pair that regressed: the light theme reused the
  // dark theme's near-black label on an accent that had to darken for the white
  // card, and landed at 5.2:1 of muddy dark-on-cyan.
  { what: "primary button label", fg: "--btn-primary-fg", bg: ["--btn-primary-bg"], min: AA },
  { what: "primary button label (hover)", fg: "--btn-primary-fg", bg: ["--btn-primary-bg-hover"], min: AA },
  // The danger button is a solid fill like the primary one (its label is
  // --btn-danger-fg on the fill), not a *-text token on a tint.
  { what: "danger button label", fg: "--btn-danger-fg", bg: ["--btn-danger-bg"], min: AA },
  { what: "danger button label (hover)", fg: "--btn-danger-fg", bg: ["--btn-danger-bg-hover"], min: AA },

  // Body copy on each of the three opaque grounds.
  { what: "primary text on a card", fg: "--text-primary", bg: ["--bg-surface"], min: AA },
  { what: "primary text on the page (About system details)", fg: "--text-primary", bg: ["--bg-page"], min: AA },
  { what: "primary text on the app ground", fg: "--text-primary", bg: ["--bg-app"], min: AA },
  { what: "secondary text on a card (and the menu item icon)", fg: "--text-secondary", bg: ["--bg-surface"], min: SMALL_TEXT },
  { what: "secondary text on a raised card", fg: "--text-secondary", bg: ["--bg-surface-raised"], min: SMALL_TEXT },
  { what: "primary text on a raised card (hovered menu item, inline code, toast message)", fg: "--text-primary", bg: ["--bg-surface-raised"], min: AA },
  { what: "secondary text on the app ground", fg: "--text-secondary", bg: ["--bg-app"], min: SMALL_TEXT },

  // Muted text is the 12px label size and the 11px meter scale, so it is the
  // most fragile. --meter-bg is a distinctly different ground from the cards
  // and was the one --text-muted had never been checked against.
  { what: "muted text on a card (license file names)", fg: "--text-muted", bg: ["--bg-surface"], min: SMALL_TEXT },
  { what: "muted text on a raised card (and the toast dismiss glyph)", fg: "--text-muted", bg: ["--bg-surface-raised"], min: SMALL_TEXT },
  { what: "muted text on the page", fg: "--text-muted", bg: ["--bg-page"], min: SMALL_TEXT },
  { what: "meter scale labels on the meter trough", fg: "--text-muted", bg: ["--meter-bg"], min: SMALL_TEXT },

  // Signal colours used as TEXT on their own tint: status badges, the live
  // indicator, tech tags. These are the *-text tokens rather than the base
  // signal colours, because a fill and a label want different colours out of the
  // same hue.
  { what: "ok badge label", fg: "--signal-ok-text", bg: ["--bg-surface", "--signal-ok-bg"], min: AA },
  { what: "ok label on the app ground", fg: "--signal-ok-text", bg: ["--bg-app"], min: AA },
  { what: "ok label on a card", fg: "--signal-ok-text", bg: ["--bg-surface"], min: AA },
  { what: "warn badge label", fg: "--signal-warn-text", bg: ["--bg-surface", "--signal-warn-bg"], min: AA },
  { what: "warn label on the app ground", fg: "--signal-warn-text", bg: ["--bg-app"], min: AA },
  { what: "crit badge label", fg: "--signal-crit-text", bg: ["--bg-surface", "--signal-crit-bg"], min: AA },
  { what: "crit label on a raised card", fg: "--signal-crit-text", bg: ["--bg-surface-raised", "--signal-crit-bg"], min: AA },

  // Accent used as text: the highlighted tech tag and the dropdown's selected
  // value. The raised card is the worse of the two grounds.
  // The login/confirm modal status line is the same pair.
  { what: "accent tag label (and modal status text)", fg: "--accent-cyan-text", bg: ["--bg-surface", "--accent-cyan-bg"], min: AA },
  { what: "accent tag label on a raised card", fg: "--accent-cyan-text", bg: ["--bg-surface-raised", "--accent-cyan-bg"], min: AA },
  { what: "accent label on a card (license disclosure link)", fg: "--accent-cyan-text", bg: ["--bg-surface"], min: AA },

  // The ultrasonic (PCM L16) tag, same shape as the accent tag.
  { what: "ultrasonic tag label", fg: "--ultrasonic-text", bg: ["--bg-surface", "--ultrasonic-bg"], min: AA },
  { what: "ultrasonic tag label on a raised card", fg: "--ultrasonic-text", bg: ["--bg-surface-raised", "--ultrasonic-bg"], min: AA },

  // Signal label on a plain card, with no tint under it: the access-token state
  // line, the settings-drift notice and its inline Reload link.
  { what: "warn label on a card", fg: "--signal-warn-text", bg: ["--bg-surface"], min: AA },

  // The open-access banner sits on the app ground under a warn tint, and puts
  // both an icon and a link on it.
  { what: "open-access banner icon", fg: "--signal-warn-text", bg: ["--bg-app", "--signal-warn-bg"], min: AA_NON_TEXT },
  { what: "open-access banner link", fg: "--accent-cyan-text", bg: ["--bg-app", "--signal-warn-bg"], min: AA },
  // The Events page's load-failure notice: body text on the warn tint over a
  // card.
  { what: "events load notice text", fg: "--text-primary", bg: ["--bg-surface", "--signal-warn-bg"], min: AA },

  // BODY TEXT ON A NOTIFICATION ROW. Every row, whatever its severity, is the
  // neutral composited row surface (severity is carried by the icon badge, not
  // by a tint), so one set of pairs covers them all. It still needs asserting:
  // it is a different ground from any plain card, and it is the one every row
  // in the panel uses.
  { what: "notification row timestamp and message", fg: "--text-secondary", bg: ["--bg-surface", "--bg-surface-subtle"], min: SMALL_TEXT },
  { what: "notification row title", fg: "--text-primary", bg: ["--bg-surface", "--bg-surface-subtle"], min: AA },
  // Panel furniture that sits on its own grounds rather than on a row.
  { what: "notification category chip", fg: "--text-secondary", bg: ["--bg-surface-active"], min: SMALL_TEXT },
  { what: "active-issues group heading", fg: "--signal-crit-text", bg: ["--bg-surface"], min: AA },
  // The unread count on the header bell and the latched CLIP label: the
  // smallest text in the UI (11-12px bold) on a solid crit fill, so it takes
  // the small-text bar.
  { what: "unread badge count and latched CLIP label", fg: "--crit-fill-fg", bg: ["--crit-fill-bg"], min: SMALL_TEXT },

  // Menu button popover (the header theme menu): the item icons and the
  // hovered or focused item's label reuse pairs above; the accent check mark
  // marks the chosen one.
  { what: "menu check mark (hover)", fg: "--accent-cyan-text", bg: ["--bg-surface-raised"], min: AA_NON_TEXT },

  // About page: license texts sit on the page ground inside a card (the system
  // details, file names, inline code and disclosure link reuse pairs above).
  { what: "license text", fg: "--text-secondary", bg: ["--bg-page"], min: SMALL_TEXT },

  // Toasts are the neutral raised surface for every severity; the message and
  // the dismiss glyph reuse the raised-card pairs above.

  // Severity glyphs in their badge tile. These are icons, not text, so SC
  // 1.4.11 applies: they still have to be distinguishable from the tile.
  { what: "ok glyph in its severity badge", fg: "--signal-ok-text", bg: ["--bg-surface", "--signal-ok-glow"], min: AA_NON_TEXT },
  { what: "warn glyph in its severity badge", fg: "--signal-warn-text", bg: ["--bg-surface", "--signal-warn-glow"], min: AA_NON_TEXT },
  { what: "crit glyph in its severity badge", fg: "--signal-crit-text", bg: ["--bg-surface", "--signal-crit-glow"], min: AA_NON_TEXT },

  // The keyboard focus ring (a 2px --accent-cyan outline, 2px outside the
  // control) against every ground a focusable control sits on. SC 1.4.11
  // applies: the ring is the only sign of focus.
  { what: "focus ring on the app ground", fg: "--accent-cyan", bg: ["--bg-app"], min: AA_NON_TEXT },
  { what: "focus ring on the page", fg: "--accent-cyan", bg: ["--bg-page"], min: AA_NON_TEXT },
  { what: "focus ring on a card", fg: "--accent-cyan", bg: ["--bg-surface"], min: AA_NON_TEXT },
  { what: "focus ring on a raised card", fg: "--accent-cyan", bg: ["--bg-surface-raised"], min: AA_NON_TEXT },
];

// Rules that paint the saturated base accent as an icon colour, where the
// accent would fail as the only indicator but each is decorative under SC
// 1.4.11: a visible label or ARIA state beside it carries the meaning. The
// judgement is recorded here so the list is reviewed when one of them
// changes; a test below keeps it in step with the stylesheet. A rendered
// sweep would be needed to prove nothing else paints the base accent on its
// own.
const DECORATIVE = new Map<string, string>([
  [".brand-icon", "the logo beside the brand name"],
  [".nav-item:hover svg", "hover tint on a tab icon; the tab label carries the meaning"],
  [".custom-dropdown.open .dropdown-chevron", "open chevron; aria-expanded and the open list carry the state"],
]);

const css = readFileSync(STYLES, "utf8");

for (const theme of ["dark", "light"] as const) {
  test(`${theme} theme: token pairs meet WCAG AA contrast`, () => {
    const tokens = tokensFor(css, theme);
    const failures: string[] = [];

    for (const pair of PAIRS) {
      const fg = colorOf(tokens, pair.fg);
      const bg = ground(tokens, ...pair.bg);
      // The foreground may itself be translucent, so composite before measuring.
      const ratio = contrast(over(fg, bg), bg);
      if (ratio < pair.min) {
        failures.push(
          `${pair.what}: ${pair.fg} on ${pair.bg.join(" + ")} is ${ratio.toFixed(2)}:1, needs ${pair.min}:1`,
        );
      }
    }

    assert.deepEqual(failures, [], `\n  ${failures.join("\n  ")}\n`);
  });
}

test("each foreground and ground pair is listed once", () => {
  // A repeated pair asserts nothing new and hides which surfaces share it;
  // name the extra surface in the existing entry's description instead.
  const seen = new Map<string, string>();
  const repeats: string[] = [];
  for (const pair of PAIRS) {
    const key = `${pair.fg} on ${pair.bg.join(" + ")}`;
    const first = seen.get(key);
    if (first !== undefined) repeats.push(`"${pair.what}" repeats "${first}" (${key})`);
    else seen.set(key, pair.what);
  }
  assert.deepEqual(repeats, []);
});

test("every decorative accent icon rule still paints the base accent", () => {
  const source = stripComments(css);
  const stale: string[] = [];
  for (const selector of DECORATIVE.keys()) {
    const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const rule = new RegExp(`(?:^|[}\\s,])${escaped}\\s*\\{([^}]*)\\}`, "m").exec(source);
    if (!rule || !/color\s*:\s*var\(--accent-cyan\)/.test(rule[1])) stale.push(selector);
  }
  assert.deepEqual(stale, [], "update DECORATIVE: these rules no longer paint --accent-cyan");
});

test("a theme override only redefines tokens the base theme declares", () => {
  // The light block is an override layer, so it is expected to redeclare only a
  // subset (it inherits the fonts, radii and spacing from :root untouched). What
  // must never happen is the reverse: a token that exists ONLY in the light
  // block is undefined on the dark theme, which is how a theme silently loses a
  // contrast fix. colorOf would throw, but naming the gap directly is clearer.
  const dark = tokensFor(css, "dark");
  const light = blockDeclarations(css, /^:root\[data-theme="light"\]\s*\{/m);
  const unknown = [...light.keys()].filter((k) => !dark.has(k));
  assert.deepEqual(unknown, [], `light theme declares tokens the base theme does not: ${unknown.join(", ")}`);
});
