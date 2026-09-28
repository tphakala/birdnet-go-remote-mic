import { store } from "../lib/store.ts";
import { meterFrames, type VUMeter } from "../components/vu-meter.ts";
import { router } from "../lib/router.ts";
import { showToast } from "../components/toast.ts";
import { api, isRefusal } from "../lib/api.ts";
import { announce, button, clearBusy, deviceStateBadge, elem, focusDropped, focusWorkspace, formatUptime, holdsFocus, ICON_COPY, iconSpan, modeLabel, orderChildren, renderLoadError, reportClipboardFailure, setBusy, setHidden, setText, svgIcon, showUnconfirmed, switchControl, writeToClipboard } from "../lib/ui.ts";
import { availableCardKey, availableGoneMessage, availablePlan, bannerIsError, captureFormatLabel, channelLabel, controlGoneMessage, deviceGoneMessage, downCauseTitle, focusMovedMessage, footerMetrics, neighbourOrder, runtimeEnabled, tokenHiddenMessage, followDashboardRoute, followLevels, LevelsWatch, routeLevels, type LevelsTarget } from "../lib/dashboard-core.ts";
import { deviceIdTitle } from "../lib/text.ts";
import { hideInactiveKey, hideInactivePrefDevice, onPrefChange, parseBoolPref, readBoolPref, writeBoolPref } from "../lib/prefs.ts";
import { getToken } from "../lib/auth.ts";
import type { ApplianceStatus, AvailableDevice, Device, DeviceConfig, SystemInfo } from "../lib/types.ts";
import { apiErrorToast, ConfigQueue, STALE_BASE_TEXT } from "./dashboard/config-queue.ts";
import { MeterConsole } from "./dashboard/meter-console.ts";
import { SettingsPanel } from "./dashboard/settings-panel.ts";

// Trusted static SVG icon markup (no interpolation of runtime data).
const ICON_MIC =
  svgIcon('<path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path>', 20);
const ICON_ULTRA =
  svgIcon('<path d="M2 12h2"></path><path d="M6 8v8"></path><path d="M10 4v16"></path><path d="M14 6v12"></path><path d="M18 9v6"></path><path d="M22 12h-2"></path>', 20);
const ICON_ERROR =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line>', 20);
const ICON_WARN =
  svgIcon('<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line>', 18);
const ICON_LOCK =
  svgIcon('<rect width="18" height="11" x="3" y="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 10 0v4"></path>', 11);
// Vertical faders (the mixing-desk "sliders" glyph) for the settings toggle:
// the panel adjusts capture and stream parameters, which reads closer to an
// audio console than a generic gear does.
const ICON_SLIDERS =
  svgIcon('<line x1="4" x2="4" y1="21" y2="14"></line><line x1="4" x2="4" y1="10" y2="3"></line><line x1="12" x2="12" y1="21" y2="12"></line><line x1="12" x2="12" y1="8" y2="3"></line><line x1="20" x2="20" y1="21" y2="16"></line><line x1="20" x2="20" y1="12" y2="3"></line><line x1="2" x2="6" y1="14" y2="14"></line><line x1="10" x2="14" y1="8" y2="8"></line><line x1="18" x2="22" y1="16" y2="16"></line>', 13);
const ICON_CHEVRON =
  svgIcon('<path d="m6 9 6 6 6-6"></path>', 12, 2.2);

// METER_LEVELS is how routeLevels feeds a VUMeter, built once rather than
// per levels event.
const METER_LEVELS = {
  set: (m: VUMeter, rms: number, peak: number, clipped: boolean) => m.setLevels(rms, peak, clipped),
  clear: (m: VUMeter) => m.clearLevels(),
};

// availableLabel names an available device for people: its friendly name,
// with the ALSA address when there is one to tell two identical units apart,
// else the address, else the device id.
function availableLabel(d: AvailableDevice): string {
  return d.friendlyName && d.hwAddr ? `${d.friendlyName} (${d.hwAddr})` : d.friendlyName || d.hwAddr || d.device;
}

// setEnableBusy shows an Enable button's progress, and keeps its accessible
// name (which names the device) saying the same as its visible text.
function setEnableBusy(btn: HTMLElement, d: AvailableDevice, busy: boolean): void {
  if (busy) setBusy(btn, "Enabling...");
  else clearBusy(btn, "Enable");
  btn.setAttribute("aria-label", `${busy ? "Enabling" : "Enable"} ${availableLabel(d)}`);
}

// capsSummary renders a short human summary of a device's probed capabilities
// (channel support and top sample rate) for the available-devices list.
function capsSummary(d: AvailableDevice): string {
  const parts: string[] = [];
  const ch = d.supportedChannels ?? [];
  if (ch.length) {
    if (ch.includes(1) && ch.includes(2)) parts.push("mono/stereo");
    else if (ch.includes(2)) parts.push("stereo");
    else parts.push("mono");
  }
  const rates = d.supportedRates ?? [];
  if (rates.length) {
    const maxKhz = Math.max(...rates) / 1000;
    parts.push(`up to ${maxKhz.toLocaleString("en-US")} kHz`);
  }
  return parts.join(" · ");
}

// LiveBody holds the nodes of a serving card's body (endpoint strip, meter
// console, metrics footer). IdleBody holds the nodes of a non-serving card's
// body (error banner, footer note). Exactly one is present on a card, chosen by
// its shape; syncCard writes into whichever exists.
interface LiveBody {
  urlEl: HTMLElement;
  clientsEl: HTMLElement;
  droppedEl: HTMLElement;
  overrunsEl: HTMLElement;
  negotiatedEl: HTMLElement;
  meterConsole: MeterConsole;
}

interface IdleBody {
  banner: HTMLElement;
  bannerIcon: HTMLElement;
  bannerTitle: HTMLElement;
  bannerDesc: HTMLElement;
  footerNote: HTMLElement;
}

// ArticleParts are the nodes buildArticle produces for a device's current shape:
// the <article> plus every header/body node syncCard writes into. Returned as
// one bundle so the compiler checks that buildArticle populates all of them, and
// so the caller can assemble (or, on a rebuild, refresh) the CardEntry in one
// checked step instead of mutating a partially-built object behind an `as` cast.
interface ArticleParts {
  article: HTMLElement;
  // Header nodes, shape-independent (chips and lock are hidden on idle cards
  // rather than absent, so the header never has to be rebuilt on a shape flip):
  avatar: HTMLElement;
  titleEl: HTMLElement;
  hwEl: HTMLElement;
  modeTag: HTMLElement;
  rateTag: HTMLElement;
  chTag: HTMLElement;
  lockEl: HTMLElement;
  statusEl: HTMLElement;
  toggleInput: HTMLInputElement;
  settingsBtn: HTMLElement;
  pendingNote: HTMLElement;
  // Body: exactly one is present, matching the shape.
  live: LiveBody | null;
  idle: IdleBody | null;
}

// CardEntry is the STABLE per-device identity, held in a Map keyed by the
// immutable device id (read from device.device). The <article> and its
// body are a disposable render swapped only when the card's shape changes
// (serving with a given channel count, versus idle); everything that must
// survive a rebuild (the settings panel node, its form, the expanded/dirty
// state) hangs off the entry, not the article. Every node that shows DEVICE OR
// CONFIG data is written by exactly one function, syncCard; transient
// interaction state (the toggle's in-flight checked/busy and the copy button's
// "Copied!" feedback) is the deliberate exception. buildArticle creates the
// skeleton with no device data, so a data field syncCard forgets renders blank
// at development time instead of going silently stale in production, which is
// the class of bug the old build/update split produced.
interface CardEntry extends ArticleParts {
  device: Device; // latest runtime view, set by syncCard; device.device is the id
  // Set while an enable/disable PATCH for this device is queued or in flight,
  // on the entry rather than the toggle node, so a card rebuilt meanwhile
  // gets a busy toggle that the sync does not reset.
  togglePending?: boolean;
  // The state the pending change asked for, shown on a rebuilt toggle.
  toggleWant?: boolean;
  shape: string; // shapeKey of the currently mounted article
  // Settings panel: owned by the entry and moved between article renders, so an
  // open form survives a card rebuild rather than being torn down under the user.
  panel: SettingsPanel;
  // The per-device "hide inactive channels" display preference. Read from
  // storage once when the entry is created, rather than on every poll; the
  // settings switch and a change saved in another tab (the storage event)
  // update it. The dashboard is the key's one owner: it alone writes it. When
  // the browser cannot persist it, the choice holds until the device leaves
  // the list.
  hideInactive: boolean;
}

