import { api, apiErrorMessage, firstProblem, isRefusal } from "../../lib/api.ts";
import { store } from "../../lib/store.ts";
import { clearBusy, elem, part, setBusy, setFieldError, setLoading, showUnconfirmed } from "../../lib/ui.ts";
import { confirmDialog } from "../../lib/modal.ts";
import { sentence } from "../../lib/text.ts";
import {
  NOTIFY_FIELDS,
  buildNotificationsPatch,
  fieldForServerPath,
  unparsedThresholds,
  type NotifyFieldSpec,
} from "../../lib/notification-settings-core.ts";
import { showToast } from "../../components/toast.ts";
import type { Config } from "../../lib/types.ts";

// NotificationsCard is the Notifications card (#sys-notifications-card): the
// condition-monitor switch and thresholds, with the save and discard flow.
export class NotificationsCard {
  private readonly cardEl: HTMLElement | null;
  private readonly actionsEl: HTMLElement | null;
  private readonly enabledEl: HTMLInputElement | null;
  private readonly errorEl: HTMLElement | null;
  // Threshold inputs and their .form-field wrappers, keyed by NotifyFieldSpec.key,
  // built once in bind so a config poll only rewrites their values.
  private readonly inputs = new Map<string, HTMLInputElement>();
  private readonly fields = new Map<string, HTMLElement>();
  private dirty = false;
  private saving = false;

  constructor(root: HTMLElement | null) {
    this.cardEl = root;
    this.actionsEl = part(root, "sys-notify-actions");
    this.enabledEl = part<HTMLInputElement>(root, "sys-notify-enabled");
    this.errorEl = part(root, "sys-notify-error");
    // Laid out from the first paint, and usable once the first config read
    // fills it.
    if (root) setLoading(root, true);
    this.bind();
  }

  // config fills the card from a config read, unless it holds unsaved edits.
  public config(cfg: Config): void {
    if (!this.dirty) this.populate(cfg);
  }

  // buildField builds one threshold input (label, number input with the
  // contract's min/max, error, hint) into its group container and records the
  // input and its .form-field wrapper for later population and error marking. It
  // mirrors the device form's field() helper; the server is the authority on
  // bounds, so there is no live validation here beyond the native min/max.
  private buildField(container: HTMLElement, spec: NotifyFieldSpec): void {
    const id = `sys-notify-${spec.key}`;
    const field = elem("div", "form-field");
    const label = elem("label", "field-label", spec.label);
    label.setAttribute("for", id);
    field.appendChild(label);

    const input = document.createElement("input");
    input.type = "number";
    input.className = "field-input mono";
    input.id = id;
    input.min = String(spec.min);
    input.max = String(spec.max);
    input.step = "1";
    // Only hint the digits-only keypad for non-negative fields: a numeric
    // inputMode omits the minus sign on mobile, which would block editing a
    // negative-bounded field (quietDbfs, min -99); its default keyboard keeps it.
    if (spec.min >= 0) input.inputMode = "numeric";
    input.setAttribute("aria-describedby", `${id}-err ${id}-hint`);
    input.addEventListener("input", () => this.markDirty(spec.key));
    field.appendChild(input);

    const error = elem("span", "field-error");
    error.id = `${id}-err`;
    field.appendChild(error);
    const hint = elem("span", "field-hint", spec.hint);
    hint.id = `${id}-hint`;
    field.appendChild(hint);

    container.appendChild(field);
    this.inputs.set(spec.key, input);
    this.fields.set(spec.key, field);
  }

  private bind(): void {
    const audioEl = part(this.cardEl, "sys-notify-audio-fields");
    const hostEl = part(this.cardEl, "sys-notify-host-fields");
    // Build the inputs once, and only into both containers, never half the
    // form.
    if (audioEl && hostEl) {
      for (const spec of NOTIFY_FIELDS) {
        this.buildField(spec.group === "audio" ? audioEl : hostEl, spec);
      }
    }
    this.enabledEl?.addEventListener("change", () => {
      this.markDirty();
      this.applyEnabledState();
    });
    part(this.cardEl, "btn-notify-save")?.addEventListener("click", () => void this.save());
    part(this.cardEl, "btn-notify-discard")?.addEventListener("click", () => void this.discard());
  }

  // errorFor is the error line under one threshold input.
  private errorFor(key: string): HTMLElement | null {
    return part(this.cardEl, `sys-notify-${key}-err`);
  }

  // markDirty stages a change: it reveals the actions bar and clears the edited
  // field's error (and the form-level error) so a stale 422 does not linger
  // over a value the operator has since changed.
  private markDirty(key?: string): void {
    this.dirty = true;
    if (this.actionsEl) this.actionsEl.hidden = false;
    if (key) {
      this.fields.get(key)?.classList.remove("invalid");
      this.inputs.get(key)?.setAttribute("aria-invalid", "false");
    }
    if (this.errorEl) this.errorEl.textContent = "";
  }

  // applyEnabledState greys the thresholds when the monitors are off, so it is
  // clear they are inert. Their values are still read on save (a disabled input
  // keeps its value), so turning the monitors back on preserves them.
  private applyEnabledState(): void {
    const on = this.enabledEl?.checked ?? true;
    for (const input of this.inputs.values()) input.disabled = !on;
  }

