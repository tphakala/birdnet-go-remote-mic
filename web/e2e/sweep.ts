// Rendered UI sweep: drives Chromium through every view of the compiled web UI,
// served by the mock API in mock-server.ts, and checks what only a rendered page
// can show:
//
//   - composited text contrast (WCAG AA, plus the SMALL_TEXT floor for the
//     secondary and muted text tokens), measured on the colours the browser
//     actually computes rather than on token pairs (test/contrast.test.ts);
//   - no horizontal page overflow at a phone and a desktop width;
//   - text that grows with the browser's default font size (compared with the
//     same element at the stock 16 px);
//   - meter rows that keep their height while the live levels change.
//
// It runs every combination of theme, viewport width and browser default font
// size, over each view plus the open notification panel, theme menu and error
// toast, prints a report grouped by combination and then by finding, and exits
// non-zero on any failure.
//
//   task web:sweep
//   npx -p playwright@1.59.0 node web/e2e/sweep.ts <dist dir> [flags]
//
// Flags: --only-view <dashboard|events|system|about>, --only-theme
// <light|dark>, --only-width <px>, --only-font <px>, --headed, and
// --keep-open (leave the headed browser and mock server up after the report
// until Ctrl-C).
//
// Playwright is not a dependency of this repository: the task runs this file
// under `npx -p playwright@<version>`, which puts the package's bin directory
// on PATH, and loadPlaywright resolves the package from there.
//
// Known limits, reported rather than hidden: text drawn by ::before/::after
// content, placeholders and canvas is not checked; a background is resolved
// through the element's ancestors only, so translucent text positioned over an
// unrelated sibling is measured against its ancestors' ground; url() background
// images are treated as transparent and counted; text under a filter, a blend
// mode or a backdrop filter showing through a translucent background is not
// modelled, and is skipped and counted.