// pendingStop reports that a device is currently serving while the config now
// disables it. A config change is hot-applied, so this divergence is only ever a
// brief moment while the reload stops the device; the banner labels that instant
// so the still-live "Serving" badge and meters are not unexplained. Only a
// serving device qualifies: a failed or skipped device is not serving (its footer
// already explains the exclusion), and the reverse (a disabled card now enabled)
// is explained by the non-serving footer, so neither needs a banner.
function pendingStop(configEnabled: boolean, state: string): boolean {
  return state === "serving" && !configEnabled;
}

const PENDING_STOP_TEXT = "Disabling; this device stops serving shortly.";

// The copy button's resting and success labels, defined once so handleCopyUrl
// restores the resting values by identity rather than re-reading the (possibly
// mid-swap) DOM: a rapid second click must not capture "Copied!" as the value to
// restore and leave the visible label or accessible name stuck on it.
const COPY_LABEL = "Copy URL";
const COPY_LABEL_DONE = "Copied!";
const COPY_ARIA = "Copy RTSP stream URL";
const COPY_ARIA_DONE = "RTSP stream URL copied";
const TOKEN_LABEL = "Token";
const TOKEN_ARIA = "Copy access token";
// Pending label-restore timer per copy button, so a second click clears the
// prior restore instead of letting two timers fight (WeakMap: entries GC with
// the button, no leak).
const copyResetTimers = new WeakMap<HTMLElement, number>();

// flashCopied plays the copy-success feedback on btn: the green "copied" pulse,
// the visible label swapped to "Copied!" and the accessible name to ariaDone
// (so the success is announced too), all restored after a moment. It restores
// to the fixed resting values rather than re-reading the DOM, and clears any
// pending restore first, so a rapid second click cannot strand the button on
// "Copied!".
function flashCopied(btn: HTMLElement, label: HTMLElement | null, rest: string, ariaRest: string, ariaDone: string): void {
  btn.classList.add("copied");
  if (label) label.textContent = COPY_LABEL_DONE;
  btn.setAttribute("aria-label", ariaDone);
  const prev = copyResetTimers.get(btn);
  if (prev !== undefined) window.clearTimeout(prev);
  copyResetTimers.set(btn, window.setTimeout(() => {
    btn.classList.remove("copied");
    if (label) label.textContent = rest;
    btn.setAttribute("aria-label", ariaRest);
    copyResetTimers.delete(btn);
  }, 1600));
}

// nonServingFooterText is the footer message for a card that is not serving.
function nonServingFooterText(state: string, configEnabled: boolean): string {
  if (state === "disabled") {
    return configEnabled
      ? "Enabling; this device starts serving shortly."
      : "Streaming is disabled for this device. Enable it to start serving.";
  }
  return "Excluded from the RTSP stream server. Other active devices continue serving without interruption.";
}

// OVERRUNS_DESCRIPTION explains the capture-overrun counter, which the label
// alone cannot for an operator who has not met the term.
const OVERRUNS_DESCRIPTION =
  "Times capture fell behind the sound card, or the system suspended, and lost audio; " +
  "usually a busy host or unstable USB. Counted since the device opened.";

// metricItem builds one footer metric: its label and the value element syncCard
// fills. A description is read after the value by a screen reader from a
// visually hidden span, and shown to mouse users as a title on the visible
// label; a title alone reaches neither touch nor assistive-tech users. That
// label is hidden from assistive tech and a visually hidden copy speaks for it,
// so a screen reader that also announces titles does not read the description
// twice.
function metricItem(label: string, description?: string): { item: HTMLElement; value: HTMLElement } {
  const item = elem("div", "metric-item");
  if (description) {
    const shown = elem("span", undefined, label);
    shown.title = description;
    shown.setAttribute("aria-hidden", "true");
    item.append(shown, elem("span", "visually-hidden", label));
  } else {
    item.appendChild(elem("span", undefined, label));
  }
  const value = elem("span", "metric-val mono");
  item.appendChild(value);
  if (description) item.appendChild(elem("span", "visually-hidden", ` (${description})`));
  return { item, value };
}

// Sequence for the token tag's description element ids (aria-describedby targets).
let tokenDescSeq = 0;

function rtspPort(listen: string | undefined): string {
  if (!listen) return "8554";
  const i = listen.lastIndexOf(":");
  return i >= 0 ? listen.slice(i + 1) : listen;
}

// meterCount is the number of VU meter rows a serving device shows: one per
// CAPTURED hardware channel (the negotiated count), not per streamed channel.
function meterCount(d: Device): number {
  return Math.max(1, d.negotiatedChannels ?? d.channels.length);
}

// shapeKey names the only genuinely structural facts about a card: whether it
// has a serving body (endpoint strip + meter console + metrics) or an idle body
// (error banner + footer), and how many meter rows the serving body has. A
// change to either forces a rebuild (mount); every other field is a syncCard
// write on the existing article. Mode is deliberately excluded: the avatar and
// chips are synced, not rebuilt.
function shapeKey(d: Device): string {
  return d.state === "serving" ? `serving:${meterCount(d)}` : "idle";
}

export class DashboardView {
  // Cards keyed by immutable device id (the stable identity). The levels
  // payload is keyed by name, which the levels handler reads from each card.
  private cards: Map<string, CardEntry> = new Map();
  private rack: HTMLElement | null;
  private emptyEl: HTMLElement | null;
  private availableSection: HTMLElement | null;
  private availableRack: HTMLElement | null;
  // The polite status region for focus moves the operator did not make.
  private announceEl: HTMLElement | null;
  // Device ids with a provisioning request in flight, so the Enable button shows
  // progress and a second click cannot double-provision.
  private provisioning: Set<string> = new Set();
  // The Available Devices cards on screen, by device id, each with the key of
  // what it shows (availableCardKey), so a render rebuilds only changed cards.
  private availableCards = new Map<string, { card: HTMLElement; key: string; label: string }>();
  // The Available card that held keyboard focus when a render removed it
  // during that device's Enable: its device, its neighbours on screen and its
  // label, so the Enable can move focus when it settles (to the new device
  // card on success; on failure to its own card when it is listed again,
  // else its nearest neighbour still listed). Only set while that Enable is in flight.
  private availableFocusLost: { id: string; neighbours: string[]; label: string } | null = null;
  private status: ApplianceStatus | null = null;
  // Serializes the device mutations of every card (see ConfigQueue).
  private readonly queue = new ConfigQueue();
  // Set while a reconcile() is queued on the microtask, so the several store
  // events a single poll tick fires collapse into one pass (see render()).
  private renderScheduled = false;

  constructor() {
    this.rack = document.getElementById("channel-rack");
    this.emptyEl = document.getElementById("rack-empty");
    this.availableSection = document.getElementById("available-section");
    this.availableRack = document.getElementById("available-rack");
    this.announceEl = document.getElementById("dashboard-announce");
    this.initHeader();
    this.bindEvents();
  }

  private initHeader(): void {
    const host = document.getElementById("appliance-hostname");
    if (host) host.textContent = window.location.hostname;
    const addr = document.getElementById("appliance-address");
    if (addr) addr.textContent = window.location.host;
  }

