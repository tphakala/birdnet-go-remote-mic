// Type legibility floor and type scale for web/static/styles.css.
//
// Why this exists: contrast.test.ts measures colour pairs, but a pair that
// passes can still be hard to read when the text is tiny and thin. The UI had
// drifted to 10-11px captions and 12px regular-weight subtitles in the muted
// colour, each size picked locally, and no check looked at size or weight.
//
// The rules, per CSS rule that sets a font-size:
//   - the size is a type scale token, var(--font-size-*) defined on :root,
//     never a literal, so sizes are picked by role and cannot drift;
//   - nothing below MIN_PX, apart from the few entries in ALLOWED_BELOW_MIN;
//   - text below BODY_PX must be at least SMALL_TEXT_WEIGHT. Short labels and
//     data may be small if they are medium weight; sentences move up to the
//     body size instead.
// A font: shorthand may only reset the font (inherit and the other global
// keywords); a shorthand that sets a size would bypass the scale.
//
// The check is static: a rule that sets a font-size but no font-weight counts
// as regular (400), even if another rule on the same element sets the weight.
// Declare the weight next to the size. Relative tokens (em) scale their
// parent and cannot be resolved here, so they are skipped; keep them for
// inline code inside text that already passes.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const STYLES = fileURLToPath(new URL("../../static/styles.css", import.meta.url).href);

const MIN_PX = 12;
const BODY_PX = 13;
const SMALL_TEXT_WEIGHT = 500;
const ROOT_PX = 16;

// Selector to the smallest size it may use, with the reason it cannot meet
// MIN_PX. Each still has to meet the small-text weight.
const ALLOWED_BELOW_MIN = new Map<string, { px: number; why: string }>([
  [".notif-badge", { px: 11, why: "unread count inside a 16px dot on the header bell" }],
  [".meter-scale-track", { px: 11, why: "nine dBFS tick labels must fit across the narrowest meter" }],
]);

interface Rule {
  selector: string;
  line: number;
  // The declared value, for messages.
  value: string;
  // The resolved size in px, or null for a relative token or a bad value.
  px: number | null;
  // Why the value is not an acceptable size, if it is not.
  problem: string | null;
  weight: number | null;
}

const TOKEN_DECL = /(--font-size-[\w-]+)\s*:\s*([\d.]+)(px|rem|em)\s*;/g;
const GLOBAL_KEYWORDS = new Set(["inherit", "initial", "unset", "revert", "revert-layer"]);

// The type scale tokens: name to px, or null for a relative (em) token.
function scaleTokens(css: string): Map<string, number | null> {
  const tokens = new Map<string, number | null>();
  for (const m of css.matchAll(TOKEN_DECL)) {
    tokens.set(m[1], m[3] === "em" ? null : m[3] === "rem" ? Number(m[2]) * ROOT_PX : Number(m[2]));
  }
  return tokens;
}

function blankComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, (c) => c.replace(/[^\n]/g, " "));
}

// The value of the last declaration of prop in a block (the one that wins),
// with !important stripped. The last declaration may omit its semicolon.
function declared(body: string, prop: string): string | null {
  const re = new RegExp(`(?:^|[;\\s])${prop}\\s*:\\s*([^;]*?)\\s*(?:!important\\s*)?(?:;|$)`, "g");
  let value: string | null = null;
  for (const m of body.matchAll(re)) value = m[1];
  return value;
}

function resolve(value: string, tokens: Map<string, number | null>): Pick<Rule, "px" | "problem"> {
  const ref = /^var\(\s*(--font-size-[\w-]+)\s*\)$/.exec(value);
  if (!ref) {
    return { px: null, problem: `font-size ${value} is not a type scale token; use var(--font-size-*)` };
  }
  if (!tokens.has(ref[1])) {
    return { px: null, problem: `${ref[1]} is not defined on :root` };
  }
  return { px: tokens.get(ref[1]) ?? null, problem: null };
}