import { existsSync } from "node:fs";
import { delimiter, dirname, join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

import { startMockServer } from "./mock-server.ts";
import type { Browser, BrowserContext, Page, Playwright } from "./playwright.d.ts";

const PLAYWRIGHT_VERSION = "1.59.0";
// theme-init.ts reads this key before the first paint; web/src/lib/theme.ts
// THEME_KEY holds the same value.
const THEME_KEY = "remote-mic-theme";

const THEMES = ["light", "dark"] as const;
const WIDTHS = [320, 1280] as const;
// Browser default font sizes: 16 px is the stock setting, 20 and 24 px are the
// "Large" and "Very large" settings a low-vision operator picks.
const FONT_SIZES = [16, 20, 24] as const;
const VIEWS = ["dashboard", "events", "system", "about"] as const;
type ViewName = (typeof VIEWS)[number];

// The selector each view renders once its data arrived, so a check never runs
// against a loading placeholder.
const VIEW_READY: Record<ViewName, string> = {
  dashboard: "#view-dashboard .meter-track-wrapper",
  events: "#view-events .ev-list",
  system: "#sys-info-card:not([hidden]) .info-grid dd",
  about: "#view-about .about-body",
};

// WCAG 2.1 SC 1.4.3, and the stricter floor test/contrast.test.ts holds the
// secondary and muted text tokens to.
const AA = 4.5;
const AA_LARGE = 3;
const SMALL_TEXT = 5.5;
const SMALL_TEXT_TOKENS = ["--text-secondary", "--text-muted"];

// A meter row may not change height as the readout moves through every value.
// SC 1.4.4 (Resize text): with a larger browser default font size, text must
// grow in proportion. A text element fails when it reaches less than this share
// of the growth the setting asks for (at 24 px, 1.5x its 16 px size).
const SCALE_TOLERANCE = 0.95;

// Meter rows are sampled for at least one full level cycle of the mock (4 s),
// and on until every row has shown its narrowest (-inf) and widest (-xx.x dBFS)
// readout, up to two cycles.
const STABILITY_MIN_MS = 4300;
const STABILITY_MAX_MS = 8600;

// ---------------------------------------------------------------------------
// Findings and report

type Kind = "contrast" | "overflow" | "stability" | "scaling" | "render" | "js-error";

interface Finding {
  kind: Kind;
  // Where it happened: the view or open state.
  where: string;
  // What: a stable description used to merge the same finding across combos.
  key: string;
  detail: string;
}

interface Combo {
  theme: (typeof THEMES)[number];
  width: number;
  font: number;
}

function comboName(c: Combo): string {
  return `${c.theme} ${c.width}px font ${c.font}px`;
}

interface ContrastHit {
  selector: string;
  text: string;
  fg: string;
  bg: string;
  ratio: number;
  min: number;
  floor: string;
  fontSize: number;
  fontWeight: number;
  ariaHidden: boolean;
}

interface ContrastResult {
  checked: number;
  hits: ContrastHit[];
  // The computed font size of every checked text element, keyed by its
  // selector and text, for the text-scaling comparison across font sizes.
  sizes: Array<[string, number]>;
  skipped: Record<string, number>;
}

interface OverflowResult {
  scrollWidth: number;
  clientWidth: number;
  offenders: Array<{ selector: string; right: number; width: number }>;
}

interface StabilityResult {
  // heights pairs each distinct row height with the first readout seen at it.
  rows: Array<{ selector: string; heights: Array<[number, string]>; readouts: string[] }>;
  cards: Array<{ selector: string; heights: number[] }>;
}

// ---------------------------------------------------------------------------
// In-page audits. page.evaluate serializes each function to source and runs it
// in the page, so they must not reference anything outside their own bodies.

function auditContrast(opts: { scope: string | null; smallTokens: string[]; aa: number; aaLarge: number; smallText: number }): ContrastResult {
  interface RGBA { r: number; g: number; b: number; a: number }
  const TRANSPARENT: RGBA = { r: 0, g: 0, b: 0, a: 0 };
  const skipped: Record<string, number> = {};
  const skip = (why: string): void => {
    skipped[why] = (skipped[why] ?? 0) + 1;
  };

  // Parses the forms Chromium's computed style returns: rgb()/rgba() in either
  // comma or space syntax, and color(srgb ...) for colours it keeps in a wider
  // form. Channels are returned straight (not premultiplied).
  function parse(raw: string): RGBA | null {
    const s = raw.trim();
    if (s === "transparent") return TRANSPARENT;
    let m = /^rgba?\(([^)]+)\)$/.exec(s);
    if (m) {
      const [r, g, b, a = 1] = (m[1] ?? "").split(/[\s,/]+/).filter(Boolean).map(Number);
      if (r === undefined || g === undefined || b === undefined) return null;
      return { r, g, b, a };
    }
    m = /^color\(srgb ([^)]+)\)$/.exec(s);
    if (m) {
      const [r, g, b, a = 1] = (m[1] ?? "").split(/[\s/]+/).filter(Boolean).map(Number);
      if (r === undefined || g === undefined || b === undefined) return null;
      return { r: r * 255, g: g * 255, b: b * 255, a };
    }
    return null;
  }
  const hex = (c: RGBA): string => {
    const h = (v: number): string => Math.round(Math.min(255, Math.max(0, v))).toString(16).padStart(2, "0");
    return `#${h(c.r)}${h(c.g)}${h(c.b)}${c.a < 0.999 ? h(c.a * 255) : ""}`;
  };

  // Source-over on premultiplied colour, so group opacity composes exactly.
  interface Pre { r: number; g: number; b: number; a: number }
  const pre = (c: RGBA): Pre => ({ r: c.r * c.a, g: c.g * c.a, b: c.b * c.a, a: c.a });
  const over = (top: Pre, under: Pre): Pre => ({
    r: top.r + under.r * (1 - top.a),
    g: top.g + under.g * (1 - top.a),
    b: top.b + under.b * (1 - top.a),
    a: top.a + under.a * (1 - top.a),
  });
  const scale = (c: Pre, k: number): Pre => ({ r: c.r * k, g: c.g * k, b: c.b * k, a: c.a * k });
  const unpre = (c: Pre): RGBA => (c.a <= 0 ? { r: 0, g: 0, b: 0, a: 0 } : { r: c.r / c.a, g: c.g / c.a, b: c.b / c.a, a: c.a });

  const lum = (c: RGBA): number => {
    const ch = (v: number): number => {
      const s = v / 255;
      return s <= 0.04045 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
    };
    return 0.2126 * ch(c.r) + 0.7152 * ch(c.g) + 0.0722 * ch(c.b);
  };
  const ratio = (a: RGBA, b: RGBA): number => {
    const la = lum(a);
    const lb = lum(b);
    return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
  };

  // Splits a computed background-image list at its top-level commas.
  function layers(bgImage: string): string[] {
    if (bgImage === "none") return [];
    const out: string[] = [];
    let depth = 0;
    let start = 0;
    for (let i = 0; i < bgImage.length; i++) {
      const ch = bgImage[i];
      if (ch === "(") depth++;
      else if (ch === ")") depth--;
      else if (ch === "," && depth === 0) {
        out.push(bgImage.slice(start, i).trim());
        start = i + 1;
      }
    }
    out.push(bgImage.slice(start).trim());
    return out;
  }

  // Every paint layer of one element as a set of candidate colours in paint
  // order (bottom first): the background colour, then each background image
  // from the last listed (bottom) to the first (top). A gradient whose stops are
  // all equal is a solid layer; any other gradient contributes each stop as a
  // candidate, and the audit keeps the worst.
  let imageBackgrounds = 0;
  function paintLayers(cs: CSSStyleDeclaration): RGBA[][] {
    const out: RGBA[][] = [];
    const bg = parse(cs.backgroundColor);
    if (bg && bg.a > 0) out.push([bg]);
    for (const layer of layers(cs.backgroundImage).reverse()) {
      if (layer.includes("gradient(")) {
        const stops = (layer.match(/rgba?\([^)]*\)|color\(srgb[^)]*\)/g) ?? [])
          .map(parse)
          .filter((c): c is RGBA => c !== null);
        const unique = stops.filter((c, i) => stops.findIndex((d) => hex(d) === hex(c)) === i);
        if (unique.length > 0) out.push(unique);
      } else if (layer.startsWith("url(")) {
        imageBackgrounds++;
      }
    }
    return out;
  }

  const probeColor = (token: string): string | null => {
    if (!getComputedStyle(document.documentElement).getPropertyValue(token).trim()) return null;
    const probe = document.createElement("span");
    probe.style.color = `var(${token})`;
    document.body.appendChild(probe);
    const c = getComputedStyle(probe).color;
    probe.remove();
    return c;
  };
  const smallColors = new Set(opts.smallTokens.map(probeColor).filter((c): c is string => c !== null));
  const smallNames = new Map(opts.smallTokens.map((t) => [probeColor(t), t] as const));

  // looping reports an animation that repeats for ever, as a pulse does.
  function looping(a: Animation): boolean {
    return a.effect?.getComputedTiming().iterations === Infinity;
  }

  // Settle every animation first: finish the one-shot ones, so an entrance
  // fade is read at its end, and hold the looping ones at their start.
  for (const a of document.getAnimations()) {
    if (looping(a)) {
      a.pause();
      a.currentTime = 0;
    } else {
      try {
        a.finish();
      } catch {
        // An infinite effect cannot finish; looping() caught those already.
      }
    }
  }

  // lowestOpacity is the dimmest opacity el reaches: its current one, or the
  // lowest keyframe opacity of a looping animation on it.
  function lowestOpacity(el: Element, current: number): number {
    let low = current;
    for (const a of el.getAnimations()) {
      if (!looping(a)) continue;
      const effect = a.effect as KeyframeEffect | null;
      for (const k of effect?.getKeyframes() ?? []) {
        const o = Number(k.opacity);
        if (k.opacity !== undefined && Number.isFinite(o)) low = Math.min(low, o);
      }
    }
    return low;
  }

  function describe(el: Element): string {
    const one = (e: Element): string => {
      let s = e.tagName.toLowerCase();
      if (e.id) return `${s}#${e.id}`;
      const cls = [...e.classList].slice(0, 2);
      if (cls.length) s += "." + cls.join(".");
      return s;
    };
    const parts: string[] = [];
    let cur: Element | null = el;
    while (cur && cur !== document.body && parts.length < 3) {
      parts.unshift(one(cur));
      if (cur.id) break;
      cur = cur.parentElement;
    }
    return parts.join(" > ");
  }

  function visible(el: Element): boolean {
    const opts2 = { opacityProperty: true, visibilityProperty: true, checkOpacity: true, checkVisibilityCSS: true };
    if (!el.checkVisibility(opts2)) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }

  const scopeRoot = opts.scope ? document.querySelector(opts.scope) : document.body;
  const hits: ContrastHit[] = [];
  const sizes: Array<[string, number]> = [];
  let checked = 0;
  if (!scopeRoot) return { checked, hits, sizes, skipped: { "scope not found": 1 } };

  // The canvas under the root: white unless the page paints its own, which
  // every theme here does on html or body.
  const canvas: Pre = pre({ r: 255, g: 255, b: 255, a: 1 });

  for (const el of scopeRoot.querySelectorAll<HTMLElement>("*")) {
    let text = "";
    const tag = el.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA") {
      const input = el as HTMLInputElement;
      if (["checkbox", "radio", "range", "color", "hidden", "file"].includes(input.type)) continue;
      text = input.type === "password" ? "•".repeat(input.value.length) : input.value;
      if (!text && input.placeholder) {
        skip("placeholder (not checked)");
        continue;
      }
    } else if (tag === "SELECT") {
      const sel = el as HTMLSelectElement;
      text = sel.selectedOptions[0]?.textContent ?? "";
    } else if (tag === "SCRIPT" || tag === "STYLE" || tag === "OPTION" || tag === "svg") {
      continue;
    } else {
      for (const n of el.childNodes) if (n.nodeType === Node.TEXT_NODE) text += n.textContent ?? "";
    }
    text = text.trim();
    if (!text) continue;

    if (el.closest(".visually-hidden")) {
      skip("visually hidden");
      continue;
    }
    if (!visible(el)) continue;
    const rect = el.getBoundingClientRect();
    if (rect.width <= 1 && rect.height <= 1) {
      skip("visually hidden");
      continue;
    }
    if (el.closest(":disabled, [aria-disabled='true']")) {
      skip("disabled control");
      continue;
    }
    // Glyph-only text (a bullet separator, an arrow) carries no words; SC 1.4.3
    // covers text, and these are decorative.
    if (!/[\p{L}\p{N}]/u.test(text)) {
      skip("decorative glyph");
      continue;
    }

    const path: HTMLElement[] = [];
    for (let cur: HTMLElement | null = el; cur; cur = cur.parentElement) path.unshift(cur);
    const styles = path.map((e) => getComputedStyle(e));
    // A filter or blend mode on the text or anything under it, or a backdrop
    // filter with no opaque background between it and the text, changes the
    // colours this model composites; such text is skipped and counted, never
    // passed. A backdrop filter under an opaque layer (the modal card over the
    // blurred scrim, the action bar's own background) cannot change the text's
    // contrast.
    let unmodelled = false;
    let opaqueAbove = false;
    for (let i = styles.length - 1; i >= 0 && !unmodelled; i--) {
      const s = styles[i];
      if (!s) continue;
      const opaque = (parse(s.backgroundColor)?.a ?? 0) >= 1 && Number(s.opacity) >= 1;
      if (s.filter !== "none" || s.mixBlendMode !== "normal") unmodelled = true;
      else if (s.backdropFilter && s.backdropFilter !== "none" && !opaqueAbove && !opaque) unmodelled = true;
      opaqueAbove ||= opaque;
    }
    if (unmodelled) {
      skip("filter, blend mode or translucent backdrop filter (not modelled)");
      continue;
    }
    const paint = styles.map(paintLayers);
    const opacity = styles.map((s, i) => {
      const e = path[i];
      return e ? lowestOpacity(e, Number(s.opacity)) : Number(s.opacity);
    });

    const cs = styles.at(-1);
    if (!cs) continue;
    const fgRaw = parse(cs.color);
    if (!fgRaw) {
      skip(`unparsed colour ${cs.color}`);
      continue;
    }
    checked++;

    // Enumerate one candidate per gradient layer (the first 64 combinations;
    // the UI paints at most one gradient under any text today), render the
    // path's groups with and without the text on top, and keep the worst ratio.
    const slots: RGBA[][] = paint.flat();
    let combos = 1;
    for (const s of slots) combos *= s.length;
    let worst = Infinity;
    let worstFg: RGBA = fgRaw;
    let worstBg: RGBA = fgRaw;
    const total = Math.min(combos, 64);
    for (let k = 0; k < total; k++) {
      let rem = k;
      const choice = slots.map((s) => {
        // The fallback never applies: an empty slot makes total 0.
        const c = s[rem % s.length] ?? TRANSPARENT;
        rem = Math.floor(rem / s.length);
        return c;
      });
      let idx = 0;
      const chosen = paint.map((lv) => lv.map(() => choice[idx++] ?? TRANSPARENT));
      const render = (i: number, withText: boolean): Pre => {
        let acc: Pre = pre(TRANSPARENT);
        for (const c of chosen[i] ?? []) acc = over(pre(c), acc);
        if (i < path.length - 1) acc = over(render(i + 1, withText), acc);
        else if (withText) acc = over(pre(fgRaw), acc);
        return scale(acc, opacity[i] ?? 1);
      };
      const bg = unpre(over(render(0, false), canvas));
      const fg = unpre(over(render(0, true), canvas));
      const r = ratio(fg, bg);
      if (r < worst) {
        worst = r;
        worstFg = fg;
        worstBg = bg;
      }
    }

    const size = parseFloat(cs.fontSize);
    sizes.push([`${describe(el)} "${text.slice(0, 32)}"`, size]);
    const weight = Number(cs.fontWeight) || 400;
    const large = size >= 24 || (size >= 18.66 && weight >= 700);
    const small = smallColors.has(cs.color);
    const min = small ? Math.max(opts.smallText, large ? opts.aaLarge : opts.aa) : large ? opts.aaLarge : opts.aa;
    const floor = small ? `SMALL_TEXT (${smallNames.get(cs.color)})` : large ? "AA large" : "AA";
    if (worst + 1e-6 < min) {
      hits.push({
        selector: describe(el),
        text: text.length > 48 ? text.slice(0, 45) + "..." : text,
        fg: hex(worstFg),
        bg: hex(worstBg),
        ratio: Math.round(worst * 100) / 100,
        min,
        floor,
        fontSize: size,
        fontWeight: weight,
        ariaHidden: el.closest("[aria-hidden='true']") !== null,
      });
    }
  }
  if (imageBackgrounds) skipped["url() background layer (treated as transparent)"] = imageBackgrounds;
  return { checked, hits, sizes, skipped };
}

