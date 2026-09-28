# web/AGENTS.md: Web UI rules

Frontend guidance for AI coding agents. The root `AGENTS.md` still applies
(gate, code style, workflow). Paths below are relative to `web/`. The rules
for the TypeScript source (state and reactivity, components, DOM safety) are
in `src/AGENTS.md`; read it before editing under `src/`.

The UI is deliberately framework-free. Do NOT introduce React, Svelte, Vue,
Lit, a virtual DOM, a bundler, a CSS framework, or any npm runtime dependency.
The output must stay plain ES modules (plus the one classic script,
`theme-init.js`) that `tsc` emits and Go embeds
(`embed.go`; `embed_skipfrontend.go` is the stub for `-tags skipfrontend`).

## Toolchain and modules

- TypeScript strict mode (`tsconfig.json`: `strict`, `noImplicitAny`,
  `noUnusedLocals`, `noUnusedParameters`, `noUncheckedIndexedAccess`,
  `verbatimModuleSyntax`, `erasableSyntaxOnly`), target and module ES2022.
  Every step runs through `npx -p <pinned tool>` from `Taskfile.yml`: `tsc`
  (typescript 7), `oxlint --deny-warnings`, and `html-validate` with the
  recommended and a11y presets. Zero warnings is the bar. `task web:verify`
  runs them all; `task web:test` runs the unit tests with Node's own type
  stripping, so it needs a Node that strips types by default (CI pins 26).
  `test/tsconfig.json` and the sweep's `e2e/tsconfig.json` extend
  `tsconfig.json`, so a new strictness flag reaches all three.
- An indexed read (`arr[i]`, `record[key]`, a regex group) may be
  `undefined`: guard it, or use `.entries()` or `charAt`. No blanket `!`. In
  tests, `at` and `group` from `test/fixtures.ts` fail the test on a missing
  item.
- Relative imports carry the source `.ts` extension
  (`import { store } from "./lib/store.ts"`); no bare package imports. tsc
  rewrites them to `.js` in the output (`rewriteRelativeImportExtensions`),
  which the browser loads as is. `moduleResolution: "bundler"` is only a tsc
  resolution mode; there is no bundler, so an extensionless import breaks at
  runtime.
- Only erasable syntax (`erasableSyntaxOnly`): no enums, namespaces, or
  constructor parameter properties; declare the field and assign it. Type-only
  imports say `import type` (`verbatimModuleSyntax`). This is what lets
  `web:test` run the tests straight from source with Node's type stripping;
  `web:typecheck` type-checks them (`test/tsconfig.json`).
- `static/index.html` is the one page: header, nav, one `.view-container` per
  view, toast regions, and live regions. It loads `app.js` as a module, and
  `theme-init.js` as a blocking classic script in `<head>`.
  `static/styles.css` is the one stylesheet. Fonts (Inter, JetBrains Mono) are
  self-hosted woff2 in `static/fonts`; icons are inline SVG. Never load
  anything from a CDN: the appliance often runs on a LAN with no internet.

## Structure

- `src/app.ts`: bootstrap only (theme, nav, views, notification center,
  `store.start()`).
- `src/theme-init.ts`: the one non-module script; applies the theme before
  the first paint (see Styling).
