import { api, apiErrorMessage, firstProblem, isRefusal } from "../../lib/api.ts";
import { store } from "../../lib/store.ts";
import { clearBusy, h, part, setBusy, setLoading, showUnconfirmed } from "../../lib/ui.ts";
import { confirmDialog } from "../../lib/modal.ts";
import { overrideLines, overridesSignature } from "../../lib/system-core.ts";
import { showToast } from "../../components/toast.ts";
import type { Config, ConfigOverride } from "../../lib/types.ts";

// NetworkCard is the Network & Discovery card (#sys-network-card): the listen
// addresses (read-only), the mDNS switch with its save flow, and the note on
// serve overrides.
export class NetworkCard {
  private readonly cardEl: HTMLElement | null;
  private readonly actionsEl: HTMLElement | null;
  private readonly discoveryEl: HTMLInputElement | null;
  private readonly overridesEl: HTMLElement | null;
  private dirty = false;
  // Signature of the override set last rendered into the #sys-overrides live
  // region, so a 3s status poll that changes nothing does not re-announce it.
  private lastOverridesSig: string | null = null;

  constructor(root: HTMLElement | null) {
    this.cardEl = root;
    this.actionsEl = part(root, "sys-network-actions");
    this.discoveryEl = part<HTMLInputElement>(root, "sys-discovery-enabled");
    this.overridesEl = part(root, "sys-overrides");
    // Laid out from the first paint, and usable once the first config read
    // fills it.
    if (root) setLoading(root, true);
    this.bind();
  }

  // config fills the form from a config read, unless it holds unsaved edits.
  public config(cfg: Config): void {
    if (!this.dirty) this.populate(cfg);
  }

  // overrides shows or hides the serve-overrides note. Each entry explains why a
  // config-view (persisted) value differs from what the appliance is actually
  // running (effective), because a serve CLI flag overrode it for this run.
  // Empty or absent means no override is active.
  public overrides(list: readonly ConfigOverride[] | undefined): void {
    if (!this.overridesEl) return;
    const overrides = list ?? [];
    // Rebuild only when the set actually changes. This runs on every 3s status
    // poll and #sys-overrides is a role=status live region, so an unconditional
    // rebuild would re-announce the unchanged note to a screen reader each tick.
    // Mirrors AccessCard's setText guard on #sys-auth-state.
    const sig = overridesSignature(overrides);
    if (sig === this.lastOverridesSig) return;
    this.lastOverridesSig = sig;
    this.overridesEl.textContent = "";
    if (overrides.length === 0) {
      this.overridesEl.hidden = true;
      return;
    }
    this.overridesEl.hidden = false;
    this.overridesEl.appendChild(h("span", { class: "staged-badge" }, "Serve overrides active"));
    for (const line of overrideLines(overrides)) {
      this.overridesEl.appendChild(h("span", { class: "override-line" }, line));
    }
  }

  private bind(): void {
    if (this.discoveryEl) {
      this.discoveryEl.addEventListener("change", () => {
        this.dirty = true;
        if (this.actionsEl) this.actionsEl.hidden = false;
      });
    }
    part(this.cardEl, "btn-network-save")?.addEventListener("click", () => this.save());
    part(this.cardEl, "btn-network-discard")?.addEventListener("click", () => void this.discard());
  }

  // discard reverts the network form to the saved config, confirming first when
  // there are unsaved edits so a stray click cannot drop them.
  private async discard(): Promise<void> {
    if (this.dirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "The network settings have unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    const cfg = store.getState().config;
    if (cfg) this.populate(cfg);
    // populate hid the actions bar holding the Discard button focus was on,
    // dropping it to <body>; return focus to the discovery toggle.
    this.discoveryEl?.focus();
  }

  private populate(cfg: Config): void {
    if (this.cardEl) setLoading(this.cardEl, false);
    // Config is polled every 3 s, so only write an input when its value actually
    // changes: a redundant assignment is wasteful and could disturb a field the
    // operator is reading. These run only while the form is not dirty (guarded by
    // the caller), so they never overwrite an in-progress edit.
    const rtsp = part<HTMLInputElement>(this.cardEl, "sys-rtsp-listen");
    const rtspVal = cfg.listen ?? "";
    if (rtsp && rtsp.value !== rtspVal) rtsp.value = rtspVal;
    const mgmt = part<HTMLInputElement>(this.cardEl, "sys-mgmt-listen");
    const mgmtVal = cfg.management?.listen ?? "(default)";
    if (mgmt && mgmt.value !== mgmtVal) mgmt.value = mgmtVal;
    const discovery = cfg.discovery?.enabled ?? true;
    if (this.discoveryEl && this.discoveryEl.checked !== discovery) this.discoveryEl.checked = discovery;
    this.dirty = false;
    if (this.actionsEl) this.actionsEl.hidden = true;
  }

  private async save(): Promise<void> {
    const saveBtn = part<HTMLButtonElement>(this.cardEl, "btn-network-save");
    const discardBtn = part<HTMLButtonElement>(this.cardEl, "btn-network-discard");
    // setBusy keeps Save focusable, so guard re-entry against a keyboard
    // re-activation while the PATCH is in flight, matching the Access Control save.
    if (saveBtn?.getAttribute("aria-disabled") === "true") return;
    if (saveBtn) setBusy(saveBtn, "Saving...");
    if (discardBtn) discardBtn.disabled = true;
    try {
      const res = await api.patchConfig({ discovery: { enabled: this.discoveryEl?.checked ?? true } });
      this.dirty = false;
      // Seed the cached config with the authoritative PATCH response, matching the
      // auth/notify/device save paths, so a later queued read builds from this
      // change instead of a stale base.
      store.applyConfig(res.config);
      if (this.actionsEl) this.actionsEl.hidden = true;
      await store.refreshConfig();
      showToast(res.restartRequired ? "Discovery setting saved. Restart the appliance to apply." : "Discovery setting applied.");
      // Hiding the actions bar dropped focus from the Save button; return it to
      // the discovery toggle, the card's editable control.
      this.discoveryEl?.focus();
    } catch (err: unknown) {
      if (isRefusal(err)) {
        // A validation problem reads by its reason, as on the other forms;
        // its detail repeats the raw field path.
        showToast(`Save failed: ${firstProblem(err)?.reason ?? apiErrorMessage(err)}`, "error");
      } else {
        // As for the notification settings: the form keeps the edit, and a
        // second save is safe.
        showUnconfirmed("that the discovery setting was saved", "save again to be sure");
      }
    } finally {
      if (saveBtn) clearBusy(saveBtn, "Save Changes");
      if (discardBtn) discardBtn.disabled = false;
    }
  }
}