  private bindEvents(): void {
    // A "hide inactive channels" change saved in another tab applies here at
    // once (the storage event fires only in the other tabs), including in an
    // open settings form's switch. A cleared storage (key null) resets every
    // card to the default.
    onPrefChange((key) => hideInactivePrefDevice(key) !== null, ({ key, newValue }) => {
      const id = hideInactivePrefDevice(key);
      const hide = parseBoolPref(newValue, true);
      let changed = false;
      for (const entry of this.cards.values()) {
        if (id !== null && entry.device.device !== id) continue;
        if (entry.hideInactive === hide) continue;
        entry.hideInactive = hide;
        entry.panel.setHideInactive(hide);
        changed = true;
      }
      if (changed) this.render();
    });
    // The view is a function of store state: devices, status and config each
    // trigger a full render() that reads store.getState(), rather than each
    // patching its own subset of the DOM. status is stored first because URLs
    // and the lock tag depend on it; config now arrives on every poll (see the
    // store) so an out-of-band change reflects within one interval.
    store.on("devices", () => this.render());
    store.on("config", () => this.render());
    store.on("status", (status) => {
      this.status = status;
      this.updateTelemetryFromStatus();
      this.render();
    });
    store.on("system", (system) => {
      this.updateTelemetryFromSystem(system);
    });
    // The meters draw only while the dashboard shows, and levels stream only
    // while it shows or was left less than LEVELS_GRACE_MS ago; until then
    // they keep the meters' state current, so a quick return shows no stale
    // needle. When the store drops them, or the stream goes down, every meter
    // clears its bar and needle and shows a waiting readout until levels come
    // back (a clip latch stays for the operator).
    // A live stream that stops carrying levels without an error (a dead
    // link) clears them too, after LEVELS_STALE_MS (see LevelsWatch).
    const watch = new LevelsWatch({ timers: window, now: () => performance.now(), clear: () => this.clearMeters() });
    followDashboardRoute(router, {
      setFramesSuspended: (suspended) => meterFrames.setSuspended(suspended),
      setLevelsWanted: (wanted) => store.setLevelsWanted(wanted),
      setWatched: (shown) => watch.setWatched(shown),
    });
    followLevels(store, watch);
    store.on("levels", (levels) => {
      routeLevels(this.liveTargets(), levels, METER_LEVELS);
    });
    store.on("available", (available) => {
      this.renderAvailable(available);
    });
    // A drop reads "Reconnecting" at once, even for a proxy that recycles
    // the stream and reconnects within a second: the meters clear with it
    // (the watch above), and an indicator that said "Online" over cleared
    // meters would contradict them.
    store.on("connection", (connected) => this.updateConnection(connected));
    store.on("loaderror", (failure) => {
      if (failure.coreFailed) this.renderLoadError(failure.message);
    });
  }

  // liveTargets lists the serving cards' meters for routeLevels.
  private *liveTargets(): Generator<LevelsTarget<VUMeter>> {
    for (const entry of this.cards.values()) {
      if (entry.live) yield { name: entry.device.name, meters: entry.live.meterConsole.meters };
    }
  }

  // clearMeters puts every meter in its waiting state when levels stop
  // arriving, so a stale bar does not pass for live signal.
  private clearMeters(): void {
    for (const entry of this.cards.values()) entry.live?.meterConsole.clear();
  }

  // renderLoadError replaces the "Loading..." placeholder with the failure cause
  // and a Retry button when the initial data fetch fails, so the view is not
  // stuck loading forever. A successful retry re-renders via the devices event.
  private renderLoadError(message: string): void {
    if (!this.emptyEl) return;
    renderLoadError(this.emptyEl, message, "Loading devices...", () => void store.retry());
  }

  // render coalesces the up-to-three store events per 3 s poll tick (devices,
  // status and config each request it) into a single reconcile on the microtask
  // queue, so same-tick events collapse into one pass instead of three. The pass
  // is idempotent and diffed, so this only drops redundant CPU. Focus
  // restoration lives in the mutation handlers (which read the live nodes) and in
  // mount(); both run inside microtask timing during their awaits, so coalescing
  // does not disturb them.
  private render(): void {
    if (this.renderScheduled) return;
    this.renderScheduled = true;
    queueMicrotask(() => {
      this.renderScheduled = false;
      this.reconcile();
    });
  }

  // reconcile is the single reconcile pass, driven by store state. For each
  // runtime device it gets or creates the stable entry, rebuilds the article only
  // when the shape changed, then syncs every field through the one write path. It
  // then removes gone cards, orders the rack with a diff (no DOM move in steady
  // state, which is what keeps keyboard focus from being dropped every poll),
  // and reconciles open forms.
  private reconcile(): void {
    if (!this.rack) return;
    // Capture the narrowed rack: the intervening syncCard/mount calls below make
    // TS re-widen this.rack to include null, so hold a non-null local for the
    // ordering pass rather than re-guarding it.
    const rack = this.rack;
    const devices = store.getState().devices;

    if (this.emptyEl) {
      this.emptyEl.hidden = devices.length > 0;
      // Clear the alert live-region role set by renderLoadError so the benign
      // empty/loaded state is not re-announced as an error.
      this.emptyEl.removeAttribute("role");
      // Replace the static "Loading devices..." placeholder once we know there
      // are genuinely zero configured devices, and point to the next step: the
      // Available Devices section below is where a detected device is enabled.
      if (devices.length === 0) {
        const hasAvailable = store.getState().available.length > 0;
        setText(this.emptyEl, hasAvailable
          ? "No capture devices are configured yet. Enable one from Available Devices below to start streaming."
          : "No capture devices are configured. Connect capture hardware; it appears under Available Devices below, ready to enable.");
      }
    }

    // Index the persisted config by device id once per pass so the per-card
    // reconcile is a Map lookup rather than a linear scan of the config list.
    const cfgByDevice = new Map<string, DeviceConfig>();
    for (const cd of store.getState().config?.devices ?? []) cfgByDevice.set(cd.device, cd);

    const seen = new Set<string>();
    let clientCount = 0;

    for (const d of devices) {
      seen.add(d.device);
      if (d.clientConnected) clientCount++;

      let entry = this.cards.get(d.device);
      if (!entry) {
        entry = this.newEntry(d);
        this.cards.set(d.device, entry);
      } else if (entry.shape !== shapeKey(d)) {
        this.mount(entry, d);
      }
      this.syncCard(entry, d, cfgByDevice);
    }

    // Remove cards for devices that are gone. A removed card that held
    // keyboard focus (another tab removed the device, or a reload dropped it)
    // hands it on below, as Available Devices does.
    let focusLost: { neighbours: string[]; name: string; own: boolean } | null = null;
    for (const [id, entry] of this.cards) {
      if (!seen.has(id)) {
        if (holdsFocus(entry.article)) {
          // The screen order, read only when a removed card held focus.
          const idOf = new Map([...this.cards].map(([cid, e]) => [e.article, cid] as const));
          const shownIds = [...rack.querySelectorAll<HTMLElement>(":scope > article.rack-card")].flatMap((a) => idOf.get(a) ?? []);
          focusLost = { neighbours: neighbourOrder(shownIds, shownIds.indexOf(id)), name: entry.device.name, own: entry.panel.removing };
        }
        entry.live?.meterConsole.destroy();
        entry.panel.destroy();
        entry.article.remove();
        this.cards.delete(id);
      }
    }

    // Order the rack to match the device list with a diff. Walk the articles by
    // previous sibling rather than indexing this.rack.children: #channel-rack
    // also holds the hidden #rack-empty placeholder, so an index-based compare was
    // off by one and moved a card every poll. Steady state performs no DOM moves,
    // so focus inside a card is never dropped by re-inserting its node.
    const ordered = devices.flatMap((d) => this.cards.get(d.device) ?? []);
    orderChildren(rack, ordered.map((e) => e.article));
    if (focusLost && focusDropped()) {
      // The card's own Remove says what went in its toast, so this says
      // only where focus went.
      const { name, own } = focusLost;
      this.focusNeighbour(focusLost.neighbours, (nid) => this.settingsTarget(nid), (next) => (own ? focusMovedMessage(next) : deviceGoneMessage(name, next)));
    }

    // Reconcile any open settings form against the (possibly refreshed) config.
    for (const entry of this.cards.values()) {
      if (entry.panel.expanded) entry.panel.sync(cfgByDevice.get(entry.device.device));
    }

    const clientsEl = document.getElementById("total-clients-display");
    if (clientsEl) setText(clientsEl, String(clientCount));
  }

