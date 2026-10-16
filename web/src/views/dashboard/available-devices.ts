import { showToast } from "../../components/toast.ts";
import { api, isRefusal } from "../../lib/api.ts";
import { availableCardKey, availableCardShape, availableChange, availableGoneMessage, availableLabel, capsSummary, neighbourOrder } from "../../lib/dashboard-core.ts";
import { KeyedList } from "../../lib/keyed-list.ts";
import { store } from "../../lib/store.ts";
import { deviceIdTitle } from "../../lib/text.ts";
import type { AvailableDevice, Device } from "../../lib/types.ts";
import { button, clearBusy, focusDropped, focusNeighbour, h, holdsFocus, part, setAttr, setBusy, setHidden, setText, showUnconfirmed } from "../../lib/ui.ts";
import { apiErrorToast, type ConfigQueue } from "./config-queue.ts";

// AvailableCard is one detected but unconfigured capture device: its name,
// address, the id provisioning persists and its capabilities, and an Enable
// button. It shows the fields availableCardKey covers, busy state included.
// A change to their text is patched in (update); the card is rebuilt only when
// its optional rows appear or go (availableCardShape).
class AvailableCard {
  public readonly el: HTMLElement;
  public readonly enableBtn: HTMLButtonElement;
  public readonly shape: string;
  public key: string;
  public label: string;
  private readonly titleEl: HTMLElement;
  private readonly addrEl: HTMLElement;
  private readonly capsEl: HTMLElement | null;
  private readonly idEl: HTMLElement | null;

  constructor(d: AvailableDevice, key: string, enabling: boolean) {
    this.shape = availableCardShape(d);
    this.key = key;
    this.label = availableLabel(d);
    const caps = capsSummary(d);
    this.enableBtn = button({ variant: "primary", extraClass: "available-enable", label: "Enable" });
    this.setEnabling(enabling);
    // Fall back to the short ALSA address, not the long stable id, when the
    // card has no friendly name: the address is what the rest of the card
    // shows.
    this.titleEl = h("div", { class: "device-title" }, d.friendlyName || d.hwAddr || d.device);
    this.addrEl = h("span", { class: "mono" }, d.hwAddr ?? d.device);
    this.capsEl = caps ? h("span", { class: "available-caps" }, caps) : null;
    // The id provisioning persists, shown as selectable text rather than a
    // tooltip (unreachable by keyboard, touch and screen readers), so an
    // operator can tell which of two identical units (serial or port) this
    // card binds.
    this.idEl = d.hwAddr && d.device !== d.hwAddr ? h("div", { class: "available-id mono" }, deviceIdTitle(d.device)) : null;
    this.el = h("div", { class: "config-device-card available-card" },
      h("div", { class: "available-info" },
        this.titleEl,
        h("div", { class: "available-sub" },
          this.addrEl,
          d.idStable === false && h("span", { class: "available-caps" }, "no stable ID"),
          this.capsEl,
        ),
        this.idEl,
      ),
      this.enableBtn,
    );
  }

  // update patches the card to a device of the same shape, writing only what
  // changed, so a text selection in the device id survives a name change.
  public update(d: AvailableDevice, key: string, enabling: boolean): void {
    this.key = key;
    this.label = availableLabel(d);
    setText(this.titleEl, d.friendlyName || d.hwAddr || d.device);
    setText(this.addrEl, d.hwAddr ?? d.device);
    if (this.capsEl) setText(this.capsEl, capsSummary(d));
    if (this.idEl) setText(this.idEl, deviceIdTitle(d.device));
    this.setEnabling(enabling);
  }

  // setEnabling shows the Enable's progress. The accessible name names the
  // device, since there is one Enable button per available device and a bare
  // "Enable" is ambiguous to a screen-reader user (two identical units share
  // a friendly name, so the label adds the address), and it says the same as
  // the visible text.
  public setEnabling(busy: boolean): void {
    if (busy) setBusy(this.enableBtn, "Enabling...");
    else clearBusy(this.enableBtn, "Enable");
    const name = `${busy ? "Enabling" : "Enable"} ${this.label}`;
    setAttr(this.enableBtn, "aria-label", name);
  }
}

// AvailableItem is what a card is drawn from: a detected device with its id,
// the shape of the card it needs and the key of the text it shows.
interface AvailableItem {
  d: AvailableDevice;
  id: string;
  shape: string;
  key: string;
}

