import { store } from "../lib/store.ts";
import { meterFrames, type VUMeter } from "../components/vu-meter.ts";
import { router } from "../lib/router.ts";
import { focusDropped, focusNeighbour, formatUptime, holdsFocus, orderChildren, renderLoadError, setHidden, setText } from "../lib/ui.ts";
import { deviceGoneMessage, focusMovedMessage, neighbourOrder, followDashboardRoute, followLevels, LevelsWatch, routeLevels, type LevelsTarget } from "../lib/dashboard-core.ts";
import { hideInactivePrefDevice, onPrefChange, parseBoolPref } from "../lib/prefs.ts";
import type { ApplianceStatus, DeviceConfig, SystemInfo } from "../lib/types.ts";
import { AvailableDevices } from "./dashboard/available-devices.ts";
import { ConfigQueue } from "./dashboard/config-queue.ts";
import { DeviceCard, type DeviceCardHost } from "./dashboard/device-card.ts";

// METER_LEVELS is how routeLevels feeds a VUMeter, built once rather than
// per levels event.
const METER_LEVELS = {
  set: (m: VUMeter, rms: number, peak: number, clipped: boolean) => m.setLevels(rms, peak, clipped),
  clear: (m: VUMeter) => m.clearLevels(),
};

export class DashboardView {
  // Cards keyed by immutable device id (the stable identity). The levels
  // payload is keyed by name, which the levels handler reads from each card.
  private cards: Map<string, DeviceCard> = new Map();
  private rack: HTMLElement | null;
  private emptyEl: HTMLElement | null;
  private readonly available: AvailableDevices;
  // The polite status region for focus moves the operator did not make.
  private announceEl: HTMLElement | null;
  // Serializes the device mutations of every card (see ConfigQueue).
  private readonly queue = new ConfigQueue();
  // What each device card needs from the view.
  private readonly cardHost: DeviceCardHost;
  // Set while a reconcile() is queued on the microtask, so the several store
  // events a single poll tick fires collapse into one pass (see render()).
  private renderScheduled = false;

