# web/AGENTS.md: Web UI rules

Frontend guidance for AI coding agents. The root `AGENTS.md` still applies
(gate, code style, workflow). Paths below are relative to `web/`.

The UI is deliberately framework-free. Do NOT introduce React, Svelte, Vue,
Lit, a virtual DOM, a bundler, a CSS framework, or any npm runtime dependency.
The output must stay plain ES modules that `tsc` emits and Go embeds
(`embed.go`; `embed_skipfrontend.go` is the stub for `-tags skipfrontend`).

## Toolchain and modules

- TypeScript strict mode (`tsconfig.json`: `strict`, `noImplicitAny`,
  `noUnusedLocals`, `noUnusedParameters`), target and module ES2022. Every
  step runs through `npx -p <pinned tool>` from `Taskfile.yml`: `tsc`
  (typescript 7), `oxlint --deny-warnings`, and `html-validate` with the
  recommended and a11y presets. Zero warnings is the bar. `task web:verify`
  runs them all; `task web:test` runs the unit tests.
- Relative imports carry the emitted `.js` extension
  (`import { store } from "./lib/store.js"`); no bare package imports.
  `moduleResolution: "bundler"` is only a tsc resolution mode; there is no
  bundler, and the browser loads the emitted files as is, so an extensionless
  import breaks at runtime.
- `static/index.html` is the one page: header, nav, one `.view-container` per
  view, toast regions, and live regions. It loads `app.js` as a module.
  `static/styles.css` is the one stylesheet. Fonts (Inter, JetBrains Mono) are
  self-hosted woff2 in `static/fonts`; icons are inline SVG. Never load
  anything from a CDN: the appliance often runs on a LAN with no internet.

## Structure

- `src/app.ts`: bootstrap only (theme, nav, views, notification center,
  `store.start()`).
- `src/lib/`: singletons and shared helpers. `api.ts` (`api`, the REST client;
  raises `ApiError` from RFC 9457 problem bodies, handles the Bearer token and
  401; the one deliberate bypass is the restart modal's raw
  `fetch("/api/v1/healthz")` probe), `sse.ts` (`sse`, fetch-streaming SSE
  client with reconnect and heartbeat watchdog), `store.ts` (`store`, app
  state), `router.ts` (hash routes `#/dashboard`, `#/events`, `#/system`),
  `modal.ts` (focus trap, inert background, `confirmDialog`), `ui.ts` (DOM and
  formatting helpers), `types.ts` (API types).
- `src/components/`: reusable widgets (`StatTile`, `FilterChips`,
  `CustomDropdown`, `VUMeter`, `DeviceSettingsForm`, `NotificationCenter`,
  toast, modals).
- `src/views/`: one class per route (`DashboardView`, `EventsView`,
  `SystemView`) bound to an existing `.view-container`.
- `src/lib/*-core.ts`: pure logic with no DOM, timers, storage, or network.
  Every non-trivial decision (filtering, grouping, formatting, diffing,
  validation) belongs here, with a matching `test/*.test.ts` run by
  `node:test`.

## State and reactivity

No reactive framework; reactivity is explicit events plus idempotent
reconcile:

- `AppStore` (`lib/store.ts`) and `NotificationStore` (`lib/notifications.ts`)
  extend `EventTarget` and own all server state. They poll the REST API,
  consume SSE, and announce changes with named `CustomEvent`s (`devices`,
  `status`, `config`, `system`, `available`, `levels`, `connection`,
  `loaderror`, `authrequired`, `authok`, and `change` on the notification
  store).
- Views and components subscribe with `addEventListener` and render from
  `store.getState()`. They never keep a second copy of server state or fetch
  on their own; mutations go through store or `api` methods, then the view
  re-renders from the next event.
- Coalesce bursts: `DashboardView` schedules one `reconcile()` per microtask
  (`queueMicrotask` behind `renderScheduled`). `EventsView` renders only while
  visible and marks itself dirty otherwise. `SystemView` patches just the
  section each event affects.