// FocusLost is an Available card that held focus when a render removed it:
// its device, its neighbours on screen, and its label.
interface FocusLost {
  id: string;
  neighbours: string[];
  label: string;
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
// fully configured host shows no empty panel, and while the device rack above
// it is still loading (see syncShown). Cards are keyed by device id. A change
// to what one shows is patched in place, and only a card whose optional rows
// appear or go is rebuilt (availableChange), so a render keeps the operator's
// text selection (the device id is there to be copied) and keyboard focus on
// the cards it leaves alone. A rebuilt card that held focus hands it to its new
// Enable button.
export class AvailableDevices {
  private readonly section: HTMLElement | null;
  private readonly host: AvailableDevicesHost;
  // Device ids with a provisioning request in flight, so the Enable button
  // shows progress and a second click cannot double-provision.
  private readonly provisioning = new Set<string>();
  // The cards on screen by device id, in screen order; null without a rack.
  private readonly cards: KeyedList<AvailableItem, AvailableCard> | null;
  // The card that held keyboard focus when the render in progress removed it
  // outside its own Enable; render hands focus to a neighbour afterwards.
  private stranded: FocusLost | null = null;
  // The card that held keyboard focus when a render removed it during that
  // device's Enable, so the Enable can move focus when it settles (to the new
  // device card on success; on failure to its own card when it is listed
  // again, else its nearest neighbour still listed). Only set while that
  // Enable is in flight.
  private focusLost: FocusLost | null = null;

  constructor(section: HTMLElement | null, host: AvailableDevicesHost) {
    this.section = section;
    const rack = part(section, "available-rack");
    this.host = host;
    this.cards = rack
      ? new KeyedList<AvailableItem, AvailableCard>(rack, {
          key: (n) => n.id,
          create: (n) => {
            const d = n.d;
            const card = new AvailableCard(d, n.key, this.provisioning.has(n.id));
            card.enableBtn.addEventListener("click", () => void this.provision(d, card));
            return card;
          },
          nodes: (c) => [c.el],
          stale: (c, n) => availableChange(c, n) === "build",
          // A rebuilt card that held focus hands it to its new Enable button.
          replace: (old, next) => {
            const refocus = holdsFocus(old.el);
            old.el.replaceWith(next.el);
            if (refocus) next.enableBtn.focus({ preventScroll: true });
          },
          update: (c, n) => {
            if (availableChange(c, n) === "patch") c.update(n.d, n.key, this.provisioning.has(n.id));
          },
          // A removed card that held focus: during its own Enable the Enable
          // moves focus once it settles; otherwise (the device went away) focus
          // goes to its nearest neighbour still listed, after the render.
          // before is the keys in screen order.
          remove: (c, id, before) => {
            if (!holdsFocus(c.el)) return;
            const lost = { id, neighbours: neighbourOrder(before, before.indexOf(id)), label: c.label };
            if (this.provisioning.has(id)) this.focusLost = lost;
            else this.stranded = lost;
          },
        })
      : null;
  }

  // syncShown shows the section when it lists a device and the device rack
  // above it is no longer loading: its first devices read applied or failed.
  // Shown earlier, the section would sit under the rack's one-line loading
  // placeholder and be pushed down, out of a laptop-sized window, when the
  // device cards arrive. The rack's length is unknown until that read, so the
  // section waits instead of reserving space.
  public syncShown(): void {
    if (this.section) setHidden(this.section, (this.cards?.size ?? 0) === 0 || store.devicesRead() === "pending");
  }

  // render shows an available-devices read.
  public render(available: AvailableDevice[]): void {
    if (!this.cards || !this.section) return;
    this.cards.sync(available.map((d) => ({ d, id: d.device, shape: availableCardShape(d), key: availableCardKey(d, this.provisioning.has(d.device)) })));
    this.syncShown();

    const stranded = this.takeStranded();
    if (stranded) this.focusCardNeighbour(stranded.neighbours, stranded.label);
  }

  // takeStranded returns and clears the record of a focused card the sync in
  // progress removed outside its own Enable, if there is one.
  private takeStranded(): FocusLost | null {
    const lost = this.stranded;
    this.stranded = null;
    return lost;
  }

  // takeFocusLost returns and clears the record of a focused card that a
  // render removed during this device's Enable, if there is one.
  private takeFocusLost(device: string): FocusLost | null {
    const lost = this.focusLost;
    if (lost?.id !== device) return null;
    this.focusLost = null;
    return lost;
  }

  // focusCardNeighbour moves focus, after a card holding it went away, to the
  // Enable button of the first of its neighbours still listed, else to the
  // workspace, and announces which device went.
  private focusCardNeighbour(neighbours: readonly string[], label: string): void {
    focusNeighbour(
      this.host.announceEl,
      neighbours,
      (id) => {
        const c = this.cards?.get(id);
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
    const label = card.label;
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
        // The refresh removed this device's Available card. If it held focus
        // and focus has not moved since (it fell to the document body), hand
        // it to the new device card, else to the workspace region, and say
        // where it went.
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
      // during the Enable rebuilt the card busy instead, or patched it (its key
      // includes the in-flight state), this render shows it idle, since the key
      // changed back. A device no longer listed (the usual success) has no
      // card left.
      this.render(store.getState().available);
      if (lost && focusDropped()) {
        // Listed again by now (a poll brought it back): its own card takes
        // focus; otherwise its nearest neighbour still listed.
        const own = this.cards?.get(d.device)?.enableBtn;
        if (own) own.focus({ preventScroll: true });
        else this.focusCardNeighbour(lost.neighbours, lost.label);
      }
    }
  }
}
