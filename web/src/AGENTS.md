# web/src/AGENTS.md: Web UI source rules

Rules for the TypeScript source under `web/src/`. `web/AGENTS.md` still
applies (toolchain, structure, accessibility, styling), and so does the root
`AGENTS.md`. Paths below are relative to `web/src/`.

## State and reactivity

No reactive framework; reactivity is explicit events plus idempotent
reconcile:

- `AppStore` (`lib/store.ts`) and `NotificationStore` (`lib/notifications.ts`)
  own all server state. They poll the REST API, consume SSE, and announce
  changes as typed events: they extend `Emitter` (`lib/emitter.ts`), whose event
  map fixes each name and payload (`StoreEvents`: `devices`, `status`, `config`,
  `system`, `available`, `levels`, `levelsdropped`, `connection`, `loaderror`,
  `authrequired`, `authok`; `change` on the notification store). The router
  announces `route` the same way. A new event goes in the map first, so a
  misspelled name or a wrong payload fails `tsc`. `devices`, `status`, `system`
  and `available` fire only when their data changed; the first three fire again
  on the first read after a failed one (unless a newer read had already
  applied), `available` does not. `config` fires every poll. A mutation flow
  must not wait for a `devices`, `status`, `system` or `available` event, which
  a no-op change never sends: repaint from `config` or the awaited call.
- Views and components subscribe with `on(name, payload => ...)` and render from
  `store.getState()`. They never keep a second copy of server state or fetch
  on their own; mutations go through store or `api` methods, then the view
  re-renders from the next event.
- Coalesce bursts: `DashboardView` schedules one `reconcile()` per microtask
  (`queueMicrotask` behind `renderScheduled`), and so does `EventsView`, which
  renders only while visible and marks itself dirty otherwise. `SystemView`
  passes each event only to the cards it affects.
- Poll only for a viewer: `AppStore` pauses the REST poll while the page is
  hidden (`setPageHidden`, fed by `visibilitychange` in `app.ts`) and
  refreshes at once when it shows again. The SSE stream stays up for
  `HIDDEN_STREAM_GRACE_MS` (60 s) of hiding, then stops; showing the page
  restarts it, and the connect re-sync recovers the notifications raised
  meanwhile (without toasts). Anything that runs per status tick inherits the
  pause, and work for one view also checks the route (the System view reloads
  the certificate only while it is the active view). Levels stream only
  while the dashboard shows or was left less than `LEVELS_GRACE_MS` (30 s)
  ago: `store.setLevelsWanted` then sets the stream's `?events=` filter to
  `NON_LEVEL_EVENTS`, so the appliance sends none, and announces
  `levelsdropped`. A new event type the UI consumes gets a constant in
  `lib/sse.ts` (as `LEVELS_EVENT` and `NOTIFICATION_EVENT` do) and goes in
  `NON_LEVEL_EVENTS` too.
- Lay out before data arrives. A region filled by a read after the first
  paint starts in its final shape with `setLoading` (placeholders sized like
  the values, `aria-busy`, `inert` on all but the heading) rather than
  `hidden`, and the read fills it in place, so the page does not shift as
  reads land. Hide only what turns out not to apply (the certificate card on
  a 501). A region below a list of unknown length waits for the list's first
  read instead (`store.devicesRead()`): Available Devices shows only once the
  rack's first devices read has settled, since the cards arriving would
  otherwise push it down.
- Never clobber user input: a form being edited is not repopulated from a
  store event (each System card with a form keeps its own `dirty` flag and
  skips a config read while it is set).
- Reconcile, never rebuild. Keep a keyed `Map` of stable per-item entries,
  create each once, patch only changed fields (`setText`/`setHidden`/`setAttr`
  write only on change), rebuild an element only when its shape changes, and
  reorder with a diff so steady-state renders move no nodes. Replacing
  `innerHTML` or re-creating a list per update is a bug: it drops keyboard
  focus and screen reader position on every poll or SSE tick.
- Guard async races. The store puts a `LatestGate` (`lib/latest-core.ts`) on
  each polled resource so an older response cannot overwrite a newer applied
  one; never drop a response merely because a newer request started, which
  starves the view on a link slower than the poll. The SSE client uses a
  generation to cancel a superseded connect loop. Do the same for any new
  async write path.
- Tell a refusal from an unknown outcome. `isRefusal(err)` is true only for
  an appliance problem body: say what was refused, naming the device.
  Anything else (a timeout, a proxy error, an unreadable 2xx) may have
  applied: re-read the resource inside the same queued task, judge the
  result by the re-read, and report with `showUnconfirmed`. A change that
  sends a whole array re-reads first after a failed re-read. Toasts quote
  `failureReason(err)`, never browser error text.