function auditOverflow(): OverflowResult {
  const root = document.scrollingElement ?? document.documentElement;
  const vw = root.clientWidth;
  const describe = (el: Element): string => {
    const parts: string[] = [];
    let cur: Element | null = el;
    while (cur && cur !== document.body && parts.length < 3) {
      let s = cur.tagName.toLowerCase();
      if (cur.id) {
        parts.unshift(`${s}#${cur.id}`);
        break;
      }
      const cls = [...cur.classList].slice(0, 2);
      if (cls.length) s += "." + cls.join(".");
      parts.unshift(s);
      cur = cur.parentElement;
    }
    return parts.join(" > ");
  };
  // Content inside its own horizontal scroller (a wide table in .table-scroll)
  // does not widen the page, so it is not an offender.
  const clipped = (el: Element): boolean => {
    for (let cur = el.parentElement; cur && cur !== document.body && cur !== document.documentElement; cur = cur.parentElement) {
      const ox = getComputedStyle(cur).overflowX;
      if (ox !== "visible") return true;
    }
    return false;
  };
  const offenders: OverflowResult["offenders"] = [];
  for (const el of document.body.querySelectorAll("*")) {
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.right <= vw + 1 || clipped(el)) continue;
    // Report the element that sticks out of its parent, not every ancestor it
    // drags along.
    const parent = el.parentElement;
    if (parent && parent !== document.body && parent.getBoundingClientRect().right > vw + 1) continue;
    offenders.push({ selector: describe(el), right: Math.round(r.right), width: Math.round(r.width) });
  }
  offenders.sort((a, b) => b.right - a.right);
  return { scrollWidth: root.scrollWidth, clientWidth: vw, offenders: offenders.slice(0, 8) };
}

