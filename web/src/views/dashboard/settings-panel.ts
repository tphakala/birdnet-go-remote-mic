import { DeviceSettingsForm } from "../../components/device-settings.ts";
import { showToast } from "../../components/toast.ts";
import { api, firstProblem, isRefusal } from "../../lib/api.ts";
import { deviceConfigKey, deviceToConfig, judgeUnconfirmedSave, REMOVED_FOCUS_MESSAGE, rejectedFieldKey } from "../../lib/dashboard-core.ts";
import { withFirstStream } from "../../lib/device-settings-core.ts";
import { confirmDialog } from "../../lib/modal.ts";
import { store } from "../../lib/store.ts";
import type { Device, DeviceConfig } from "../../lib/types.ts";
import { announce, button, clearBusy, focusDropped, focusOnOrDropped, focusWorkspace, h, setBusy, setHidden, setText, showUnconfirmed } from "../../lib/ui.ts";
import { apiErrorToast, type ConfigQueue, STALE_BASE_TEXT } from "./config-queue.ts";

// FIX_FIELDS_TEXT is the toast for a save stopped by a marked field, found by
// the form's own check or by the appliance: the field carries the reason.
const FIX_FIELDS_TEXT = "Save failed: fix the highlighted fields.";

// Sequence for the settings panels' element ids (aria-controls targets).
let settingsSeq = 0;

// SettingsPanelHost is what a panel needs from the device card it belongs to.
export interface SettingsPanelHost {
  queue: ConfigQueue;
  announceEl: HTMLElement | null;
  // device is the card's latest runtime record; device.device is the id.
  device(): Device;
  // hideInactive is the card's "hide inactive channels" preference, which the
  // form's switch shows; setHideInactive saves a change made there.
  hideInactive(): boolean;
  setHideInactive(hide: boolean): void;
  // expandedChanged shows the panel opening or closing on the card: its
  // .expanded class and the settings button's aria-expanded.
  expandedChanged(): void;
  // settingsButton is the card's current settings disclosure, where focus
  // returns when the panel's own controls go.
  settingsButton(): HTMLElement;
}

// SettingsPanel is a device card's settings panel: the DeviceSettingsForm,
// built when the panel opens from the saved config, plus its actions bar
// (Remove, the unsaved and "changed elsewhere" notices, Cancel, Save). The
// card moves el into each article it builds, so an open form survives a card
// rebuild rather than being torn down under the operator.
export class SettingsPanel {
  public readonly el: HTMLElement;
  private readonly host: SettingsPanelHost;
  private form: DeviceSettingsForm | null = null;
  // Cached deviceConfigKey of the config entry the open form was built from,
  // computed once when the form opens rather than re-serialised on every
  // render (the source does not change until the form is rebuilt). sync
  // compares it with the current config to detect an out-of-band change.
  private formSourceKey = "";
  // "Changed elsewhere" notice in the open form's actions bar.
  private staleNote: HTMLElement | null = null;
  private staleMsg: HTMLElement | null = null;
  private open = false;
  private dirty = false;
  // Set while this panel's Remove is deleting the device, so the render that
  // removes the card announces only where focus went: the Remove's own toast
  // says what went.
  private removingNow = false;

  constructor(host: SettingsPanelHost) {
    this.host = host;
    this.el = h("div", { class: "card-settings", id: `card-settings-${++settingsSeq}`, hidden: true });
  }

  public get expanded(): boolean {
    return this.open;
  }

  public get removing(): boolean {
    return this.removingNow;
  }

  // toggle opens the panel, building the form from the saved config, or
  // closes it (confirming a discard of unsaved edits first).
  public toggle(): void {
    if (this.open) {
      void this.requestClose();
      return;
    }
    if (!this.form) this.build();
    this.open = true;
    this.el.hidden = false;
    this.host.expandedChanged();
  }

  // sync shows or hides the "changed elsewhere" notice on an open form by
  // comparing the config the form was built from against current, the
  // device's entry in the current config (excluding enabled). It never
  // mutates the form; the operator chooses Reload or Save.
  public sync(current: DeviceConfig | undefined): void {
    if (!this.form || !this.staleNote || !this.staleMsg) return;
    const stale = deviceConfigKey(current) !== this.formSourceKey;
    // Write the message text (not just toggle visibility) so the role=status
    // region announces the drift as it appears and clears when resolved.
    setText(this.staleMsg, stale ? "Settings changed elsewhere. Save overwrites them. " : "");
    setHidden(this.staleNote, !stale);
  }

