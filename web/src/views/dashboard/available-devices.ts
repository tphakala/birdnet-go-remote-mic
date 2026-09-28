import { showToast } from "../../components/toast.ts";
import { api, isRefusal } from "../../lib/api.ts";
import { availableCardKey, availableGoneMessage, availableLabel, availablePlan, capsSummary, neighbourOrder } from "../../lib/dashboard-core.ts";
import { store } from "../../lib/store.ts";
import { deviceIdTitle } from "../../lib/text.ts";
import type { AvailableDevice, Device } from "../../lib/types.ts";
import { button, clearBusy, focusDropped, focusNeighbour, h, holdsFocus, orderChildren, part, setBusy, showUnconfirmed } from "../../lib/ui.ts";
import { apiErrorToast, type ConfigQueue } from "./config-queue.ts";

// AvailableCard is one detected but unconfigured capture device: its name,
// address, the id provisioning persists and its capabilities, and an Enable
// button. It shows what key names (availableCardKey) and is rebuilt when that
// changes, busy state included.
class AvailableCard {
  public readonly el: HTMLElement;
  public readonly enableBtn: HTMLButtonElement;
  public readonly key: string;
  public readonly label: string;

  constructor(d: AvailableDevice, key: string, enabling: boolean) {
    this.key = key;
    this.label = availableLabel(d);
    const caps = capsSummary(d);
    this.enableBtn = button({ variant: "primary", extraClass: "available-enable", label: "Enable" });
    this.setEnabling(enabling);
    this.el = h("div", { class: "config-device-card available-card" },
      h("div", { class: "available-info" },
        // Fall back to the short ALSA address, not the long stable id, when the
        // card has no friendly name: the address is what the rest of the card
        // shows.
        h("div", { class: "device-title" }, d.friendlyName || d.hwAddr || d.device),
        h("div", { class: "available-sub" },
          h("span", { class: "mono" }, d.hwAddr ?? d.device),
          d.idStable === false && h("span", { class: "available-caps" }, "no stable ID"),
          caps && h("span", { class: "available-caps" }, caps),
        ),
        // The id provisioning persists, shown as selectable text rather than a
        // tooltip (unreachable by keyboard, touch and screen readers), so an
        // operator can tell which of two identical units (serial or port) this
        // card binds.
        d.hwAddr && d.device !== d.hwAddr && h("div", { class: "available-id mono" }, deviceIdTitle(d.device)),
      ),
      this.enableBtn,
    );
  }

  // setEnabling shows the Enable's progress. The accessible name names the
  // device, since there is one Enable button per available device and a bare
  // "Enable" is ambiguous to a screen-reader user (two identical units share
  // a friendly name, so the label adds the address), and it says the same as
  // the visible text.
  public setEnabling(busy: boolean): void {
    if (busy) setBusy(this.enableBtn, "Enabling...");
    else clearBusy(this.enableBtn, "Enable");
    this.enableBtn.setAttribute("aria-label", `${busy ? "Enabling" : "Enable"} ${this.label}`);
  }
}

// AvailableDevicesHost is what the section needs from the Dashboard.
export interface AvailableDevicesHost {
  queue: ConfigQueue;
  announceEl: HTMLElement | null;
  // focusDevice moves focus to a device card's settings, after an Enable
  // whose card held focus added that device, and says so.
  focusDevice(id: string): void;
}

// AvailableDevices is the Available Devices section (#available-section): the
// host's detected but unconfigured capture devices, each with an Enable button
// that provisions it. The whole section hides when nothing is available, so a
// fully configured host shows no empty panel. Cards are keyed by device id and
// rebuilt only when what they show changed (availablePlan), so a render keeps
// the operator's text selection (the device id is there to be copied) and
// keyboard focus on the cards it leaves alone. A rebuilt card that held focus
// hands it to its new Enable button.
export class AvailableDevices {
  private readonly section: HTMLElement | null;
  private readonly rack: HTMLElement | null;
  private readonly host: AvailableDevicesHost;
  // Device ids with a provisioning request in flight, so the Enable button
  // shows progress and a second click cannot double-provision.
  private readonly provisioning = new Set<string>();
  // The cards on screen by device id, kept in screen order.
  private cards = new Map<string, AvailableCard>();
  // The card that held keyboard focus when a render removed it during that
  // device's Enable: its device, its neighbours on screen and its label, so
  // the Enable can move focus when it settles (to the new device card on
  // success; on failure to its own card when it is listed again, else its
  // nearest neighbour still listed). Only set while that Enable is in flight.
  private focusLost: { id: string; neighbours: string[]; label: string } | null = null;

  constructor(section: HTMLElement | null, host: AvailableDevicesHost) {
    this.section = section;
    this.rack = part(section, "available-rack");
    this.host = host;
  }