function sampleStability(opts: { minMs: number; maxMs: number }): Promise<StabilityResult> {
  const rows = [...document.querySelectorAll<HTMLElement>("#view-dashboard .meter-track-wrapper")];
  const cards = [...new Set(rows.map((r) => r.closest("article")).filter((a): a is HTMLElement => a !== null))];
  const label = (el: HTMLElement, i: number): string => {
    const card = el.closest("article");
    const name = card?.querySelector("h3, .device-name, [class*='title']")?.textContent?.trim() ?? `card`;
    return el.tagName === "ARTICLE" ? `card "${name}"` : `card "${name}" meter row ${i + 1}`;
  };
  // Each row's heights (with the readout first seen at each) and readouts,
  // and each card's heights, kept beside the element they sample.
  const rowSeen = rows.map((el) => ({ el, heights: new Map<number, string>(), readouts: new Set<string>() }));
  const cardSeen = cards.map((el) => ({ el, heights: new Set<number>() }));
  return new Promise((done) => {
    const t0 = performance.now();
    const tick = (): void => {
      for (const r of rowSeen) {
        const h = Math.round(r.el.getBoundingClientRect().height * 10) / 10;
        const ro = r.el.querySelector(".db-readout")?.textContent ?? "";
        if (!r.heights.has(h)) r.heights.set(h, ro);
        if (ro) r.readouts.add(ro);
      }
      for (const c of cardSeen) c.heights.add(Math.round(c.el.getBoundingClientRect().height * 10) / 10);
      const elapsed = performance.now() - t0;
      const covered = rowSeen.every(({ readouts }) => readouts.has("-inf") && [...readouts].some((t) => /^-\d\d\.\d dBFS$/.test(t)));
      if (elapsed < opts.minMs || (!covered && elapsed < opts.maxMs)) setTimeout(tick, 40);
      else {
        const perCard = new Map<HTMLElement, number>();
        done({
          rows: rowSeen.map((r) => {
            const card = r.el.closest("article") as HTMLElement;
            const n = perCard.get(card) ?? 0;
            perCard.set(card, n + 1);
            return { selector: label(r.el, n), heights: [...r.heights], readouts: [...r.readouts] };
          }),
          cards: cardSeen.map((c) => ({ selector: label(c.el, 0), heights: [...c.heights] })),
        });
      }
    };
    tick();
  });
}