// With comments blanked (newlines kept, so line numbers stay true), every
// declaration block is the text between a brace pair with no brace inside.
// That finds rules nested in @media too, since the at-rule's own opening brace
// ends the selector scan.
function fontRules(source: string): Rule[] {
  const css = blankComments(source);
  const tokens = scaleTokens(css);
  const rules: Rule[] = [];
  const block = /([^{}]+)\{([^{}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = block.exec(css)) !== null) {
    const size = declared(m[2], "font-size");
    const shorthand = declared(m[2], "font");
    if (size === null && shorthand === null) continue;
    const weight = declared(m[2], "font-weight");
    const selectorStart = m.index + (m[1].length - m[1].trimStart().length);
    const base = {
      selector: m[1].trim().replace(/\s+/g, " "),
      line: css.slice(0, selectorStart).split("\n").length,
      weight: weight !== null && /^\d+$/.test(weight) ? Number(weight) : null,
    };
    if (shorthand !== null && !GLOBAL_KEYWORDS.has(shorthand)) {
      rules.push({
        ...base,
        value: shorthand,
        px: null,
        problem: `font: ${shorthand} sets the size outside the type scale; set font-size with a token`,
      });
    }
    if (size !== null) {
      rules.push({ ...base, value: size, ...resolve(size, tokens) });
    }
  }
  return rules;
}

function violations(rules: Rule[]): string[] {
  const out: string[] = [];
  for (const r of rules) {
    const where = `styles.css:${r.line} ${r.selector}`;
    if (r.problem !== null) {
      out.push(`${where}: ${r.problem}`);
      continue;
    }
    if (r.px === null) continue;
    const allowed = ALLOWED_BELOW_MIN.get(r.selector);
    const floor = allowed ? allowed.px : MIN_PX;
    if (r.px < floor) {
      out.push(`${where}: ${r.px}px is below the ${floor}px floor`);
    }
    const weight = r.weight ?? 400;
    if (r.px < BODY_PX && weight < SMALL_TEXT_WEIGHT) {
      out.push(
        `${where}: ${r.px}px at weight ${weight}${r.weight === null ? " (undeclared)" : ""}; ` +
          `text below ${BODY_PX}px needs weight ${SMALL_TEXT_WEIGHT}+, or use ${BODY_PX}px`,
      );
    }
  }
  return out;
}

const css = readFileSync(STYLES, "utf8");

test("stylesheet text meets the size and weight floor", () => {
  const rules = fontRules(css);
  // A parse regression that finds nothing would pass vacuously.
  assert.ok(rules.length > 50, `found only ${rules.length} rules with a font-size`);
  const failures = violations(rules);
  assert.deepEqual(failures, [], `\n  ${failures.join("\n  ")}\n`);
});

test("every below-floor allowance names a rule that still exists", () => {
  const selectors = new Set(fontRules(css).map((r) => r.selector));
  const stale = [...ALLOWED_BELOW_MIN.keys()].filter((s) => !selectors.has(s));
  assert.deepEqual(stale, []);
});

test("the type scale is defined in rem, except the relative code size", () => {
  const tokens = scaleTokens(blankComments(css));
  assert.ok(tokens.size >= 5, `found only ${tokens.size} --font-size-* tokens`);
  // px tokens would ignore the browser font-size setting.
  const px = [...blankComments(css).matchAll(TOKEN_DECL)].filter((m) => m[3] === "px").map((m) => m[1]);
  assert.deepEqual(px, []);
  assert.equal(tokens.get("--font-size-caption"), MIN_PX);
  assert.equal(tokens.get("--font-size-body"), BODY_PX);
});

test("the checker flags literals, unknown tokens, shorthands, and small or thin text", () => {
  const sample = `
    :root { --font-size-tiny: 0.625rem; --font-size-micro: 0.6875rem; --font-size-small: 0.75rem; --font-size-body: 13px; --font-size-code: 0.9em; }
    .a { font-size: var(--font-size-tiny); font-weight: 600; }
    .b { font-size: var(--font-size-small); color: red; }
    .c { font-size: var(--font-size-small); font-weight: 400; }
    .d { font-size: var(--font-size-small); font-weight: 500; }
    @media (max-width: 600px) { .e { font-size: var(--font-size-tiny); font-weight: 500; } }
    /* .f { font-size: 8px; } */
    .g { color: red; font-size: var(--font-size-tiny) }
    .h { font-size: var(--font-size-small) !important; font-weight: 400 }
    .i { font-size: 13px; }
    .j { font-size: 0.9em; }
    .k { font-size: var(--font-size-huge); }
    .l { font: 600 12px/1.2 var(--font-sans); }
    .m { font-size: 12px; font-size: var(--font-size-body); }
    .ok4 { font-weight: 500; font-size: var(--font-size-small) }
    .ok1 { font-size: var(--font-size-small); font-weight: 500; }
    .ok2 { font-size: var(--font-size-body); }
    .ok3 { font-size: var(--font-size-code); }
    .ok5 { font: inherit; }
    .notif-badge { font-size: var(--font-size-micro); font-weight: 700; }
  `;
  const got = violations(fontRules(sample)).map((v) => v.split(" ")[1].replace(":", ""));
  assert.deepEqual(got, [".a", ".b", ".c", ".e", ".g", ".g", ".h", ".i", ".j", ".k", ".l"]);
});