  // render shows an available-devices read.
  public render(available: AvailableDevice[]): void {
    if (!this.rack || !this.section) return;
    const rack = this.rack;
    this.section.hidden = available.length === 0;
    const next = available.map((d) => ({ d, id: d.device, key: availableCardKey(d, this.provisioning.has(d.device)) }));
    const shown = new Map([...this.cards].map(([id, c]) => [id, c.key]));
    const plan = availablePlan(shown, next);

    // A removed card that held focus: during its own Enable the Enable moves
    // focus once it settles; otherwise (the device went away) focus goes to
    // its nearest neighbour still listed, below. cards is kept in screen order
    // (see the end of this render), so its keys are the order on screen.
    let stranded: { neighbours: string[]; label: string } | null = null;
    const oldOrder = [...this.cards.keys()];
    for (const id of plan.remove) {
      const c = this.cards.get(id);
      if (!c) continue;
      if (holdsFocus(c.el)) {
        const lost = { id, neighbours: neighbourOrder(oldOrder, oldOrder.indexOf(id)), label: c.label };
        if (this.provisioning.has(id)) this.focusLost = lost;
        else stranded = lost;
      }
      c.el.remove();
      this.cards.delete(id);
    }
    const build = new Set(plan.build);
    for (const { d, id, key } of next) {
      if (!build.has(id)) continue;
      const old = this.cards.get(id);
      const card = new AvailableCard(d, key, this.provisioning.has(id));
      card.enableBtn.addEventListener("click", () => void this.provision(d, card));
      const refocus = old !== undefined && holdsFocus(old.el);
      if (old) old.el.replaceWith(card.el);
      this.cards.set(id, card);
      if (refocus) card.enableBtn.focus({ preventScroll: true });
    }
    // Order with a diff, as the device rack does: steady state moves no node.
    // The map is rebuilt in the same order, so its order is the screen's.
    this.cards = new Map(plan.order.flatMap((id) => {
      const c = this.cards.get(id);
      return c ? [[id, c] as const] : [];
    }));
    orderChildren(rack, [...this.cards.values()].map((c) => c.el));

    if (stranded) this.focusNeighbour(stranded.neighbours, stranded.label);
  }

  // takeFocusLost returns and clears the record of a focused card that a
  // render removed during this device's Enable, if there is one.
  private takeFocusLost(device: string): { id: string; neighbours: string[]; label: string } | null {
    const lost = this.focusLost;
    if (lost?.id !== device) return null;
    this.focusLost = null;
    return lost;
  }

  // focusNeighbour moves focus, after a card holding it went away, to the
  // Enable button of the first of its neighbours still listed, else to the
  // workspace, and announces which device went.
  private focusNeighbour(neighbours: readonly string[], label: string): void {
    focusNeighbour(
      this.host.announceEl,
      neighbours,
      (id) => {
        const c = this.cards.get(id);
        return c && { control: c.enableBtn, name: c.label };
      },
      (next) => availableGoneMessage(label, next),
    );
  }

  private async provision(d: AvailableDevice, card: AvailableCard): Promise<void> {
    if (this.provisioning.has(d.device)) return;
    this.provisioning.add(d.device);
    card.setEnabling(true);
    const queue = this.host.queue;
    const label = availableLabel(d);
    try {
      // Serialize through the same queue as toggles and settings saves: those
      // submit a full-array PATCH built from the cached config, so a provision
      // running concurrently could be clobbered by a stale PATCH (or vice
      // versa). The refreshes run inside the task so the next queued mutation
      // rebuilds from the post-provision config.
      await queue.enqueue(async () => {
        let created: Device | undefined;
        let read: { devices: boolean; all: boolean } | undefined;
        try {
          created = await api.provisionDevice({ device: d.device });
        } catch (err: unknown) {
          if (isRefusal(err)) throw err;
          // No readable answer: re-read inside the queue, so the next queued
          // change builds from what the appliance now holds, and judge by it.
          read = await queue.refreshDeviceViews();
          created = store.getState().devices.find((dv) => dv.device === d.device);
          if (!created) {
            if (read.devices) showToast(`${label} does not appear to have been enabled; check again shortly.`, "warn");
            else showUnconfirmed(`that ${label} was enabled`, "the device list could not be read; check it before trying again");
            return;
          }
        }
        // A re-read that found the device already refreshed everything.
        read ??= await queue.refreshDeviceViews();
        if (!read.all) {
          showToast(`Enabled ${created.name}. The dashboard could not be refreshed; it updates on the next poll.`, "warn");
        } else {
          const ch = created.channels.length === 1 ? ` on channel ${created.channels[0]}` : "";
          showToast(`Enabled ${created.name}${ch}. Streaming on ${created.path}.`);
        }
        // The refresh removed this device's card. If it held focus and focus
        // has not moved since (it fell to the document body), hand it to the
        // new device card, else to the workspace region, and say where it went.
        const lost = this.takeFocusLost(d.device);
        if (lost && focusDropped()) this.host.focusDevice(created.device);
      });
    } catch (err: unknown) {
      // Only a refusal reaches here: an unknown outcome is settled in the task.
      apiErrorToast(err, `Could not enable ${label}`);
      // A 404 (the device left or was re-detected) or 409 (already set up)
      // means this card is stale; refresh now rather than at the next poll.
      if (isRefusal(err) && (err.status === 404 || err.status === 409)) void store.refreshAvailable();
    } finally {
      this.provisioning.delete(d.device);
      // Set only if the Enable failed after a render took its focused card:
      // focus goes to its own card when it is listed again, else to its
      // nearest neighbour still listed, as for any card that went.
      const lost = this.takeFocusLost(d.device);
      card.setEnabling(false);
      // setEnabling restores the card built before the click. If a render
      // during the Enable rebuilt the card busy instead (its key includes the
      // in-flight state), this render rebuilds it idle, since the key changed
      // back. A device no longer listed (the usual success) has no card left.
      this.render(store.getState().available);
      if (lost && focusDropped()) {
        // Listed again by now (a poll brought it back): its own card takes
        // focus; otherwise its nearest neighbour still listed.
        const own = this.cards.get(d.device)?.enableBtn;
        if (own) own.focus({ preventScroll: true });
        else this.focusNeighbour(lost.neighbours, lost.label);
      }
    }
  }
}