// ---------------------------------------------------------------------------
// Driver

interface Flags {
  dist: string;
  onlyView: ViewName | null;
  onlyTheme: string | null;
  onlyWidth: number | null;
  onlyFont: number | null;
  headed: boolean;
  keepOpen: boolean;
}

// oneOf returns v when it is one of the allowed values and throws otherwise, so
// a mistyped filter fails the run instead of sweeping nothing and passing.
function oneOf(flag: string, v: string, allowed: readonly string[]): string {
  if (!allowed.includes(v)) throw new Error(`${flag} must be one of ${allowed.join(", ")}`);
  return v;
}

function parseFlags(argv: string[]): Flags {
  const f: Flags = { dist: "", onlyView: null, onlyTheme: null, onlyWidth: null, onlyFont: null, headed: false, keepOpen: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === undefined) break;
    const val = (): string => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`${a} needs a value`);
      return v;
    };
    if (a === "--only-view") {
      const v = val();
      if (!(VIEWS as readonly string[]).includes(v)) throw new Error(`--only-view must be one of ${VIEWS.join(", ")}`);
      f.onlyView = v as ViewName;
    } else if (a === "--only-theme") f.onlyTheme = oneOf(a, val(), THEMES);
    else if (a === "--only-width") f.onlyWidth = Number(oneOf(a, val(), WIDTHS.map(String)));
    else if (a === "--only-font") f.onlyFont = Number(oneOf(a, val(), FONT_SIZES.map(String)));
    else if (a === "--headed") f.headed = true;
    else if (a === "--keep-open") f.keepOpen = f.headed = true;
    else if (a.startsWith("--")) throw new Error(`unknown flag ${a}`);
    else f.dist = a;
  }
  if (!f.dist) throw new Error("usage: node web/e2e/sweep.ts <dist dir> [--only-view v] [--only-theme t] [--only-width px] [--only-font px] [--headed] [--keep-open]");
  return f;
}