  // setHideInactive shows a preference changed in another tab in an open
  // form's switch.
  public setHideInactive(hide: boolean): void {
    this.form?.setHideInactive(hide);
  }

  public destroy(): void {
    this.form?.destroy();
  }

  private build(): void {
    this.el.textContent = "";
    const d = this.host.device();
    const removeBtn = button({ variant: "danger", label: "Remove", ariaLabel: `Remove ${d.name}` });
    const badge = h("span", { class: "staged-badge", hidden: true }, "Unsaved changes");
    const reloadBtn = h("button", { class: "btn-link", type: "button" }, "Reload");
    // The message text is written by sync when drift is detected: a
    // role=status region announces on a content change, so writing the text is
    // reliable where merely un-hiding pre-filled text is not. A Reload rebuilds
    // the form from the fresh config; Save keeps last-writer-wins.
    const staleMsg = h("span", { class: "stale-msg" });
    const staleNote = h("span", { class: "stale-note", role: "status", hidden: true }, staleMsg, reloadBtn);
    const cancelBtn = button({ variant: "secondary", label: "Cancel" });
    const saveBtn = button({ variant: "primary", label: "Save Changes" });
    const actions = h("div", { class: "settings-actions" },
      removeBtn, badge, staleNote, h("span", { class: "settings-actions-spacer" }), cancelBtn, saveBtn,
    );
    reloadBtn.addEventListener("click", () => void this.reload());
    removeBtn.addEventListener("click", () => void this.remove(removeBtn));

    // Build from the saved config (source of truth), matched by device id.
    // Prefer the persisted config (it carries the enabled flag); fall back to
    // the runtime device projected through deviceToConfig so enabled is always
    // present and collect() cannot drop it.
    const configured = store.getState().config?.devices.find((cd) => cd.device === d.device) ?? deviceToConfig(d);
    const form = new DeviceSettingsForm(configured, () => { badge.hidden = false; this.dirty = true; }, {
      friendlyName: d.friendlyName,
      supportedRates: d.supportedRates,
      supportedChannels: d.supportedChannels,
      idStable: d.idStable,
    }, {
      hideInactive: this.host.hideInactive(),
      onHideInactiveChange: (hide) => this.host.setHideInactive(hide),
    });
    this.form = form;
    this.formSourceKey = deviceConfigKey(configured);
    this.staleNote = staleNote;
    this.staleMsg = staleMsg;
    this.el.append(form.element, actions);

    // Tell the operator when opening the form silently downgraded an
    // unsupported saved codec, rather than the change appearing unexplained.
    const notice = form.loadNotice();
    if (notice) {
      badge.hidden = false;
      this.dirty = true;
      showToast(notice, "warn");
    }

    cancelBtn.addEventListener("click", () => void this.requestClose());
    saveBtn.addEventListener("click", () => void this.save(saveBtn, cancelBtn));
  }