  // renderAvailable lists the host's detected-but-unconfigured capture devices,
  // each with an Enable button that provisions it. The whole section hides when
  // nothing is available, so a fully configured host shows no empty panel.
  // Cards are keyed by device id and rebuilt only when what they show changed
  // (availablePlan), so a render keeps the operator's text selection (the
  // device id is there to be copied) and keyboard focus on the cards it leaves
  // alone. A rebuilt card that held focus hands it to its new Enable button.
  private renderAvailable(available: AvailableDevice[]): void {
    if (!this.availableRack || !this.availableSection) return;
    const rack = this.availableRack;
    this.availableSection.hidden = available.length === 0;
    const next = available.map((d) => ({ d, id: d.device, key: availableCardKey(d, this.provisioning.has(d.device)) }));
    const shown = new Map([...this.availableCards].map(([id, c]) => [id, c.key]));
    const plan = availablePlan(shown, next);

    // A removed card that held focus: during its own Enable the Enable moves
    // focus once it settles; otherwise (the device went away) focus goes to
    // its nearest neighbour still listed, below. availableCards is kept in
    // screen order (see the end of this render), so its keys are the order
    // on screen.
    let stranded: { neighbours: string[]; label: string } | null = null;
    const oldOrder = [...this.availableCards.keys()];
    for (const id of plan.remove) {
      const c = this.availableCards.get(id);
      if (!c) continue;
      if (holdsFocus(c.card)) {
        const lost = { id, neighbours: neighbourOrder(oldOrder, oldOrder.indexOf(id)), label: c.label };
        if (this.provisioning.has(id)) this.availableFocusLost = lost;
        else stranded = lost;
      }
      c.card.remove();
      this.availableCards.delete(id);
    }
    const build = new Set(plan.build);
    for (const { d, id, key } of next) {
      if (!build.has(id)) continue;
      const old = this.availableCards.get(id);
      const card = this.buildAvailableCard(d);
      const refocus = old !== undefined && holdsFocus(old.card);
      if (old) old.card.replaceWith(card);
      this.availableCards.set(id, { card, key, label: availableLabel(d) });
      if (refocus) card.querySelector<HTMLElement>(".available-enable")?.focus({ preventScroll: true });
    }
    // Order with a diff, as the device rack does: steady state moves no node.
    // The map is rebuilt in the same order, so its order is the screen's.
    const ordered = new Map(plan.order.flatMap((id) => {
      const c = this.availableCards.get(id);
      return c ? [[id, c] as const] : [];
    }));
    this.availableCards = ordered;
    orderChildren(rack, [...ordered.values()].map((c) => c.card));

    if (stranded) this.focusAvailableNeighbour(stranded.neighbours, stranded.label);
  }

  // takeAvailableFocusLost returns and clears the record of a focused card
  // that a render removed during this device's Enable, if there is one.
  private takeAvailableFocusLost(device: string): { id: string; neighbours: string[]; label: string } | null {
    const lost = this.availableFocusLost;
    if (lost?.id !== device) return null;
    this.availableFocusLost = null;
    return lost;
  }

  // focusAvailableNeighbour moves focus, after an Available card holding it
  // went away, to the Enable button of the first of its neighbours still
  // listed (see neighbourOrder), else to the workspace, and announces which
  // device went.
  private focusAvailableNeighbour(neighbours: readonly string[], label: string): void {
    this.focusNeighbour(
      neighbours,
      (id) => {
        const c = this.availableCards.get(id);
        const control = c?.card.querySelector<HTMLElement>(".available-enable");
        return control ? { control, name: c?.label ?? "" } : undefined;
      },
      (next) => availableGoneMessage(label, next),
    );
  }

  // focusNeighbour moves focus to the control of the first of ids still
  // shown (for a card that went, its neighbours from neighbourOrder; after
  // an Enable, the new card), else to the workspace, and announces
  // message(that card's name, or null).
  private focusNeighbour(
    neighbours: readonly string[],
    find: (id: string) => { control: HTMLElement; name: string } | undefined,
    message: (next: string | null) => string,
  ): void {
    const next = neighbours.map(find).find((n) => n !== undefined);
    if (next) {
      // The control may be out of view (the removed card was last and the
      // one before it is tall), so bring it just into view.
      next.control.focus({ preventScroll: true });
      next.control.scrollIntoView({ block: "nearest" });
    } else {
      focusWorkspace();
    }
    announce(this.announceEl, message(next?.name ?? null));
  }

  private buildAvailableCard(d: AvailableDevice): HTMLElement {
    const card = elem("div", "config-device-card available-card");
    const info = elem("div", "available-info");
    // Fall back to the short ALSA address, not the long stable id, when the card
    // has no friendly name: the address is what the rest of the card shows.
    info.appendChild(elem("div", "device-title", d.friendlyName || d.hwAddr || d.device));
    const sub = elem("div", "available-sub");
    sub.appendChild(elem("span", "mono", d.hwAddr ?? d.device));
    if (d.idStable === false) sub.appendChild(elem("span", "available-caps", "no stable ID"));
    const caps = capsSummary(d);
    if (caps) sub.appendChild(elem("span", "available-caps", caps));
    info.appendChild(sub);
    // The id provisioning persists, shown as selectable text rather than a
    // tooltip (unreachable by keyboard, touch and screen readers), so an operator
    // can tell which of two identical units (serial or port) this card binds.
    if (d.hwAddr && d.device !== d.hwAddr) {
      info.appendChild(elem("div", "available-id mono", deviceIdTitle(d.device)));
    }

    const enableBtn = button({ variant: "primary", extraClass: "available-enable", label: "Enable" });
    // Name the device in the accessible label: there is one Enable button per
    // available device, so a bare "Enable" is ambiguous to a screen-reader user.
    // Two identical units share a friendly name, so add the address to tell
    // their buttons apart.
    setEnableBusy(enableBtn, d, this.provisioning.has(d.device));
    enableBtn.addEventListener("click", () => void this.provisionDevice(d, enableBtn));

    card.append(info, enableBtn);
    return card;
  }

  // settingsTarget is a device card's settings button as a focus target.
  private settingsTarget(id: string): { control: HTMLElement; name: string } | undefined {
    const e = this.cards.get(id);
    return e && { control: e.settingsBtn, name: e.device.name };
  }

