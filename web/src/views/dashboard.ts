import { store } from "../lib/store.ts";
import { meterFrames, type VUMeter } from "../components/vu-meter.ts";
import { router } from "../lib/router.ts";
import { announce, clearLoadError, focusDropped, focusNeighbour, formatUptime, holdsFocus, renderLoadError, setHidden, setText } from "../lib/ui.ts";
import { clientCountOf, deviceGoneMessage, focusMovedMessage, neighbourOrder, followDashboardRoute, followLevels, LevelsWatch, routeLevels, telemetryReadings, type LevelsTarget } from "../lib/dashboard-core.ts";
import { KeyedList } from "../lib/keyed-list.ts";
import { hideInactivePrefDevice, onPrefChange, parseBoolPref } from "../lib/prefs.ts";
import type { ApplianceStatus, Device, DeviceConfig, SystemInfo } from "../lib/types.ts";
import { AvailableDevices } from "./dashboard/available-devices.ts";
import { ConfigQueue } from "./dashboard/config-queue.ts";
import { DeviceCard, type DeviceCardHost } from "./dashboard/device-card.ts";

// METER_LEVELS is how routeLevels feeds a VUMeter, built once rather than
// per levels event.
const METER_LEVELS = {
  set: (m: VUMeter, rms: number, peak: number, clipped: boolean) => m.setLevels(rms, peak, clipped),
  clear: (m: VUMeter) => m.clearLevels(),
};

// CardSource is what one device card is drawn from: the runtime device and its
// persisted config, if any.
interface CardSource {
  d: Device;
  cfg: DeviceConfig | undefined;
}

// RemovedFocus is a card that held keyboard focus when a sync removed it: its
// neighbours on screen, its name, and whether its own Remove was deleting it.
interface RemovedFocus {
  neighbours: string[];
  name: string;
  own: boolean;
}