- `src/lib/`: singletons and shared helpers. `api.ts` (`api`, the REST client;
  raises `ApiError` for every failed response, keeping the fields of an RFC
  9457 problem body only, read through `problemDetail` and `problemFor`,
  handles the Bearer token and 401; the one deliberate bypass is the restart
  modal's raw
  `fetch("/api/v1/healthz")` probe; every request has a
  `REQUEST_DEADLINE_MS` deadline through `withDeadline` in `deadline.ts`),
  `sse.ts` (`sse`, fetch-streaming SSE client with reconnect and heartbeat
  watchdog; it reports `connected` at the stream's first bytes, which the
  appliance sends as a `: open` comment once it has subscribed),
  `timers.ts` (the `Timers` seam tests replace with `FakeTimers`),
  `text.ts` (`sentence`, `deviceIdTitle`), `store.ts` (`store`, app
  state; `applyUpdateStatus` merges an update check or request response and
  drops older system reads), `router.ts` (hash routes `#/dashboard`,
  `#/events`, `#/system`,
  `#/about`; the pure route decisions are in `router-core.ts`),
  `modal.ts` (focus trap, inert background, `confirmDialog`), `ui.ts` (DOM and
  formatting helpers), `theme.ts` (the System/Light/Dark mode, live OS follow
  and cross-tab sync, with the browser objects injected so `node:test` covers
  it), `prefs.ts` (per-browser boolean preferences,
  `readBoolPref`/`writeBoolPref`, `onPrefChange` (every cross-tab
  preference listener), the hide-inactive
  keys, and the once-per-page "preferences not saved" notice),
  `update-core.ts` (the update status text in System Information,
  `UpdateFollow` for an update this tab started, and `VersionWatch`, which
  notices the appliance running another version than the page loaded
  against: the following tab reloads, any other tab is told to),
  `types.ts` (API types).
- `src/components/`: reusable widgets (`StatTile`, `FilterChips`,
  `CustomDropdown`, `MenuButton`, `VUMeter`, `DeviceSettingsForm`,
  `NotificationCenter`, toast, modals).
- `src/views/`: one class per route (`DashboardView`, `EventsView`,
  `SystemView`, `AboutView`) bound to an existing `.view-container`.
  `SystemView` hands each card to a class in `views/system/` that is bound to
  the card's static markup in `index.html` (found inside the card with
  `part()`) and owns its elements, dirty state and actions; the view keeps the
  store and router subscriptions and decides which cards an event touches.
  `DashboardView` keeps a `DeviceCard` per device id (built with `h()`) and
  `AvailableDevices` in `views/dashboard/`, with one `ConfigQueue` that
  serializes every card's device mutations. Like views, these cards call
  `store` and `api` methods for their own mutations, but they do not
  subscribe. The About
  page loads `static/licenses.json` (generated by `task licenses:generate`,
  never edited by hand) with a plain `fetch` on its first visit: a static
  file, not an API call, so it bypasses `api.ts`.
- `src/lib/*-core.ts`: pure logic with no DOM, timers, storage, or network.
  Every non-trivial decision (filtering, grouping, formatting, diffing,
  validation) belongs here, with a matching `test/*.test.ts` run by
  `node:test`.
- `e2e/`: the rendered sweep (`task web:sweep`, about 90 s, not in `check`).
  `mock-server.ts` serves a compiled UI with fixture data for every endpoint
  (run it alone to look at the UI: `node web/e2e/mock-server.ts <dir> [port]`);
  `sweep.ts` drives Playwright's Chromium over every view in both themes at
  320 and 1280 px and 16, 20 and 24 px browser font sizes, checking composited
  contrast, that text scales, horizontal overflow, and steady meter rows. Run
  it after layout, colour or type changes; keep the fixtures in step with
  `types.ts`.

## Accessibility (a CI gate, not a nicety)