- Never clobber user input: a form being edited is not repopulated from a
  store event (`SystemView` tracks `netDirty`, `authDirty`, `notifyDirty`).
- Reconcile, never rebuild. Keep a keyed `Map` of stable per-item entries,
  create each once, patch only changed fields (`setText`/`setHidden` write
  only on change), rebuild an element only when its shape changes, and
  reorder with a diff so steady-state renders move no nodes. Replacing
  `innerHTML` or re-creating a list per update is a bug: it drops keyboard
  focus and screen reader position on every poll or SSE tick.
- Guard async races. The store uses monotonic generation counters so a stale
  response cannot overwrite fresher state; the SSE client uses a generation to
  cancel a superseded connect loop. Do the same for any new async write path.
- High-rate data (levels at 10 Hz) goes straight to the component that draws
  it (the canvas `VUMeter`), not through a view reconcile.

## Components

- A component is a class that builds its DOM with `elem()` or
  `document.createElement` in the constructor, exposes `readonly el`, and
  offers a small imperative API (`set(...)`, `update(...)`). The caller owns
  the state and pushes it in; the component reports intent through callbacks
  in its options (`onChange`). Only app-level components (notification
  center, login modal) import the store.
- Create buttons with the `button()` factory from `lib/ui.ts`. Show work in
  progress with `setBusy`/`clearBusy` (aria-disabled plus aria-busy), never by
  toggling `disabled` on a focused control, which drops focus.
- Ids for `aria-labelledby`/`aria-describedby` come from a module-level
  sequence counter (see `dropdownSeq`, `chipsSeq`).
- Reuse before adding: `showToast`, `confirmDialog`, `renderLoadError` (load
  failure with Retry), `apiErrorMessage`/`setFieldError`, `copyText`,
  `formatUptime`/`formatRelative`, and icon constants such as `ICON_COPY` and
  `TOAST_ICONS`.

## DOM safety

- Runtime data is only written with `textContent` (via `elem`, `setText`) or
  attributes. `innerHTML` is allowed solely for trusted, static inline SVG
  constants; mark each new such assignment with `// static, trusted markup`.
  Never interpolate device names, config values, API responses, or any user
  input into markup.
- `localStorage` holds only per-browser preferences (theme, access token,
  collapsed sections, dismissed notifications). Wrap access so a
  storage-blocked browser still works (`readBoolPref`/`writeBoolPref`, the
  try/catch in `auth.ts` and `notifications.ts`). The theme read and write in
  `app.ts` are still unwrapped: a known defect to fix, not a pattern to copy.

## Accessibility (a CI gate, not a nicety)

- `web:a11y` (html-validate on `index.html`) and the WCAG contrast test
  (`test/contrast.test.ts`) must pass. The contrast test checks only the pairs
  in its `PAIRS` table, so any new colored surface or text-on-background
  combination needs a new entry.
- Everything is keyboard-operable with a visible focus ring. Modals trap focus
  (`trapFocus`), make the background inert (`setAppInert`), and return focus
  to the invoker on close. Updates must not steal or drop focus.
- Native elements and correct ARIA: toggles use `aria-pressed`, groups are
  labeled, decorative icons are `aria-hidden` (`iconSpan`). Announce state
  through the existing live regions: polite `toast-root` and `role=status`
  for notices, assertive `toast-alerts` for errors.
- Honor `prefers-reduced-motion`; every new animation needs a reduced-motion
  fallback in `styles.css`.

## Styling

- Colors, spacing, radii, and type come from CSS custom properties on `:root`
  (dark is the default) overridden in `:root[data-theme="light"]`. Use the
  tokens; no hard-coded colors or one-off per-theme overrides. If a token pair
  fails contrast, fix the token.
- Theme is the `data-theme` attribute on `<html>`, persisted per browser.
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