  private clearErrors(): void {
    for (const [key, input] of this.inputs) {
      this.fields.get(key)?.classList.remove("invalid");
      input.setAttribute("aria-invalid", "false");
    }
    if (this.errorEl) this.errorEl.textContent = "";
  }

  private populate(cfg: Config): void {
    if (this.cardEl) setLoading(this.cardEl, false);
    const n = cfg.notifications;
    const enabled = n?.enabled ?? true;
    if (this.enabledEl && this.enabledEl.checked !== enabled) {
      this.enabledEl.checked = enabled;
    }
    for (const spec of NOTIFY_FIELDS) {
      const input = this.inputs.get(spec.key);
      if (!input) continue;
      const group = (spec.group === "audio" ? n?.audio : n?.host) as
        | Record<string, number | undefined>
        | undefined;
      const value = group?.[spec.key];
      const text = value === undefined ? "" : String(value);
      // Only write when changed: this runs on every 3 s config poll (while not
      // dirty), and a redundant assignment would disturb a field being read.
      if (input.value !== text) input.value = text;
    }
    this.clearErrors();
    this.dirty = false;
    this.applyEnabledState();
    if (this.actionsEl) this.actionsEl.hidden = true;
  }

  // discard reverts the thresholds to the saved config, confirming first when
  // there are unsaved edits so a stray click cannot drop them.
  private async discard(): Promise<void> {
    if (this.dirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "The notification settings have unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    const cfg = store.getState().config;
    if (cfg) this.populate(cfg);
    // populate hid the actions bar holding the Discard button focus was on,
    // dropping it to <body>. Return focus to the card's stable top control,
    // matching save.
    this.enabledEl?.focus();
  }

  // save persists the whole notifications block. On a 422 it marks the
  // offending input (mapping the server's field path via the core helper) so a
  // clear-above-onset error lands on that input rather than as a bare toast.
  private async save(): Promise<void> {
    if (this.saving) return;
    const values: Record<string, string> = {};
    for (const [key, input] of this.inputs) values[key] = input.value;
    // Reject a blank or non-integer box before sending: the server treats an
    // absent field as unchanged, so omitting it (buildNotificationsPatch's
    // defensive behavior) would report "applied" while silently keeping the old
    // value. Mark the offending inputs and abort instead.
    const invalid = unparsedThresholds(values);
    if (invalid.length > 0) {
      this.clearErrors();
      for (const key of invalid) {
        setFieldError(this.fields.get(key) ?? null, this.inputs.get(key), this.errorFor(key), "Enter a whole number.");
      }
      if (this.errorEl) {
        this.errorEl.textContent = "Some thresholds are blank or not whole numbers.";
      }
      const firstInvalid = invalid[0];
      if (firstInvalid !== undefined) this.inputs.get(firstInvalid)?.focus();
      return;
    }
    const patch = buildNotificationsPatch(this.enabledEl?.checked ?? true, values);
    const saveBtn = part<HTMLButtonElement>(this.cardEl, "btn-notify-save");
    const discardBtn = part<HTMLButtonElement>(this.cardEl, "btn-notify-discard");
    this.saving = true;
    // Busy affordance that keeps Save focusable (see setBusy); saving guards
    // re-entry.
    if (saveBtn) setBusy(saveBtn, "Saving...");
    if (discardBtn) discardBtn.disabled = true;
    this.clearErrors();
    try {
      const res = await api.patchConfig({ notifications: patch });
      this.dirty = false;
      store.applyConfig(res.config);
      await store.refreshStatus();
      if (this.actionsEl) this.actionsEl.hidden = true;
      showToast(res.restartRequired
        ? "Notification settings saved. Restart the appliance to finish applying the configuration."
        : "Notification settings applied.", res.restartRequired ? "warn" : "info");
      // Disabling the Save button the operator just activated dropped keyboard
      // focus to <body>, and hiding the actions bar on success removes the
      // landing spot; re-enabling in the finally does not restore focus. Move it
      // to the enabled toggle, the card's stable top control, matching the auth
      // card's focus-restoration convention. The error path leaves focus on the
      // offending input (see showError), so only the success path does this.
      this.enabledEl?.focus();
    } catch (err: unknown) {
      this.showError(err);
    } finally {
      this.saving = false;
      if (saveBtn) clearBusy(saveBtn, "Save Changes");
      if (discardBtn) discardBtn.disabled = false;
    }
  }

  // showError maps a failed save onto the form: a validation error marks the
  // named input (or the form-level region when the path is not one the card
  // owns) and refocuses it; any other failure is a toast.
  private showError(err: unknown): void {
    const problem = firstProblem(err);
    if (problem) {
      const spec = problem.field ? fieldForServerPath(problem.field) : null;
      if (spec) {
        // Field reasons read as sentences, as on the device form.
        const input = this.inputs.get(spec.key);
        setFieldError(this.fields.get(spec.key) ?? null, input, this.errorFor(spec.key), sentence(problem.reason));
        input?.focus();
        return;
      }
      if (this.errorEl) {
        this.errorEl.textContent = sentence(problem.reason);
        return;
      }
    }
    if (isRefusal(err)) {
      showToast(`Save failed: ${apiErrorMessage(err)}`, "error");
      return;
    }
    // It may have applied. The form keeps the edits (a refresh would not
    // show over a dirty form), and saving the same values again is safe.
    showUnconfirmed("that the notification settings were saved", "save again to be sure");
  }
}