  private async provisionDevice(d: AvailableDevice, btn: HTMLElement): Promise<void> {
    if (this.provisioning.has(d.device)) return;
    this.provisioning.add(d.device);
    setEnableBusy(btn, d, true);
    try {
      // Serialize through the same queue as toggles and settings saves: those
      // submit a full-array PATCH built from the cached config, so a provision
      // running concurrently could be clobbered by a stale PATCH (or vice versa).
      // The refreshes run inside the task so the next queued mutation rebuilds
      // from the post-provision config.
      await this.queue.enqueue(async () => {
        let created: Device | undefined;
        let read: { devices: boolean; all: boolean } | undefined;
        try {
          created = await api.provisionDevice({ device: d.device });
        } catch (err: unknown) {
          if (isRefusal(err)) throw err;
          // No readable answer: re-read inside the queue, so the next queued
          // change builds from what the appliance now holds, and judge by it.
          read = await this.queue.refreshDeviceViews();
          created = store.getState().devices.find((dv) => dv.device === d.device);
          if (!created) {
            if (read.devices) showToast(`${availableLabel(d)} does not appear to have been enabled; check again shortly.`, "warn");
            else showUnconfirmed(`that ${availableLabel(d)} was enabled`, "the device list could not be read; check it before trying again");
            return;
          }
        }
        // A re-read that found the device already refreshed everything.
        read ??= await this.queue.refreshDeviceViews();
        if (!read.all) {
          showToast(`Enabled ${created.name}. The dashboard could not be refreshed; it updates on the next poll.`, "warn");
        } else {
          const ch = created.channels.length === 1 ? ` on channel ${created.channels[0]}` : "";
          showToast(`Enabled ${created.name}${ch}. Streaming on ${created.path}.`);
        }
        // The refresh removed this device's Available card. If it held focus
        // and focus has not moved since (it fell to the document body), hand
        // it to the new device card, else to the workspace region, and say
        // where it went.
        const lost = this.takeAvailableFocusLost(d.device);
        if (lost && focusDropped()) {
          // Reconcile now rather than rely on the render the refresh queued:
          // the new card's settings button must exist before focus moves.
          this.reconcile();
          const newId = created.device;
          this.focusNeighbour([newId], (id) => this.settingsTarget(id), (next) => focusMovedMessage(next));
        }
      });
    } catch (err: unknown) {
      // Only a refusal reaches here: an unknown outcome is settled in the task.
      apiErrorToast(err, `Could not enable ${availableLabel(d)}`);
      // A 404 (the device left or was re-detected) or 409 (already set up)
      // means this card is stale; refresh now rather than at the next poll.
      if (isRefusal(err) && (err.status === 404 || err.status === 409)) void store.refreshAvailable();
    } finally {
      this.provisioning.delete(d.device);
      // Set only if the Enable failed after a render took its focused card:
      // focus goes to its own card when it is listed again, else to its
      // nearest neighbour still listed, as for any card that went.
      const lost = this.takeAvailableFocusLost(d.device);
      setEnableBusy(btn, d, false);
      // clearBusy restores the card built before the click. If a render
      // during the Enable rebuilt the card busy instead (its key includes the
      // in-flight state), this render rebuilds it idle, since the key changed
      // back. A device no longer listed (the usual success) has no card left.
      this.renderAvailable(store.getState().available);
      if (lost && focusDropped()) {
        // Listed again by now (a poll brought it back): its own card takes
        // focus; otherwise its nearest neighbour still listed.
        const own = this.availableCards.get(d.device)?.card.querySelector<HTMLElement>(".available-enable");
        if (own) own.focus({ preventScroll: true });
        else this.focusAvailableNeighbour(lost.neighbours, lost.label);
      }
    }
  }

  private rtspUrl(d: Device): string {
    const port = rtspPort(this.status?.rtspListen);
    return `rtsp://${window.location.hostname}:${port}${d.path}`;
  }

  // newEntry creates the stable identity for a device: the settings panel node
  // (which outlives article rebuilds) plus a first built article, assembled into
  // a fully-typed CardEntry in one checked literal (no partial `as` cast).
  private newEntry(d: Device): CardEntry {
    const parts = this.buildArticle(d);
    const entry: CardEntry = {
      ...parts,
      device: d,
      shape: shapeKey(d),
      panel: new SettingsPanel({
        queue: this.queue,
        announceEl: this.announceEl,
        device: () => entry.device,
        hideInactive: () => entry.hideInactive,
        setHideInactive: (hide) => {
          entry.hideInactive = hide;
          writeBoolPref(hideInactiveKey(entry.device.device), hide);
          this.render();
        },
        expandedChanged: () => {
          entry.article.classList.toggle("expanded", entry.panel.expanded);
          this.syncSettingsButton(entry);
        },
        settingsButton: () => entry.settingsBtn,
      }),
      hideInactive: readBoolPref(hideInactiveKey(d.device), true),
    };
    this.wireArticleHandlers(entry);
    entry.article.appendChild(entry.panel.el);
    return entry;
  }

  // mount rebuilds the article for a device's current shape, moving the owned
  // settings panel into the new article and preserving keyboard focus across the
  // swap. It is called whenever shapeKey changes (serving <-> idle, or a change in
  // the captured channel count); the first article is built directly in newEntry.
  private mount(entry: CardEntry, d: Device): void {
    const saved = this.captureFocus(entry);
    const oldArticle = entry.article;
    // Take the old serving body's meters off the frame loop before the article
    // is discarded.
    entry.live?.meterConsole.destroy();

    // Refresh the article and its named nodes in one checked assignment, then
    // re-wire the fresh controls to the stable entry.
    Object.assign(entry, this.buildArticle(d));
    this.wireArticleHandlers(entry);
    // Move the owned settings panel (and its live form, if open) into the new
    // article, and restore its expanded visual state.
    entry.article.appendChild(entry.panel.el);
    if (entry.panel.expanded) entry.article.classList.add("expanded");
    entry.shape = shapeKey(d);

    // Swap in place if the card was already mounted in the rack; otherwise the
    // ordering pass in reconcile() inserts it.
    if (oldArticle.parentNode) oldArticle.replaceWith(entry.article);
    this.restoreFocus(entry, saved, d.state === "serving");
  }

  // syncSettingsButton reflects whether the settings panel is open on the
  // disclosure button: aria-expanded for assistive tech, the chevron flip and
  // accent via the card's .expanded class. It runs wherever expanded changes
  // and after every rebuild, which creates a fresh button.
  private syncSettingsButton(entry: CardEntry): void {
    entry.settingsBtn.setAttribute("aria-expanded", String(entry.panel.expanded));
    entry.settingsBtn.setAttribute("aria-controls", entry.panel.el.id);
  }

  // wireArticleHandlers attaches the settings and toggle handlers to a freshly built
  // article's controls. They close over the stable entry (not the disposable
  // nodes), so a later rebuild simply re-wires the new nodes to the same entry.
  private wireArticleHandlers(entry: CardEntry): void {
    entry.settingsBtn.addEventListener("click", () => entry.panel.toggle());
    entry.toggleInput.addEventListener("change", () => void this.handleToggleEnabled(entry));
    if (entry.togglePending) {
      // Show the change asked for, not the unchecked default of a new switch.
      entry.toggleInput.checked = entry.toggleWant ?? entry.toggleInput.checked;
      entry.toggleInput.disabled = true;
      entry.toggleInput.setAttribute("aria-busy", "true");
    }
    this.syncSettingsButton(entry);
  }