export class DashboardView {
  // Cards keyed by immutable device id (the stable identity), in screen order;
  // null without a rack. The levels payload is keyed by name, which the levels
  // handler reads from each card.
  private readonly cards: KeyedList<CardSource, DeviceCard> | null;
  // Set by the cards' remove hook when a removed card held focus, read and
  // cleared after the sync by reconcile.
  private removedFocus: RemovedFocus | null = null;
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
    const rack = this.rack;
    this.cards = rack
      ? new KeyedList<CardSource, DeviceCard>(rack, {
          key: (x) => x.d.device,
          create: (x) => new DeviceCard(x.d, this.cardHost),
          // The card's element changes when it is mounted again for a new shape.
          nodes: (c) => [c.el],
          update: (c, x) => c.update(x.d, x.cfg),
          // A removed card that held keyboard focus (another tab removed the
          // device, or a reload dropped it) hands it on after the sync, as
          // Available Devices does.
          remove: (card, id) => {
            if (holdsFocus(card.el)) {
              // The screen order, read only when a removed card held focus.
              const idOf = new Map([...(this.cards?.entries() ?? [])].map(([cid, c]) => [c.el, cid] as const));
              const shownIds = [...rack.querySelectorAll<HTMLElement>(":scope > article.rack-card")].flatMap((a) => idOf.get(a) ?? []);
              this.removedFocus = { neighbours: neighbourOrder(shownIds, shownIds.indexOf(id)), name: card.device.name, own: card.removing };
            }
            card.destroy();
          },
        })
      : null;
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
      for (const card of this.cards?.values() ?? []) {
        if (id !== null && card.device.device !== id) continue;
        if (card.setHideInactive(hide)) changed = true;
      }
      if (changed) this.render();
    });
    // The view is a function of store state: devices, status, config and
    // available each trigger a full render() that reads store.getState(),
    // rather than each patching its own subset of the DOM. config arrives on
    // every poll (see the store) so an out-of-band change reflects within one
    // interval.
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
      // Only while the rack has no list to show: a load that fails after a
      // list was read (a login's reload) leaves the cards as they are.
      if (!failure.devicesFailed || store.devicesRead() === "loaded") return;
      // A failed /status as well means the appliance is out of reach; with
      // /status answering, only the device list is missing. A poll that later
      // recovers it clears this through reconcile.
      this.renderLoadError(failure.coreFailed ? failure.message : "Could not load the device list.");
      this.available.syncShown();
    });
  }

  // liveTargets lists the serving cards' meters for routeLevels.
  private *liveTargets(): Generator<LevelsTarget<VUMeter>> {
    for (const card of this.cards?.values() ?? []) {
      const target = card.levels();
      if (target) yield target;
    }
  }

  // clearMeters puts every meter in its waiting state when levels stop
  // arriving, so a stale bar does not pass for live signal.
  private clearMeters(): void {
    for (const card of this.cards?.values() ?? []) card.clearLevels();
  }

  // renderLoadError replaces the "Loading..." placeholder with the failure cause
  // and a Retry button when the initial data fetch fails, so the view is not
  // stuck loading forever. A successful retry re-renders via the devices event.
  private renderLoadError(message: string): void {
    if (!this.emptyEl) return;
    renderLoadError(this.emptyEl, message, "Loading devices...", () => void store.retry());
  }

  // render coalesces the store events of a 3 s poll tick (devices, status,
  // config and available each request it) into a single reconcile on the
  // microtask queue, so same-tick events collapse into one pass. The pass is
  // idempotent and diffed, so this only drops redundant CPU. Focus restoration
  // lives in the cards' mutation handlers (which read the live nodes) and in
  // DeviceCard.mount; both run inside microtask timing during their awaits, so
  // coalescing does not disturb them.
  private render(): void {
    if (this.renderScheduled) return;
    this.renderScheduled = true;
    queueMicrotask(() => {
      this.renderScheduled = false;
      this.reconcile();
    });
  }

  // reconcile is the single reconcile pass, driven by store state. The cards
  // are a KeyedList: for each runtime device it gets or creates the stable
  // card and updates it (see DeviceCard.update), removes gone cards and orders
  // the rack with a diff (no DOM move in steady state, which is what keeps
  // keyboard focus from being dropped every poll).
  private reconcile(): void {
    if (!this.rack || !this.cards) return;
    const devices = store.getState().devices;

    // Until a devices read applied, an empty list only means not loaded yet.
    if (this.emptyEl && store.devicesRead() === "loaded") {
      // A poll that recovers the list while Retry holds focus removes the
      // button (the message is rewritten or hidden): park focus on the view
      // (a click on Retry parks it too) and announce the recovery, instead of
      // dropping focus to <body>.
      // clearLoadError also drops the alert live-region role set by
      // renderLoadError, so the benign empty/loaded state is not re-announced
      // as an error.
      const retryHadFocus = clearLoadError(this.emptyEl);
      this.emptyEl.hidden = devices.length > 0;
      // Replace the static "Loading devices..." placeholder once we know there
      // are genuinely zero configured devices, and point to the next step: the
      // Available Devices section below is where a detected device is enabled.
      if (devices.length === 0) {
        const hasAvailable = store.getState().available.length > 0;
        setText(this.emptyEl, hasAvailable
          ? "No capture devices are configured yet. Enable one from Available Devices below to start streaming."
          : "No capture devices are configured. Connect capture hardware; it appears under Available Devices below, ready to enable.");
      }
      if (retryHadFocus) announce(this.announceEl, "Devices loaded.");
    }

    // Index the persisted config by device id once per pass so the per-card
    // reconcile is a Map lookup rather than a linear scan of the config list.
    const cfgByDevice = new Map<string, DeviceConfig>();
    for (const cd of store.getState().config?.devices ?? []) cfgByDevice.set(cd.device, cd);

    let clientCount = 0;
    for (const d of devices) clientCount += clientCountOf(d);

    // #channel-rack also holds the hidden #rack-empty placeholder; the list
    // leaves it after the cards, and in steady state moves no node, so focus
    // inside a card is never dropped by re-inserting its node.
    this.cards.sync(devices.map((d) => ({ d, cfg: cfgByDevice.get(d.device) })));
    const lost = this.takeRemovedFocus();
    if (lost && focusDropped()) {
      // The card's own Remove says what went in its toast, so this says
      // only where focus went.
      const { name, own } = lost;
      focusNeighbour(this.announceEl, lost.neighbours, (nid) => this.settingsTarget(nid), (next) => (own ? focusMovedMessage(next) : deviceGoneMessage(name, next)));
    }

    const clientsEl = document.getElementById("total-clients-display");
    if (clientsEl) setText(clientsEl, String(clientCount));
    this.available.syncShown();
  }

  // takeRemovedFocus returns and clears the record of a focused card the sync
  // in progress removed, if there is one.
  private takeRemovedFocus(): RemovedFocus | null {
    const lost = this.removedFocus;
    this.removedFocus = null;
    return lost;
  }

  // settingsTarget is a device card's settings button as a focus target.
  private settingsTarget(id: string): { control: HTMLElement; name: string } | undefined {
    const c = this.cards?.get(id);
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
    for (const r of telemetryReadings(sys)) {
      const valueEl = document.getElementById(r.valueId);
      if (valueEl) setText(valueEl, r.value);
      const unitEl = document.getElementById(r.unitId);
      if (unitEl) setHidden(unitEl, r.unitHidden);
    }
  }

  private updateConnection(connected: boolean): void {
    const indicator = document.getElementById("connection-indicator");
    const text = document.getElementById("connection-text");
    if (text) text.textContent = connected ? "Online" : "Reconnecting";
    if (indicator) indicator.classList.toggle("offline", !connected);
  }
}