// loadPlaywright finds the package `npx -p playwright@<version>` installed: npx
// prepends its node_modules/.bin to PATH, and the package sits beside it.
async function loadPlaywright(): Promise<Playwright> {
  for (const dir of (process.env.PATH ?? "").split(delimiter)) {
    if (!dir.endsWith(join("node_modules", ".bin"))) continue;
    const entry = join(dirname(dir), "playwright", "index.mjs");
    if (existsSync(entry)) return (await import(pathToFileURL(entry).href)) as Playwright;
  }
  throw new Error(`playwright not found on PATH; run under: npx -p playwright@${PLAYWRIGHT_VERSION} node web/e2e/sweep.ts <dist dir>`);
}

// setDefaultFontSize changes the browser's default font size the way the
// settings page does (Chromium's font-size preference, over CDP), so it moves
// `medium`, rem and em exactly as an operator's setting would. If CDP refuses,
// it falls back to a root font-size rule, which cannot reproduce a stylesheet
// that sets its own px root size.
async function setDefaultFontSize(context: BrowserContext, page: Page, px: number): Promise<string> {
  if (px === 16) return "default";
  try {
    const cdp = await context.newCDPSession(page);
    await cdp.send("Page.setFontSizes", { fontSizes: { standard: px, fixed: Math.round((px * 13) / 16) } });
    return "cdp";
  } catch {
    await page.addInitScript((size: number) => {
      // Init scripts run before the document has a root element.
      document.addEventListener("DOMContentLoaded", () => {
        const style = document.createElement("style");
        style.textContent = `html{font-size:${size}px}`;
        document.head.appendChild(style);
      });
    }, px);
    return "root style";
  }
}

async function run(): Promise<number> {
  const flags = parseFlags(process.argv.slice(2));
  const dist = resolve(flags.dist);
  if (!existsSync(join(dist, "index.html")) || !existsSync(join(dist, "app.js"))) {
    throw new Error(`${dist} is not a built UI (index.html and app.js expected)`);
  }
  const pw = await loadPlaywright();
  // Port 0 lets the OS pick a free port, so a sweep never collides with a
  // mock server someone left running on the default port.
  const server = await startMockServer(dist, 0);
  let browser: Browser | null = null;
  try {
    browser = await pw.chromium.launch({ headless: !flags.headed });
    return await sweep(flags, browser, server.url);
  } finally {
    // A failure part way (a control that never appeared) must not leave the
    // browser and the mock's timers keeping the process alive, and a browser
    // that fails to close must not keep the mock open either.
    try {
      await browser?.close();
    } finally {
      await server.close();
    }
  }
}