  // buildArticle creates the DOM skeleton for a device's shape with NO device
  // data written: header nodes with empty text, chips and lock hidden, toggle
  // unchecked, the body for the shape. It returns the node bundle; syncCard fills
  // every value and wireArticleHandlers binds the controls to the stable entry.
  // Trusted static SVG for the copy, lock and settings icons is assigned here; the
  // avatar icon depends on state and is written by syncCard.
  private buildArticle(d: Device): ArticleParts {
    const serving = d.state === "serving";
    const article = elem("article", "rack-card");

    // Header
    const header = elem("div", "rack-header");
    const ident = elem("div", "device-ident");
    const avatar = elem("span", "device-avatar");
    // Decorative: the avatar icon repeats the state shown by the status badge.
    avatar.setAttribute("aria-hidden", "true");
    const nameBlock = elem("div", "device-name-block");
    const titleEl = elem("span", "device-title");
    const hwEl = elem("span", "device-path mono");
    nameBlock.appendChild(titleEl);
    nameBlock.appendChild(hwEl);
    ident.appendChild(avatar);
    ident.appendChild(nameBlock);

    const tags = elem("div", "device-tags");
    const modeTag = elem("span", "tech-tag");
    const rateTag = elem("span", "tech-tag");
    const chTag = elem("span", "tech-tag");
    // "Token" marks a stream that needs the access token, and copies it: the
    // same feedback as Copy URL. Its aria-label names the action; the title
    // adds why the token matters for a pointer user.
    const lockEl = elem("button", "tech-tag lock-tag");
    lockEl.setAttribute("type", "button");
    lockEl.setAttribute("aria-label", TOKEN_ARIA);
    lockEl.title = "Pulling this stream requires the access token. Click to copy it.";
    lockEl.dataset.focus = "token";
    // Build it with the SAME predicate syncCard uses (serving and auth required)
    // rather than always hidden: mount() runs restoreFocus BEFORE the following
    // syncCard, so a keyboard user who was on the Token tag before a rebuild
    // lands back on it when it should be visible, and still falls back cleanly to
    // the settings button when it should not.
    lockEl.hidden = !(serving && !!this.status?.authRequired);
    lockEl.appendChild(iconSpan(ICON_LOCK, "icon-copy"));
    lockEl.appendChild(elem("span", "copy-label", TOKEN_LABEL));
    // aria-label names the action ("Copy access token"); a described-by span adds
    // why the token matters, which a title attribute alone does not reliably
    // reach a screen-reader user.
    const tokenDesc = elem("span", "visually-hidden", "Pulling this stream requires the access token");
    tokenDesc.id = `token-desc-${++tokenDescSeq}`;
    lockEl.setAttribute("aria-describedby", tokenDesc.id);
    lockEl.appendChild(tokenDesc);
    lockEl.addEventListener("click", () => this.handleCopyToken(lockEl));
    const statusEl = elem("span");
    tags.append(modeTag, rateTag, chTag, lockEl, statusEl);

    // Streaming enable/disable toggle. A disabled device stays configured but is
    // not opened; toggling persists the flag and a config reload applies it at
    // once, starting or stopping the device. Reuses the shared switch style.
    // The visible "Stream" caption keeps the bare track from reading as an
    // unlabeled control; it is hidden from assistive tech because the input is
    // named by an aria-label (syncCard keeps it current across a rename), and it
    // would otherwise be announced twice. The native checkbox exposes its own
    // checked state, so no aria-checked is written.
    const { el: toggleLabel, input: toggleInput } = switchControl({
      ariaLabel: `Stream ${d.name}`,
      caption: "Stream",
      extraClass: "device-toggle",
      title: "Stream this device (applies immediately)",
    });
    toggleInput.dataset.focus = "toggle";
    tags.appendChild(toggleLabel);

    // Settings disclosure. It sits at the right end of the footer, directly
    // above the panel it expands, in both body shapes (a disabled or failed
    // device still needs its settings). syncSettingsButton writes its
    // expanded state.
    const settingsBtn = elem("button", "card-settings-toggle");
    settingsBtn.setAttribute("type", "button");
    settingsBtn.dataset.focus = "settings";
    settingsBtn.appendChild(iconSpan(ICON_SLIDERS, "settings-toggle-icon"));
    settingsBtn.appendChild(elem("span", undefined, "Settings"));
    settingsBtn.appendChild(iconSpan(ICON_CHEVRON, "settings-toggle-chevron"));

    header.appendChild(ident);
    header.appendChild(tags);
    article.appendChild(header);

    // Persistent restart-required banner: shown when the device is still serving
    // while the config now disables it, so a toggle made this session is never
    // silently lost behind a still-live "Serving" card.
    const pendingNote = elem("div", "pending-restart-note");
    pendingNote.setAttribute("role", "status");
    pendingNote.hidden = true;
    article.appendChild(pendingNote);

    let live: LiveBody | null = null;
    let idle: IdleBody | null = null;

    if (serving) {
      // Endpoint strip
      const strip = elem("div", "endpoint-strip");
      const info = elem("div", "endpoint-info");
      info.appendChild(elem("span", "endpoint-label", "RTSP URL:"));
      const urlEl = elem("span", "endpoint-url mono");
      info.appendChild(urlEl);
      const copyBtn = elem("button", "copy-btn");
      copyBtn.setAttribute("type", "button");
      copyBtn.setAttribute("aria-label", COPY_ARIA);
      copyBtn.title = COPY_ARIA;
      copyBtn.dataset.focus = "copy";
      copyBtn.appendChild(iconSpan(ICON_COPY, "icon-copy"));
      copyBtn.appendChild(elem("span", "copy-label", COPY_LABEL));
      copyBtn.addEventListener("click", () => this.handleCopyUrl(copyBtn, urlEl));
      strip.appendChild(info);
      strip.appendChild(copyBtn);
      article.appendChild(strip);

      const meterConsole = new MeterConsole(meterCount(d));
      article.appendChild(meterConsole.el);

      // Footer
      const footer = elem("div", "rack-footer");
      const metrics = elem("div", "stream-metrics");
      const clients = metricItem("Clients:");
      const dropped = metricItem("Dropped Frames:");
      const overruns = metricItem("Capture Overruns:", OVERRUNS_DESCRIPTION);
      metrics.append(clients.item, dropped.item, overruns.item);
      footer.appendChild(metrics);
      const negotiated = elem("div");
      const negotiatedEl = elem("span");
      negotiated.appendChild(negotiatedEl);
      // The negotiated rate and the settings toggle share the right end.
      const footerEnd = elem("div", "rack-footer-end");
      footerEnd.append(negotiated, settingsBtn);
      footer.appendChild(footerEnd);
      article.appendChild(footer);

      live = {
        urlEl,
        clientsEl: clients.value,
        droppedEl: dropped.value,
        overrunsEl: overruns.value,
        negotiatedEl,
        meterConsole,
      };
    } else {
      // Error / skipped / disabled body. The banner is always present and hidden
      // by syncCard when the device has no error, so an error whose text changes
      // while the card stays idle is still reflected in place.
      const banner = elem("div", "error-banner");
      const bannerIcon = iconSpan(ICON_WARN, "error-banner-icon");
      const body = elem("div", "error-banner-body");
      // syncCard titles the banner by the device's down cause.
      const bannerTitle = elem("span", "error-banner-title", downCauseTitle(undefined));
      body.appendChild(bannerTitle);
      const bannerDesc = elem("span", "error-banner-desc");
      body.appendChild(bannerDesc);
      banner.appendChild(bannerIcon);
      banner.appendChild(body);
      banner.hidden = true;
      article.appendChild(banner);

      const footer = elem("div", "rack-footer");
      const footerNote = elem("span");
      footer.appendChild(footerNote);
      footer.appendChild(settingsBtn);
      article.appendChild(footer);

      idle = { banner, bannerIcon, bannerTitle, bannerDesc, footerNote };
    }

    return {
      article, avatar, titleEl, hwEl, modeTag, rateTag, chTag, lockEl,
      statusEl, toggleInput, settingsBtn, pendingNote, live, idle,
    };
  }

