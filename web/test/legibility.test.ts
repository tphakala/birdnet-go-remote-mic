// Type legibility floor for web/static/styles.css.
//
// Why this exists: contrast.test.ts measures colour pairs, but a pair that
// passes can still be hard to read when the text is tiny and thin. The UI had
// drifted to 10-11px captions and 12px regular-weight subtitles in the muted
// colour, each size picked locally, and no check looked at size or weight.
//
// The rule, per CSS rule that sets a font-size:
//   - nothing below MIN_PX, apart from the few entries in ALLOWED_BELOW_MIN;
//   - text below BODY_PX must be at least SMALL_TEXT_WEIGHT. Short labels and
//     data may be small if they are medium weight; sentences move up to the
//     body size instead.
//
// The check is static: a rule that sets a font-size but no font-weight counts
// as regular (400), even if another rule on the same element sets the weight.
// Declare the weight next to the size. Relative sizes (em, %) scale their
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
  px: number;
  weight: number | null;
}

// With comments blanked (newlines kept, so line numbers stay true), every
// declaration block is the text between a brace pair with no brace inside.
// That finds rules nested in @media too, since the at-rule's own opening brace
// ends the selector scan.
function fontRules(source: string): Rule[] {
  const css = source.replace(/\/\*[\s\S]*?\*\//g, (c) => c.replace(/[^\n]/g, " "));
  const rules: Rule[] = [];
  const block = /([^{}]+)\{([^{}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = block.exec(css)) !== null) {
    const size = /(?:^|[;\s])font-size\s*:\s*([\d.]+)(px|rem)\s*;/.exec(m[2]);
    if (!size) continue;
    const px = size[2] === "rem" ? Number(size[1]) * ROOT_PX : Number(size[1]);
    const weight = /(?:^|[;\s])font-weight\s*:\s*(\d+)\s*;/.exec(m[2]);
    const selectorStart = m.index + (m[1].length - m[1].trimStart().length);
    rules.push({
      selector: m[1].trim().replace(/\s+/g, " "),
      line: css.slice(0, selectorStart).split("\n").length,
      px,
      weight: weight ? Number(weight[1]) : null,
    });
  }
  return rules;
}

function violations(rules: Rule[]): string[] {
  const out: string[] = [];
  for (const r of rules) {
    const where = `styles.css:${r.line} ${r.selector}`;
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

test("the checker flags small, thin and undeclared-weight text", () => {
  const sample = `
    .a { font-size: 10px; font-weight: 600; }
    .b { font-size: 12px; color: red; }
    .c { font-size: 12px; font-weight: 400; }
    .d { font-size: 0.75rem; font-weight: 500; }
    @media (max-width: 600px) { .e { font-size: 11px; font-weight: 500; } }
    /* .f { font-size: 8px; } */
    .ok1 { font-size: 12px; font-weight: 500; }
    .ok2 { font-size: 13px; }
    .ok3 { font-size: 0.9em; }
    .notif-badge { font-size: 11px; font-weight: 700; }
  `;
  const got = violations(fontRules(sample)).map((v) => v.split(" ")[1].replace(":", ""));
  assert.deepEqual(got, [".a", ".b", ".c", ".e"]);
});