- High-rate data (levels at 10 Hz) goes straight to the component that draws
  it (the canvas `VUMeter`), not through a view reconcile. Each meter's state
  and sequencing live in `MeterController` (`lib/meter-core.ts`), and every
  meter draws on one shared frame loop (`meterFrames` in
  `components/vu-meter.ts`, a `FrameScheduler` from `lib/meter-core.ts`) that
  runs only while some meter has something new to draw; the dashboard
  suspends it while another view shows. Time-based animation uses elapsed
  milliseconds, never a frame count, which varies with refresh rate. A meter
  with no current level (none yet, levels dropped, or the stream down) sits
  at the floor and reads `--`, never `-inf`, so missing data does not pass
  for silence. `LevelsWatch` (`lib/dashboard-core.ts`) clears the meters
  when levels stop: dropped, the stream down, or none for
  `LEVELS_STALE_MS` on a live stream while the dashboard shows; a card also
  clears its own when a levels event lacks its device. The clip latch sees
  only the levels the page receives. A meter scrolled out of view skips its
  canvas paint. A reverse proxy must pass the event stream unbuffered (the
  appliance sends `X-Accel-Buffering: no`); buffered, levels arrive in bursts
  and the meters clear between them.

## Components

- A component is a class that builds its DOM with `h()` in the
  constructor, exposes `readonly el`, and
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
  failure with Retry) and `clearLoadError` (call it before rewriting or
  removing that container: it drops the alert role and parks focus; only the
  Dashboard announces the recovery, elsewhere focus parks on the view section
  when Retry held it, and a screen reader typically reads its label),
  `apiErrorMessage`/`firstProblem`/`setFieldError`,
  `focusDropped`/`focusOnOrDropped`/`focusWorkspace` (focus fallback when a
  control went away), `showUnconfirmed`, `failureReason`,
  `scrollBehavior` (a scripted scroll that follows reduced motion),
  `copyText`, `infoRow` (an `.info-grid` key and value pair), `sectionHead`
  (a card's icon title and description), `externalLink` (new-tab link with
  `rel="noopener"`), `announce` (a polite live-region message),
  `MenuButton` (a single-choice header menu),
  `formatUptime`/`formatRelative`, `switchControl` (every scripted on/off
  switch; the static ones in `index.html` copy its markup, `role="switch"`
  included), `svgIcon` (wraps a 24x24 stroked glyph's paths at a size and stroke
  width; every stroked icon uses it, from `lib/svg.ts`, a leaf module `ui.ts`
  re-exports), `focusTarget` (where focus or a click went relative to a popup
  and its opener), and icon constants such as `ICON_COPY` and `TOAST_ICONS`.
- A component with non-trivial event wiring keeps its state and sequencing in
  a DOM-free controller in `lib/*-core.ts` that drives injected ports (see
  `MenuController` and `PopoverController` in `lib/menu-core.ts`), so
  node:test pins the wiring, not only the decisions.
- Build DOM with `h(tag, attrs?, ...children)` (the leaf `lib/h.ts`,
  re-exported by `ui.ts`), so nesting mirrors the markup and the result has
  the tag's own type (`h("button")` is an `HTMLButtonElement`). Attributes
  are written as in the markup (`class`, `aria-label`); `false`, `null` or
  `undefined` leaves one off, and `on*` handlers are refused (use
  `addEventListener`). Strings become text nodes; `false`, `null`,
  `undefined` and `""` children are skipped, so an empty value stays
  `:empty` for the loading placeholders. It only builds: keep the elements
  you update in fields and patch them with `setText`/`setHidden`/`setAttr`.
  `StatTile` and `views/about.ts` show the style.

## DOM safety

- Runtime data is only written with `textContent` (via `h` or
  `setText`) or attributes. `innerHTML` is allowed solely for trusted,
  static inline SVG constants; mark each new such assignment with
  `// static, trusted markup`. Never interpolate device names, config
  values, API responses, or any user input into markup.
- `localStorage` holds only per-browser preferences (theme, access token,
  hidden meter channels, the Events `/` shortcut, read and dismissed
  notifications). Wrap access so a storage-blocked browser still works
  (`readBoolPref`/`writeBoolPref`, the try/catch in `auth.ts`,
  `notifications.ts`, `theme-init.ts`, and `lib/theme.ts`). When a
  preference the operator just chose cannot be saved, call
  `prefSaveNotice.report()` (`lib/prefs.ts`), which warns once per page; an
  automatic write (a snapshot or a live event) and the access token stay
  silent. Each key has one writer. The theme, the hidden channels and the
  Events `/` shortcut also follow changes made in another tab (the `storage`
  event).
