// A leaf module for the h() element builder, so modules that lib/ui.ts itself
// imports (the toast) can build DOM with it without an import cycle. lib/ui.ts
// re-exports it.

type HAttrValue = string | number | boolean | null | undefined;

// HAttrs are the attributes h() sets, written as in the markup ("class",
// "aria-label", "tabindex"). A string or number is set as is, true sets an
// empty (present) attribute, and false, null or undefined leave it off, so a
// conditional attribute reads inline. aria-pressed and friends take the
// strings "true" and "false", not booleans. An event handler attribute (on*)
// would be inline script, so the type refuses lowercase ones and h() throws on
// any casing; add listeners with addEventListener.
export type HAttrs = Readonly<Record<string, HAttrValue> & { [K in `on${string}`]?: never }>;

// HChild is one child of h(). A string becomes a text node, never markup.
// false, null, undefined and "" are skipped, so a conditional child reads
// inline and an element built with an empty text stays :empty, as the loading
// placeholders in styles.css expect.
export type HChild = Node | string | false | null | undefined;

const EVENT_HANDLER_ATTR = /^on/i;

// h builds an element whose nesting mirrors the markup it produces, typed by
// its tag (h("button") is an HTMLButtonElement). It only builds: updates keep
// using setText, setHidden and friends on the elements it returns.
export function h<K extends keyof HTMLElementTagNameMap>(tag: K, attrs: HAttrs, ...children: HChild[]): HTMLElementTagNameMap[K];
export function h<K extends keyof HTMLElementTagNameMap>(tag: K, ...children: HChild[]): HTMLElementTagNameMap[K];
export function h<K extends keyof HTMLElementTagNameMap>(tag: K, ...rest: (HAttrs | HChild)[]): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  const first = rest[0];
  // A plain object in the first slot is the attributes; a node has nodeType.
  if (typeof first === "object" && first !== null && !("nodeType" in first)) {
    rest.shift();
    for (const [name, value] of Object.entries(first as HAttrs)) {
      if (EVENT_HANDLER_ATTR.test(name)) throw new TypeError(`h: event handler attribute ${name} on <${tag}>; use addEventListener`);
      if (value === false || value === null || value === undefined) continue;
      e.setAttribute(name, value === true ? "" : String(value));
    }
  }
  for (const c of rest as HChild[]) {
    if (c === false || c === null || c === undefined || c === "") continue;
    e.append(c);
  }
  return e;
}
