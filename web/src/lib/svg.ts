// A leaf module for icon markup, so components that lib/ui.ts itself imports
// (the toast) can build icons without an import cycle. lib/ui.ts re-exports it.

// svgIcon wraps the body of a 24x24 stroked (Lucide style) glyph in its <svg>
// element at the given pixel size and stroke width, so an icon constant
// carries only its paths. The body must be static, trusted markup: the result
// goes through innerHTML.
export function svgIcon(body: string, size = 16, stroke = 2): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="${stroke}" stroke-linecap="round" stroke-linejoin="round">${body}</svg>`;
}