  constructor() {
    this.rack = document.getElementById("channel-rack");
    this.emptyEl = document.getElementById("rack-empty");
    this.announceEl = document.getElementById("dashboard-announce");
    this.cardHost = {
      queue: this.queue,
      announceEl: this.announceEl,
      render: () => this.render(),
    };
    this.available = new AvailableDevices(document.getElementById("available-section"), {
      queue: this.queue,
      announceEl: this.announceEl,
      focusDevice: (id) => {
        // Reconcile now rather than rely on the render the refresh queued:
        // the new card's settings button must exist before focus moves.
        this.reconcile();
        focusNeighbour(this.announceEl, [id], (cid) => this.settingsTarget(cid), focusMovedMessage);
      },
    });
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
      for (const card of this.cards.values()) {
        if (id !== null && card.device.device !== id) continue;
        if (card.setHideInactive(hide)) changed = true;
      }
      if (changed) this.render();
    });
    // The view is a function of store state: devices, status and config each
    // trigger a full render() that reads store.getState(), rather than each
    // patching its own subset of the DOM. config arrives on every poll (see
    // the store) so an out-of-band change reflects within one interval.
    store.on("devices", () => this.render());
    store.on("config", () => this.render());
    store.on("status", (status) => {
      this.updateTelemetryFromStatus(status);
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
    // The rack's empty-state text names Available Devices, so it renders too.
    store.on("available", (available) => {
      this.available.render(available);
      this.render();
    });
    // A drop reads "Reconnecting" at once, even for a proxy that recycles
    // the stream and reconnects within a second: the meters clear with it
    // (the watch above), and an indicator that said "Online" over cleared
    // meters would contradict them.
    store.on("connection", (connected) => this.updateConnection(connected));
    store.on("loaderror", (failure) => {
      if (!failure.coreFailed) return;
      this.renderLoadError(failure.message);
      this.available.syncShown();
    });
  }

  // liveTargets lists the serving cards' meters for routeLevels.
  private *liveTargets(): Generator<LevelsTarget<VUMeter>> {
    for (const card of this.cards.values()) {
      const target = card.levels();
      if (target) yield target;
    }
  }

  // clearMeters puts every meter in its waiting state when levels stop
  // arriving, so a stale bar does not pass for live signal.
  private clearMeters(): void {
    for (const card of this.cards.values()) card.clearLevels();
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
  // runtime device it gets or creates the stable card and updates it (see
  // DeviceCard.update). It then removes gone cards and orders the rack with a
  // diff (no DOM move in steady state, which is what keeps keyboard focus from
  // being dropped every poll).
  private reconcile(): void {
    if (!this.rack) return;
    // Capture the narrowed rack: the intervening card updates below make TS
    // re-widen this.rack to include null, so hold a non-null local for the
    // ordering pass rather than re-guarding it.
    const rack = this.rack;
    const devices = store.getState().devices;

    // Until a devices read applied, an empty list only means not loaded yet.
    if (this.emptyEl && store.devicesRead() === "loaded") {
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

      let card = this.cards.get(d.device);
      if (!card) {
        card = new DeviceCard(d, this.cardHost);
        this.cards.set(d.device, card);
      }
      card.update(d, cfgByDevice.get(d.device));
    }

    // Remove cards for devices that are gone. A removed card that held
    // keyboard focus (another tab removed the device, or a reload dropped it)
    // hands it on below, as Available Devices does.
    let focusLost: { neighbours: string[]; name: string; own: boolean } | null = null;
    for (const [id, card] of this.cards) {
      if (!seen.has(id)) {
        if (holdsFocus(card.el)) {
          // The screen order, read only when a removed card held focus.
          const idOf = new Map([...this.cards].map(([cid, c]) => [c.el, cid] as const));
          const shownIds = [...rack.querySelectorAll<HTMLElement>(":scope > article.rack-card")].flatMap((a) => idOf.get(a) ?? []);
          focusLost = { neighbours: neighbourOrder(shownIds, shownIds.indexOf(id)), name: card.device.name, own: card.removing };
        }
        card.destroy();
        this.cards.delete(id);
      }
    }

    // Order the rack to match the device list with a diff. Walk the articles by
    // previous sibling rather than indexing this.rack.children: #channel-rack
    // also holds the hidden #rack-empty placeholder, so an index-based compare was
    // off by one and moved a card every poll. Steady state performs no DOM moves,
    // so focus inside a card is never dropped by re-inserting its node.
    const ordered = devices.flatMap((d) => this.cards.get(d.device) ?? []);
    orderChildren(rack, ordered.map((c) => c.el));
    if (focusLost && focusDropped()) {
      // The card's own Remove says what went in its toast, so this says
      // only where focus went.
      const { name, own } = focusLost;
      focusNeighbour(this.announceEl, focusLost.neighbours, (nid) => this.settingsTarget(nid), (next) => (own ? focusMovedMessage(next) : deviceGoneMessage(name, next)));
    }

    const clientsEl = document.getElementById("total-clients-display");
    if (clientsEl) setText(clientsEl, String(clientCount));
    this.available.syncShown();
  }

  // settingsTarget is a device card's settings button as a focus target.
  private settingsTarget(id: string): { control: HTMLElement; name: string } | undefined {
    const c = this.cards.get(id);
    return c && { control: c.settingsButton, name: c.device.name };
  }

  private updateTelemetryFromStatus(status: ApplianceStatus): void {
    const uptimeEl = document.getElementById("uptime-display");
    if (uptimeEl) uptimeEl.textContent = formatUptime(status.uptimeSeconds, { seconds: true });

    const servingEl = document.getElementById("devices-serving-display");
    if (servingEl) servingEl.textContent = `${status.devicesServing} / ${status.devicesTotal}`;

    // The open-access notice shows while no token is configured. Guard the write
    // so this role=status banner is not re-announced on every ~3s poll when its
    // state is unchanged.
    const banner = document.getElementById("open-access-banner");
    if (banner) setHidden(banner, status.authRequired);

    const badge = document.getElementById("appliance-status-badge");
    const text = document.getElementById("appliance-status-text");
    if (badge && text) {
      if (status.devicesServing > 0) {
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