  // syncCard is the ONE write path for device and config data. It runs right
  // after every build and on every render, writing each dynamic field through a
  // diffed helper (setText / setHidden / classList.toggle) so a steady state
  // does not dirty the DOM or re-announce a live region. A field that exists in
  // the DOM but is not written here renders blank, making an omission a visible
  // defect rather than a silent staleness bug.
  private syncCard(entry: CardEntry, d: Device, cfgByDevice: Map<string, DeviceConfig>): void {
    entry.device = d;
    const serving = d.state === "serving";
    const disabled = d.state === "disabled";
    const isUltra = d.mode === "pcm";
    // The toggle reflects the persisted (desired) enabled flag, which can differ
    // from the runtime state only briefly while a config reload applies. Fall
    // back to the runtime state only when the config is not loaded.
    const cfgDev = cfgByDevice.get(d.device);
    const configEnabled = cfgDev?.enabled ?? runtimeEnabled(d.state);

    // A disabled device is off by intent, not broken: neutral styling; a
    // failed/skipped device gets the error styling.
    entry.article.classList.toggle("active-stream", serving);
    entry.article.classList.toggle("error-stream", !serving && !disabled);

    // Avatar icon depends on state and mode; rewrite innerHTML only when the icon
    // actually changes (keyed by data-icon) so we do not reparse SVG every poll.
    const iconKey = serving ? (isUltra ? "ultra" : "mic") : disabled ? "mic" : "error";
    if (entry.avatar.dataset.icon !== iconKey) {
      entry.avatar.dataset.icon = iconKey;
      entry.avatar.innerHTML = serving ? (isUltra ? ICON_ULTRA : ICON_MIC) : disabled ? ICON_MIC : ICON_ERROR;
    }
    const avatarColor = serving && isUltra ? "var(--ultrasonic-purple)" : !serving && !disabled ? "var(--signal-crit)" : "";
    if (entry.avatar.style.color !== avatarColor) entry.avatar.style.color = avatarColor;

    setText(entry.titleEl, d.name);
    // Show the hardware the configured id resolves to right now: its current ALSA
    // address and the sound card's model (friendlyName). When the id resolved to
    // no single present device the address is absent: a serving card-index device
    // opened without a resolution (the container fallback) still shows its
    // configured id, and anything else shows "No matching hardware". Omit the
    // model when absent or when it only repeats the configured name. The persisted
    // id is long, so it goes in the tooltip rather than the line.
    const hw = d.friendlyName?.trim();
    const showHw = !!hw && hw.toLowerCase() !== d.name.trim().toLowerCase();
    const addr = d.hwAddr ? `ALSA: ${d.hwAddr}` : serving ? `ALSA: ${d.device}` : "No matching hardware";
    let hwText = showHw ? `${addr} · ${hw}` : addr;
    // A card-index id can name a different device after a reboot or replug; the
    // settings panel's Device ID hint carries the remedy (remove and re-add).
    if (d.idStable === false) hwText += " · card index (can change after a reboot)";
    setText(entry.hwEl, hwText);
    const hwTitle = deviceIdTitle(d.device);
    if (entry.hwEl.title !== hwTitle) entry.hwEl.title = hwTitle;

    // Chips describe the live stream and are shown only while serving.
    const rate = d.negotiatedRate ?? d.rate;
    setText(entry.modeTag, modeLabel(d.mode));
    entry.modeTag.classList.toggle("ultrasonic", isUltra);
    entry.modeTag.classList.toggle("highlight", !isUltra);
    setHidden(entry.modeTag, !serving);
    setText(entry.rateTag, `${rate.toLocaleString("en-US")} Hz`);
    setHidden(entry.rateTag, !serving);
    // Show every streamed channel (the union across the device's streams), so the
    // header channel tag agrees with the per-channel tally lights below rather
    // than showing only the first stream's channels (d.channels).
    const chLabel = channelLabel(d.streamedChannels ?? d.channels);
    setText(entry.chTag, chLabel);
    setHidden(entry.chTag, !serving || !chLabel);
    // The Token tag hides in place when access control is turned off elsewhere
    // (the card shape does not change, so mount's focus capture never runs);
    // if it held focus, keep focus on the card and say why it moved.
    const hideLock = !serving || !this.status?.authRequired;
    const lockHadFocus = hideLock && !entry.lockEl.hidden && holdsFocus(entry.lockEl);
    setHidden(entry.lockEl, hideLock);
    if (lockHadFocus) {
      entry.settingsBtn.focus();
      announce(this.announceEl, tokenHiddenMessage(d.name));
    }

    const badge = deviceStateBadge(d.state);
    if (entry.statusEl.className !== badge.cls) entry.statusEl.className = badge.cls;
    setText(entry.statusEl, badge.label);

    const toggleAria = `Stream ${d.name}`;
    if (entry.toggleInput.getAttribute("aria-label") !== toggleAria) entry.toggleInput.setAttribute("aria-label", toggleAria);
    // The settings disclosure reads "Settings" for every card; name the device so
    // a screen-reader user can tell which card's settings the button opens.
    const settingsAria = `Settings for ${d.name}`;
    if (entry.settingsBtn.getAttribute("aria-label") !== settingsAria) entry.settingsBtn.setAttribute("aria-label", settingsAria);
    // Do not fight the user mid-interaction (a PATCH is queued or in flight);
    // otherwise keep it in sync with the persisted flag.
    if (!entry.togglePending && entry.toggleInput.checked !== configEnabled) {
      entry.toggleInput.checked = configEnabled;
    }

    const showPending = pendingStop(configEnabled, d.state);
    setText(entry.pendingNote, showPending ? PENDING_STOP_TEXT : "");
    setHidden(entry.pendingNote, !showPending);

    if (entry.live) {
      const url = this.rtspUrl(d);
      setText(entry.live.urlEl, url);
      if (entry.live.urlEl.title !== url) entry.live.urlEl.title = url;
      const counters = footerMetrics(d);
      setText(entry.live.clientsEl, counters.clients);
      setText(entry.live.droppedEl, counters.dropped);
      setText(entry.live.overrunsEl, counters.overruns);
      const negFormat = d.negotiatedFormat ? ` · ${captureFormatLabel(d.negotiatedFormat)}` : "";
      setText(entry.live.negotiatedEl, `Negotiated: ${rate.toLocaleString("en-US")} Hz${negFormat}`);
      // The label is the hardware capture format; the RTSP stream is always 16-bit,
      // so name that in a tooltip rather than let "· 24-bit" read as the stream depth.
      const negTitle = d.negotiatedFormat
        ? "Hardware capture format. The RTSP stream is 16-bit; a wider capture is downconverted."
        : "";
      if (entry.live.negotiatedEl.title !== negTitle) entry.live.negotiatedEl.title = negTitle;
      // The tally lights follow every streamed channel (the union across the
      // device's streams), as the header channel tag does.
      entry.live.meterConsole.sync(d.streamedChannels ?? d.channels, entry.hideInactive, entry.settingsBtn, this.announceEl);
    }
    if (entry.idle) {
      setHidden(entry.idle.banner, !d.error);
      const isError = bannerIsError(d.state, d.downCause);
      const bannerKey = isError ? "error" : "warn";
      if (entry.idle.bannerIcon.dataset.icon !== bannerKey) {
        entry.idle.bannerIcon.dataset.icon = bannerKey;
        entry.idle.bannerIcon.innerHTML = isError ? ICON_ERROR : ICON_WARN;
      }
      setText(entry.idle.bannerTitle, downCauseTitle(d.downCause));
      setText(entry.idle.bannerDesc, d.error ?? "");
      setText(entry.idle.footerNote, nonServingFooterText(d.state, configEnabled));
    }
  }