- `web:a11y` (html-validate on `index.html`) and the WCAG contrast test
  (`test/contrast.test.ts`) must pass. The contrast test checks only the pairs
  in its `PAIRS` table, so any new colored surface or text-on-background
  combination needs a new entry (each foreground and ground pair once; name
  another surface that shares it in the entry's description). Text and the
  surface under it take token colors even on a one-off surface, so a pair can
  describe them. Secondary and muted text must reach `SMALL_TEXT` (5.5:1),
  not just AA.
- Type scale and floor (`test/legibility.test.ts`): every `font-size` is a
  `var(--font-size-*)` role token (rem, defined on `:root`), never a literal,
  and a `font:` shorthand may only reset (`font: inherit`). Sizes below are
  at the default 16px root. No text below
  12px, and text below the 13px body size needs weight 500 or more. Sentences
  (subtitles, hints, notes, messages) use body (13px) at regular weight;
  caption (12px) is for short labels, badges and data at 500+. Declare the
  weight next to the size, since the test reads each rule on its own. The two
  allowed 11px exceptions (`--font-size-micro`) are listed in the test with
  their reasons. The one relative token, `--font-size-code`, is only for
  inline code in body-size or larger text, and `index.html` sets no font size
  inline; the test checks both.
- Everything is keyboard-operable with a visible focus ring. Modals trap focus
  (`trapFocus`), make the background inert (`setAppInert`), and return focus
  to the invoker on close. Updates must not steal or drop focus.
- Native elements and correct ARIA: toggles use `aria-pressed`, groups are
  labeled, decorative icons are `aria-hidden` (`iconSpan`). Announce state
  through the existing live regions: polite `toast-root` and `role=status`
  for notices, assertive `toast-alerts` for errors. When a render must move
  focus the operator did not move (a focused control hides), move it to a
  stable control in the same card and `announce` why in
  `#dashboard-announce`. When the card itself went, focus goes to its
  nearest neighbour (`neighbourOrder`, `focusNeighbour`), else
  `focusWorkspace()`. An async
  action (a queued save, an Enable, a Remove) moves focus when it settles
  only if it is still where the action left it or dropped (`focusDropped`).
- Honor `prefers-reduced-motion`; every new animation needs a reduced-motion
  fallback in `styles.css`.

## Styling

- Colors, radii, control heights and type come from CSS custom properties on
  `:root` (dark is the default) overridden in `:root[data-theme="light"]`.
  Use the tokens; no hard-coded colors or one-off per-theme overrides. If a
  token pair fails contrast, fix the token.
- Theme is the `data-theme` attribute on `<html>`, persisted per browser.
  `src/theme-init.ts` is a classic (non-module) script loaded in `<head>`
  that applies it before the first paint: the saved choice, else (nothing
  saved, or storage blocked) `prefers-color-scheme`, else dark when
  `matchMedia` is unavailable. Keep it import-free, free of type syntax, and
  non-throwing; `test/theme-init.test.ts` runs the source as a classic script
  and pins its `<head>` tag, its key and its media query against `THEME_KEY`
  and `PREFERS_LIGHT_QUERY` in `lib/theme.ts`. While nothing is saved,
  `lib/theme.ts` follows OS changes live in System mode; choosing Light or
  Dark in the header menu saves and wins, and System removes the key.
  `color-scheme` on each theme block keeps native controls in step.
- Programmatic focus on a tall region uses `focus({ preventScroll: true })`,
  so the focus call never picks the scroll position: a fallback focus onto
  `#main-content` (device removal, login close) leaves the page where the
  operator was, while the router focuses the new view section and then scrolls
  to the top on purpose, so the header stays in view.
- The UI follows the browser's font size setting: font sizes are rem
  tokens, a box sized to fit its text (a badge or pill height, a line height,
  the meter columns) is rem or em, and breakpoints are em. Layout widths
  that do not fit text (grid minimums, flex bases, container and modal
  maximums) may stay px. `task web:sweep` renders every view at
  16, 20 and 24px browser font sizes and fails on text that does not grow or
  a page that scrolls sideways.
- Class names are descriptive kebab-case (`.view-container`,
  `.meter-canvas-container`). Apart from `.visually-hidden` there are no
  utility classes; style by component.
- UI copy is plain English matching existing wording (Title Case for headings
  and section titles). Say what happened and what to do next; no raw error
  codes without context.

## Keeping the UI and API in sync

`src/lib/types.ts` is hand-written to mirror `../api/openapi.yaml`. When the
spec changes, update `types.ts` and the `api.ts` methods in the same change.
SSE event names and payloads must match the spec's `/events` documentation.