async function sweep(flags: Flags, browser: Browser, serverUrl: string): Promise<number> {

  const combos: Combo[] = [];
  for (const theme of THEMES) {
    if (flags.onlyTheme && theme !== flags.onlyTheme) continue;
    for (const width of WIDTHS) {
      if (flags.onlyWidth && width !== flags.onlyWidth) continue;
      for (const font of FONT_SIZES) {
        if (flags.onlyFont && font !== flags.onlyFont) continue;
        combos.push({ theme, width, font });
      }
    }
  }
  if (combos.length === 0) throw new Error("the filters leave no combination to sweep");
  const views = flags.onlyView ? [flags.onlyView] : [...VIEWS];

  const byCombo = new Map<string, Finding[]>();
  // Text sizes at the stock 16 px default, per theme, width and view, which the
  // larger font sizes are compared against. The font size loop runs 16 px first.
  const baseSizes = new Map<string, Map<string, number>>();
  const skippedTotals: Record<string, number> = {};
  // Font sizes swept without a 16 px run to compare against (an --only-font
  // filter), whose scaling therefore went unchecked.
  const unscaled = new Set<string>();
  let checkedTotal = 0;
  const fontMethods = new Set<string>();
  const t0 = Date.now();

  for (const combo of combos) {
    const name = comboName(combo);
    const findings: Finding[] = [];
    byCombo.set(name, findings);
    process.stderr.write(`sweep: ${name}\n`);

    const context = await browser.newContext({
      viewport: { width: combo.width, height: 900 },
      colorScheme: combo.theme,
    });
    // Save the theme before theme-init.ts runs, as an operator's choice would be.
    await context.addInitScript(
      (args: { key: string; theme: string }) => {
        try {
          localStorage.setItem(args.key, args.theme);
        } catch {
          // Storage blocked: colorScheme above still selects the same theme.
        }
      },
      { key: THEME_KEY, theme: combo.theme },
    );
    const page = await context.newPage();
    page.on("pageerror", (err: Error) => {
      findings.push({ kind: "js-error", where: "page", key: `uncaught: ${err.message}`, detail: err.message });
    });
    fontMethods.add(await setDefaultFontSize(context, page, combo.font));
    // Transitions are off, so a check never reads a fade half way. The
    // contrast audit settles CSS animations itself: a one-shot one (an
    // entrance) is finished, and a looping one (a pulse) is judged at its
    // lowest keyframe opacity. Reduced motion is deliberately not emulated: it
    // also stops the meters' animation loop, whose peak-hold ballistics the
    // stability check depends on.
    await page.addInitScript(() => {
      document.addEventListener("DOMContentLoaded", () => {
        const style = document.createElement("style");
        style.textContent = "*,*::before,*::after{transition:none!important}";
        document.head.appendChild(style);
      });
    });

    const check = async (where: string, scope: string | null): Promise<void> => {
      const c = await page.evaluate(auditContrast, { scope, smallTokens: SMALL_TEXT_TOKENS, aa: AA, aaLarge: AA_LARGE, smallText: SMALL_TEXT });
      checkedTotal += c.checked;
      // A check that looked at nothing is a failure, not a pass: the scope
      // never rendered, or the page came up empty.
      if (c.skipped["scope not found"] || c.checked === 0) {
        findings.push({ kind: "render", where, key: `${where}: nothing checked`, detail: `${scope ?? "page"} had no text to check` });
      }
      for (const [why, n] of Object.entries(c.skipped)) skippedTotals[why] = (skippedTotals[why] ?? 0) + n;
      for (const h of c.hits) {
        findings.push({
          kind: "contrast",
          where,
          key: `${h.selector} "${h.text}" ${h.fg} on ${h.bg}`,
          detail: `${h.ratio}:1 < ${h.min}:1 (${h.floor}) ${h.selector} "${h.text}" ${h.fg} on ${h.bg}, ${h.fontSize}px/${h.fontWeight}${h.ariaHidden ? ", aria-hidden" : ""}`,
        });
      }
      const base = `${combo.theme}|${combo.width}|${where}`;
      if (combo.font === 16) {
        baseSizes.set(base, new Map(c.sizes));
      } else if (!baseSizes.has(base)) {
        unscaled.add(`${combo.font}px`);
      } else {
        const want = combo.font / 16;
        const baseline = baseSizes.get(base) as Map<string, number>;
        let compared = 0;
        const stuck: string[] = [];
        for (const [key, size] of c.sizes) {
          const was = baseline.get(key);
          if (was === undefined) continue;
          compared++;
          if (size < was * want * SCALE_TOLERANCE) stuck.push(`${key} ${was}px -> ${size}px`);
        }
        if (compared === 0) {
          findings.push({
            kind: "scaling",
            where,
            key: `${where}: nothing to compare`,
            detail: `no text element matched the ${combo.font}px run against the 16px one`,
          });
        }
        if (stuck.length) {
          findings.push({
            kind: "scaling",
            where,
            key: `${where}: text does not follow the browser font size`,
            detail: `${stuck.length} of ${compared} text elements did not grow ${want}x with the ${combo.font}px default, e.g. ${stuck.slice(0, 3).join("; ")}`,
          });
        }
      }
      const o = await page.evaluate(auditOverflow);
      if (o.scrollWidth > o.clientWidth + 1) {
        const list = o.offenders.map((x) => `${x.selector} (right ${x.right}px, width ${x.width}px)`).join("; ");
        findings.push({
          kind: "overflow",
          where,
          key: o.offenders[0]?.selector ?? "page",
          detail: `page scrollWidth ${o.scrollWidth}px > clientWidth ${o.clientWidth}px; widest: ${list || "none found outside scrollers"}`,
        });
      }
    };

    const ready = async (view: ViewName): Promise<boolean> => {
      try {
        await page.waitForSelector(VIEW_READY[view], { state: "attached", timeout: 8000 });
        await page.evaluate(() => document.fonts.ready.then(() => undefined));
        // Let one more poll and a few level frames land.
        await page.waitForTimeout(400);
        return true;
      } catch {
        findings.push({ kind: "render", where: view, key: `${view} did not render`, detail: `${VIEW_READY[view]} never appeared` });
        return false;
      }
    };

    await page.goto(`${serverUrl}/#/${views[0]}`);
    for (const view of views) {
      await page.evaluate((v: string) => {
        location.hash = `#/${v}`;
      }, view);
      if (!(await ready(view))) continue;
      await check(view, null);

      if (view === "dashboard") {
        const s = await page.evaluate(sampleStability, { minMs: STABILITY_MIN_MS, maxMs: STABILITY_MAX_MS });
        if (s.rows.length === 0) {
          findings.push({ kind: "stability", where: view, key: "no meter rows", detail: "no .meter-track-wrapper to sample" });
        }
        for (const r of s.rows) {
          if (r.heights.length > 1) {
            const at = r.heights.map(([h, ro]) => `${h}px at "${ro}"`).join(", ");
            findings.push({ kind: "stability", where: view, key: `${r.selector} height changes`, detail: `${r.selector} height changes with the readout: ${at}` });
          }
        }
        for (const c of s.cards) {
          if (c.heights.length > 1) {
            findings.push({ kind: "stability", where: view, key: `${c.selector} height changes`, detail: `${c.selector} height took ${c.heights.join(", ")}px` });
          }
        }
        for (const r of s.rows) {
          if (!r.readouts.includes("-inf") || !r.readouts.some((t) => /^-\d\d\.\d dBFS$/.test(t))) {
            findings.push({ kind: "stability", where: view, key: `${r.selector} level cycle not seen`, detail: `${r.selector} never showed both -inf and a -xx.x dBFS readout (saw ${r.readouts.join(", ") || "none"})` });
          }
        }
      }
    }

    // Open states, checked on the dashboard: only the overlay is audited for
    // contrast (the page under it was checked above), the whole page for
    // overflow.
    if (!flags.onlyView || flags.onlyView === "dashboard") {
      await page.evaluate(() => {
        location.hash = "#/dashboard";
      });
      await ready("dashboard");

      await page.click("#theme-menu-btn");
      try {
        await page.waitForSelector("[role='menu']:not([hidden])", { timeout: 3000 });
        await check("theme menu open", "[role='menu']:not([hidden])");
      } catch {
        findings.push({ kind: "render", where: "theme menu open", key: "theme menu did not open", detail: "[role=menu] stayed hidden" });
      }
      await page.keyboard.press("Escape");

      await page.click("#header-bell-btn");
      try {
        await page.waitForSelector("#notif-panel:not([hidden])", { timeout: 3000 });
        await page.waitForTimeout(200);
        await check("notification panel open", "#notif-panel");
      } catch {
        findings.push({ kind: "render", where: "notification panel open", key: "notification panel did not open", detail: "#notif-panel stayed hidden" });
      }
      await page.keyboard.press("Escape");

      // Last, because an error toast lingers for seconds over whatever follows.
      await fetch(`${serverUrl}/__mock/notify`, { method: "POST" });
      try {
        await page.waitForSelector(".toast-container .toast", { timeout: 3000 });
        await page.waitForTimeout(200);
        await check("error toast", ".toast-stack");
      } catch {
        findings.push({ kind: "render", where: "error toast", key: "error toast did not appear", detail: "no .toast after a live error notification" });
      }
    }

    await context.close();
  }

  // Report: per combination, then the same findings merged across
  // combinations, which is the list to fix from.
  const out: string[] = [];
  let failures = 0;
  out.push("", "=== Rendered UI sweep ===", "");
  for (const [name, findings] of byCombo) {
    failures += findings.length;
    out.push(`--- ${name}: ${findings.length ? `${findings.length} failure(s)` : "ok"}`);
    const where = [...new Set(findings.map((f) => f.where))];
    for (const w of where) {
      out.push(`  [${w}]`);
      for (const f of findings.filter((x) => x.where === w)) out.push(`    ${f.kind}: ${f.detail}`);
    }
  }

  const merged = new Map<string, { kind: Kind; detail: string; where: Set<string>; combos: Set<string> }>();
  for (const [name, findings] of byCombo) {
    for (const f of findings) {
      const k = `${f.kind}|${f.key}`;
      const m = merged.get(k) ?? { kind: f.kind, detail: f.detail, where: new Set<string>(), combos: new Set<string>() };
      m.where.add(f.where);
      m.combos.add(name);
      merged.set(k, m);
    }
  }
  out.push("", `=== Unique findings (${merged.size}) ===`);
  for (const kind of ["render", "js-error", "overflow", "stability", "scaling", "contrast"] as const) {
    const list = [...merged.values()].filter((m) => m.kind === kind);
    if (!list.length) continue;
    out.push("", `${kind} (${list.length}):`);
    for (const m of list) {
      out.push(`  - ${m.detail}`);
      out.push(`      in: ${[...m.where].join(", ")} | ${m.combos.size === combos.length ? "every combination" : [...m.combos].join("; ")}`);
    }
  }

  out.push("", "=== Coverage ===");
  out.push(`combinations: ${combos.length}; views: ${views.join(", ")}; text elements checked: ${checkedTotal}`);
  out.push(`font size method: ${[...fontMethods].join(", ")}`);
  if (unscaled.size) out.push(`scaling not checked at ${[...unscaled].join(", ")}: no 16px run to compare against`);
  out.push(`not checked: ${Object.entries(skippedTotals).map(([k, v]) => `${k} x${v}`).join(", ") || "nothing"}; also ::before/::after text and canvas drawing`);
  out.push(`result: ${failures ? `FAIL, ${failures} failure(s)` : "PASS"} in ${Math.round((Date.now() - t0) / 1000)}s`);
  console.log(out.join("\n"));

  if (flags.keepOpen) {
    const page = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();
    await page.goto(`${serverUrl}/#/dashboard`);
    console.log(`keep-open: browser and ${serverUrl} stay up; Ctrl-C to quit`);
    await new Promise<never>(() => {});
  }
  return failures ? 1 : 0;
}

try {
  process.exitCode = await run();
} catch (err) {
  console.error("sweep:", err instanceof Error ? err.message : err);
  process.exitCode = 2;
}
