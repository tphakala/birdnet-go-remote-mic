import { store } from "../lib/store.ts";
import { meterFrames, type VUMeter } from "../components/vu-meter.ts";
import { router } from "../lib/router.ts";
import { showToast } from "../components/toast.ts";
import { api, isRefusal } from "../lib/api.ts";
import { announce, button, clearBusy, elem, focusDropped, focusWorkspace, formatUptime, holdsFocus, orderChildren, renderLoadError, setBusy, setHidden, setText, showUnconfirmed } from "../lib/ui.ts";
import { availableCardKey, availableGoneMessage, availablePlan, deviceGoneMessage, focusMovedMessage, neighbourOrder, followDashboardRoute, followLevels, LevelsWatch, routeLevels, type LevelsTarget } from "../lib/dashboard-core.ts";
import { deviceIdTitle } from "../lib/text.ts";
import { hideInactivePrefDevice, onPrefChange, parseBoolPref } from "../lib/prefs.ts";
import type { ApplianceStatus, AvailableDevice, Device, DeviceConfig, SystemInfo } from "../lib/types.ts";
import { apiErrorToast, ConfigQueue } from "./dashboard/config-queue.ts";
import { DeviceCard, type DeviceCardHost } from "./dashboard/device-card.ts";

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

export class DashboardView {
  // Cards keyed by immutable device id (the stable identity). The levels
  // payload is keyed by name, which the levels handler reads from each card.
  private cards: Map<string, DeviceCard> = new Map();
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
  // What each device card needs from the view.
  private readonly cardHost: DeviceCardHost;
  // Set while a reconcile() is queued on the microtask, so the several store
  // events a single poll tick fires collapse into one pass (see render()).
  private renderScheduled = false;

  constructor() {
    this.rack = document.getElementById("channel-rack");
    this.emptyEl = document.getElementById("rack-empty");
    this.availableSection = document.getElementById("available-section");
    this.availableRack = document.getElementById("available-rack");
    this.announceEl = document.getElementById("dashboard-announce");
    this.cardHost = {
      queue: this.queue,
      announceEl: this.announceEl,
      status: () => this.status,
      render: () => this.render(),
    };
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
      this.focusNeighbour(focusLost.neighbours, (nid) => this.settingsTarget(nid), (next) => (own ? focusMovedMessage(next) : deviceGoneMessage(name, next)));
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
    const c = this.cards.get(id);
    return c && { control: c.settingsButton, name: c.device.name };
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