  // requestClose collapses the panel, but first confirms the discard if the
  // form has unsaved edits. It guards every collapse path (the Cancel button
  // and the settings button), so a stray click cannot silently drop pending
  // changes.
  private async requestClose(): Promise<void> {
    if (this.dirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "This device has unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    this.close();
    // close clears the panel (including the Cancel button focus was on), so
    // return focus to the settings button, which always survives the
    // collapse, rather than letting focus fall to <body>.
    this.host.settingsButton().focus();
  }

  private close(): void {
    this.open = false;
    this.dirty = false;
    this.el.hidden = true;
    this.host.expandedChanged();
    this.el.textContent = "";
    this.form?.destroy();
    this.form = null;
    this.formSourceKey = "";
    this.staleNote = null;
    this.staleMsg = null;
  }

  // reload rebuilds the open form from the current config after an
  // out-of-band change, confirming a discard first if the operator has
  // unsaved edits. It never rewrites the form under the operator without
  // asking.
  private async reload(): Promise<void> {
    if (this.dirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "Reloading replaces your unsaved changes with the current saved settings.",
        confirmLabel: "Discard and reload",
        danger: true,
      });
      if (!ok) return;
    }
    this.close();
    this.toggle();
    // The Reload button was just removed with the old form; move focus to the
    // settings button (which survives the rebuild) rather than letting it fall
    // to <body>.
    this.host.settingsButton().focus();
  }

  // finishSave ends a device save that applied: it closes the form that was
  // saved (not one the operator opened while the save was queued, which
  // would drop its edits), says so, and returns focus to the settings button
  // if closing the form dropped it.
  private finishSave(form: DeviceSettingsForm, text: string): void {
    const same = this.form === form;
    if (same) this.close();
    showToast(text);
    if (same && focusDropped()) this.host.settingsButton().focus();
  }

  private async save(btn: HTMLElement, cancelBtn: HTMLButtonElement): Promise<void> {
    if (btn.getAttribute("aria-disabled") === "true") return;
    const form = this.form;
    if (!form) return;
    if (!form.validate()) {
      showToast(FIX_FIELDS_TEXT, "error");
      // Move focus to the first flagged field, matching AccessCard.save, so a
      // keyboard user is taken to what needs fixing instead of staying on the
      // Save button.
      form.focusFirstInvalid();
      return;
    }
    // Refuse to save from the runtime fallback: until GET /config has loaded,
    // the queue's base() projects via deviceToConfig, which omits config-only
    // fields (quietAlert), so this full-array PATCH would reset every device's
    // opt-out. config only ever goes null -> loaded.
    if (!store.getState().config) {
      showToast("Configuration has not loaded yet. Try again in a moment.", "warn");
      return;
    }
    const edited = form.collect();
    const queue = this.host.queue;
    // Show the save in flight and block a second submit or a discard while the
    // queued PATCH runs; setBusy keeps Save focusable (aria-disabled) while
    // Cancel, which is not focused, can simply be disabled.
    setBusy(btn, "Saving...");
    cancelBtn.disabled = true;
    try {
      await queue.enqueue(async () => {
        if (!(await queue.freshBase())) {
          showToast(STALE_BASE_TEXT, "warn");
          return;
        }
        // Source the enabled flag and the patch base FRESH inside the queued
        // task: the settings form does not edit enabled, the card toggle may
        // have changed it since the panel opened, and a prior queued mutation
        // may have changed the base. Building here (not at collect time) avoids
        // clobbering either.
        const cur = store.getState().config?.devices.find((cd) => cd.device === edited.device);
        if (cur?.enabled !== undefined) edited.enabled = cur.enabled;
        // The form edits the first stream; a multi-stream device keeps its
        // other streams as the config holds them now, so a change made
        // elsewhere since the form opened is not reverted.
        const toSave = withFirstStream(edited, cur?.streams);
        const merged = queue.base().map((cd) => (cd.device === edited.device ? toSave : cd));
        if (!merged.some((cd) => cd.device === edited.device)) merged.push(toSave);

        try {
          const res = await api.patchConfig({ devices: merged });
          // Seed the cached config with the authoritative PATCH response before
          // the refresh (see ConfigQueue.applied). A refresh failure after a
          // successful PATCH must not report "Save failed": the change persisted.
          queue.applied(res.config);
          this.finishSave(form, res.restartRequired ? "Device settings saved. Restart the appliance to apply." : "Device settings applied.");
          await Promise.all([store.refreshConfig(), store.refreshDevices()]);
        } catch (err: unknown) {
          // A problem the form can show is marked on its field (see
          // rejectedFieldKey and markRejected), and the toast then only says
          // the save failed, as for a local check: the field carries the
          // reason. Focus moves there only if it is still on Save or dropped,
          // and only on the form that was saved.
          const problem = firstProblem(err);
          const key = problem?.field ? rejectedFieldKey(problem.field, merged, edited.device) : null;
          const moveFocus = focusOnOrDropped(btn);
          const marked = problem !== null && key !== null && this.form === form && form.markRejected(key, problem.reason, moveFocus);
          // The short toast only when focus went to the marked field, which
          // then reads the reason; otherwise the toast says it all.
          if (marked && moveFocus) showToast(FIX_FIELDS_TEXT, "error");
          else if (isRefusal(err)) apiErrorToast(err, "Save failed", merged);
          else {
            // The save may have persisted: re-read inside the queue, and judge
            // by what the appliance now holds (judgeUnconfirmedSave).
            const read = await queue.refreshConfigViews();
            const after = store.getState().config?.devices.find((cd) => cd.device === edited.device);
            const what = `the save of ${edited.name}`;
            switch (judgeUnconfirmedSave(read, deviceConfigKey(cur), deviceConfigKey(after), deviceConfigKey(toSave))) {
              case "unread":
                showUnconfirmed(what, "check the settings before saving again");
                break;
              case "applied":
                this.finishSave(form, "Device settings applied.");
                break;
              case "unchanged":
                this.finishSave(form, `${edited.name}'s settings were already as saved.`);
                break;
              case "notApplied":
                showToast(`The save of ${edited.name} does not appear to have applied; check again shortly.`, "warn");
                break;
              case "changed":
                showUnconfirmed(what, "the settings changed; check them before saving again");
                break;
            }
          }
        }
      });
    } finally {
      // Restore the buttons whether the save succeeded (its panel is torn down,
      // so this is a harmless no-op on detached nodes) or failed (they stay for
      // retry).
      clearBusy(btn, "Save Changes");
      cancelBtn.disabled = false;
    }
  }

  // remove deletes the configured device after confirmation, returning its
  // hardware to the available list. On success the card disappears via the
  // device refresh; on failure the button is restored so it can be retried. A
  // refresh that fails after the delete succeeded is reported as such, not as
  // a failed removal: the next poll takes the card away.
  private async remove(btn: HTMLElement): Promise<void> {
    // aria-disabled keeps the button focusable while busy, so guard against a
    // keyboard re-activation that pointer-events cannot block: without this a
    // second Enter opens a second confirm and issues a second DELETE.
    if (btn.getAttribute("aria-disabled") === "true") return;
    const ok = await confirmDialog({
      title: "Remove device",
      body: `Remove ${this.host.device().name}? It stops streaming and returns to the available list, and its stream path is discarded.`,
      confirmLabel: "Remove",
      danger: true,
    });
    if (!ok) return;
    // The accessible name follows the visible text, as on Enable.
    const removeLabel = btn.getAttribute("aria-label");
    setBusy(btn, "Removing...");
    btn.setAttribute("aria-label", `Removing ${this.host.device().name}`);
    const queue = this.host.queue;
    let removed = false;
    try {
      // Serialize with toggles and settings saves: a stale full-array PATCH
      // from one of those must not run interleaved with this delete and
      // restore the removed device (or drop a concurrently provisioned one).
      await queue.enqueue(async () => {
        const { device: id, name } = this.host.device();
        // notFound is a 404: no device by that name, so gone already (an
        // earlier attempt whose answer was lost, another tab) or renamed,
        // which the re-read tells apart by the device's id. unknown is no
        // readable answer at all.
        let notFound: unknown = null;
        let unknown = false;
        // A render that removes the card (the re-read below, or a poll that
        // lands while the DELETE is in flight) hands focus to its nearest
        // neighbour, saying only where focus went: the toast below says what
        // went.
        this.removingNow = true;
        let read: { devices: boolean; all: boolean };
        try {
          try {
            await api.deleteDevice(name);
          } catch (err: unknown) {
            if (!isRefusal(err)) unknown = true;
            else if (err.status === 404) notFound = err;
            else throw err;
          }
          read = await queue.refreshDeviceViews();
        } finally {
          this.removingNow = false;
        }
        const listed = store.getState().devices.some((dv) => dv.device === id);
        if (read.devices && listed) {
          if (notFound !== null) throw notFound;
          showToast(`${name} does not appear to have been removed; check again shortly.`, "warn");
          return;
        }
        if (!read.devices && (unknown || notFound !== null)) {
          showUnconfirmed(`that ${name} was removed`, "the device list could not be read; check it before trying again");
          return;
        }
        removed = true;
        if (read.all) showToast(`Removed ${name}.`);
        else showToast(`Removed ${name}. The dashboard could not be refreshed; it updates on the next poll.`, "warn");
        // Focus that had already fallen to the page (the confirm returned it
        // to a Remove button a poll had just removed) goes to the dashboard.
        if (focusDropped()) {
          focusWorkspace();
          announce(this.host.announceEl, REMOVED_FOCUS_MESSAGE);
        }
      });
    } catch (err: unknown) {
      // Only a refusal reaches here: an unknown outcome is settled in the task.
      apiErrorToast(err, `Could not remove ${this.host.device().name}`);
    } finally {
      if (!removed) {
        clearBusy(btn, "Remove");
        if (removeLabel !== null) btn.setAttribute("aria-label", removeLabel);
      }
    }
  }
}