  // captureFocus records where keyboard focus is inside a card before its article
  // is rebuilt, so restoreFocus can put it back. Focus inside the settings panel
  // is remembered by element identity (the panel is moved, not rebuilt); focus on
  // a rebuilt control is remembered by its data-focus key (toggle, settings, copy,
  // token, clip-N), which the new article recreates.
  private captureFocus(entry: CardEntry): { el?: HTMLElement; key?: string } | null {
    // captureFocus runs only from mount(), which rebuilds an existing article, so
    // entry.article is always present (the first article is built in newEntry).
    const art = entry.article;
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || !art.contains(active)) return null;
    if (entry.panel.el.contains(active)) return { el: active };
    const key = active.dataset.focus;
    return key ? { key } : null;
  }

  // serving is the device's state in the rebuilt shape (mount runs before
  // syncCard updates entry.device).
  private restoreFocus(entry: CardEntry, saved: { el?: HTMLElement; key?: string } | null, serving: boolean): void {
    if (!saved) return;
    if (saved.el) {
      // The panel node was moved into the new article and is connected again.
      if (saved.el.isConnected) saved.el.focus();
      return;
    }
    if (saved.key) {
      const node = entry.article.querySelector<HTMLElement>(`[data-focus="${saved.key}"]`);
      // A control that is gone or hidden in the rebuilt shape (copy and clip-N
      // after a flip to idle, or the token tag when the stream no longer needs
      // the token) cannot take focus; keep focus on the card via the settings
      // button rather than letting it fall to <body>, and say why it moved.
      if (node && !node.hidden && !node.closest("[hidden]")) {
        node.focus();
      } else {
        entry.settingsBtn.focus();
        // The token message only when the tag went because the token is no
        // longer needed; a device that stopped serving loses it too, and there
        // the token is still required.
        const tokenDropped = saved.key === "token" && serving && !this.status?.authRequired;
        const name = entry.device.name;
        announce(this.announceEl, tokenDropped ? tokenHiddenMessage(name) : controlGoneMessage(name, saved.key, serving));
      }
    }
  }

  // handleToggleEnabled persists a device's streaming enable/disable flag. The
  // change is hot-applied to the running pipeline (the device is started or
  // stopped in place, other devices keep serving), so it takes effect at once;
  // the toggle reflects the desired state immediately and reverts if the PATCH is
  // rejected.
  private async handleToggleEnabled(entry: CardEntry): Promise<void> {
    const input = entry.toggleInput;
    const want = input.checked;
    const id = entry.device.device;
    const name = entry.device.name;
    // Refuse to mutate the device list from the runtime fallback: until GET
    // /config has loaded, the queue's base() projects via deviceToConfig, which
    // omits config-only fields (quietAlert), so a full-array PATCH would reset
    // every device's opt-out. config only ever goes null -> loaded, so checking
    // here is equivalent to checking inside the queued task.
    if (!store.getState().config) {
      input.checked = !want;
      showToast("Configuration has not loaded yet. Try again in a moment.", "warn");
      return;
    }
    // Remember focus before disabling: re-enabling a disabled control drops focus
    // to the body, dumping a keyboard user at the top of the page.
    const hadFocus = document.activeElement === input;
    entry.togglePending = true;
    entry.toggleWant = want;
    input.disabled = true;
    input.setAttribute("aria-busy", "true");
    const queue = this.queue;
    await queue.enqueue(async () => {
      // Build merged from a FRESH base inside the queued task, after any prior
      // mutation's PATCH+refresh settled, so this full-array PATCH cannot clobber
      // a concurrent change with a stale base.
      let merged: DeviceConfig[] = [];
      const verb = want ? "Enabled" : "Disabled";
      try {
        if (!(await queue.freshBase())) {
          entry.toggleInput.checked = !want;
          showToast(STALE_BASE_TEXT, "warn");
          return;
        }
        merged = queue.base().map((cd) => (cd.device === id ? { ...cd, enabled: want } : cd));
        const res = await api.patchConfig({ devices: merged });
        // The PATCH persisted: seed the cached config before the refresh (see
        // ConfigQueue.applied).
        queue.applied(res.config);
        // A refresh failure afterwards must NOT revert the toggle: the change is
        // already applied and reflected in the cached config above.
        await Promise.all([store.refreshConfig(), store.refreshDevices()]);
        showToast(
          res.restartRequired
            ? `${verb} ${name}. Restart the appliance to apply.`
            : `${verb} ${name}.`,
        );
      } catch (err: unknown) {
        if (isRefusal(err)) {
          // A refusal did not persist, so the toggle reverts (the live node:
          // a poll may have rebuilt the card meanwhile).
          entry.toggleInput.checked = !want;
          apiErrorToast(err, `Could not ${want ? "enable" : "disable"} ${name}`, merged);
        } else {
          // The change may have persisted: re-read inside the queue, so the
          // next queued change builds from what the appliance holds, and say
          // what the re-read found.
          const read = await queue.refreshConfigViews();
          const now = store.getState().config?.devices.find((cd) => cd.device === id);
          if (!read) showUnconfirmed(`the change to ${name}`, "check the switch before trying again");
          else if ((now?.enabled ?? true) === want) showToast(`${verb} ${name}.`);
          else showToast(`The change to ${name} does not appear to have applied; check again shortly.`, "warn");
        }
      } finally {
        // Re-read the current toggle: a poll may have rebuilt the card during the
        // PATCH (a serving<->idle flip, or a captured-channel change) and replaced
        // the node this closure captured. Clear the busy state and restore focus on
        // the live node, falling back to the settings button if the toggle is gone, so a
        // keyboard user is never stranded on the document body.
        entry.togglePending = false;
        // A card rebuilt while the change was pending skipped the sync, so
        // render once more to set its toggle from the config.
        this.render();
        const toggle = entry.toggleInput;
        toggle.disabled = false;
        toggle.removeAttribute("aria-busy");
        // Only if focus is still on the toggle or dropped: the operator may
        // have moved on while the change was queued.
        if (hadFocus && ((toggle.isConnected && holdsFocus(toggle)) || focusDropped())) (toggle.isConnected ? toggle : entry.settingsBtn).focus();
      }
    });
  }

  // handleCopyUrl copies the stream URL. When the appliance requires the access
  // token and this browser holds it, the copied URL embeds it as RTSP
  // credentials (rtsp://mic:<token>@host:port/path) so it pastes straight into
  // BirdNET-Go, ffmpeg or VLC; the displayed URL stays credential-free. The
  // credentialed form is derived from the displayed URL (which syncCard keeps
  // current), not from a path captured when the card was built, so a device
  // path edit is reflected in the copied URL.
  private handleCopyUrl(btn: HTMLElement, urlEl: HTMLElement | null): void {
    const shown = urlEl?.textContent;
    if (!shown) return;
    let url = shown;
    const token = this.status?.authRequired ? getToken() : null;
    if (token) {
      // Anchor the scheme to the start so only the leading rtsp:// is rewritten,
      // never a literal "rtsp://" that appears later in the path.
      url = shown.replace(/^rtsp:\/\//, `rtsp://mic:${token}@`);
    }
    // Route through the shared clipboard primitive so a plain-http origin (no
    // Clipboard API) reports the same "unavailable" toast as every other Copy
    // button instead of silently doing nothing.
    void writeToClipboard(url).then((result) => {
      if (result !== "ok") {
        reportClipboardFailure(result);
        return;
      }
      if (token) showToast("Stream URL copied with the access token included.");
      // With the token included the toast already announces the copy, so the
      // accessible name stays put rather than announcing it twice.
      flashCopied(btn, btn.querySelector<HTMLElement>(".copy-label"), COPY_LABEL, COPY_ARIA, token ? COPY_ARIA : COPY_ARIA_DONE);
    });
  }

  // handleCopyToken copies the access token this browser signed in with. The
  // tag only shows while the appliance requires a token, so a signed-in browser
  // holds one; without it (or without a clipboard, as on a plain http origin)
  // the operator is told rather than getting a silent no-op.
  private handleCopyToken(btn: HTMLElement): void {
    const token = getToken();
    if (!token) {
      showToast("This browser does not hold the access token. Run remote-mic token get on the appliance.", "warn");
      return;
    }
    // Route through the shared clipboard primitive so an unavailable or failed
    // copy reports the same toast as every other Copy button.
    void writeToClipboard(token).then((result) => {
      if (result !== "ok") {
        reportClipboardFailure(result);
        return;
      }
      showToast("Access token copied.");
      // The toast announces the copy, so the accessible name stays fixed rather
      // than announcing it twice (mirrors the credentialed Copy URL path).
      flashCopied(btn, btn.querySelector<HTMLElement>(".copy-label"), TOKEN_LABEL, TOKEN_ARIA, TOKEN_ARIA);
    });
  }

  private updateTelemetryFromStatus(): void {
    if (!this.status) return;
    const uptimeEl = document.getElementById("uptime-display");
    if (uptimeEl) uptimeEl.textContent = formatUptime(this.status.uptimeSeconds, { seconds: true });

    const servingEl = document.getElementById("devices-serving-display");
    if (servingEl) servingEl.textContent = `${this.status.devicesServing} / ${this.status.devicesTotal}`;

    // The open-access notice shows while no token is configured. Guard the write
    // so this role=status banner is not re-announced on every ~3s poll when its
    // state is unchanged.
    const banner = document.getElementById("open-access-banner");
    if (banner) setHidden(banner, this.status.authRequired);

    const badge = document.getElementById("appliance-status-badge");
    const text = document.getElementById("appliance-status-text");
    if (badge && text) {
      if (this.status.devicesServing > 0) {
        badge.className = "status-badge ok";
        text.textContent = "Streaming";
      } else {
        badge.className = "status-badge crit";
        text.textContent = "No Devices";
      }
    }
  }

  private updateTelemetryFromSystem(sys: SystemInfo): void {
    const cpuEl = document.getElementById("cpu-load-display");
    if (cpuEl) cpuEl.textContent = sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a";
    const tempEl = document.getElementById("soc-temp-display");
    if (tempEl) tempEl.textContent = sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a";
  }

  private updateConnection(connected: boolean): void {
    const indicator = document.getElementById("connection-indicator");
    const text = document.getElementById("connection-text");
    if (text) text.textContent = connected ? "Online" : "Reconnecting";
    if (indicator) indicator.classList.toggle("offline", !connected);
  }
}
