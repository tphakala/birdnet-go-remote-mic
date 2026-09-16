import { api, ApiError } from "../lib/api.js";
import { store } from "../lib/store.js";
import { deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError, setText } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
import { triggerApplianceRestart } from "../components/restart-modal.js";
import { showToast } from "../components/toast.js";
import { generateToken, setToken } from "../lib/auth.js";
import {
  NOTIFY_FIELDS,
  buildNotificationsPatch,
  fieldForServerPath,
  unparsedThresholds,
  type NotifyFieldSpec,
} from "../lib/notification-settings-core.js";
import type { ApplianceStatus, Config, Device, LoadError, SystemInfo } from "../lib/types.js";

// TOKEN_RULE mirrors the appliance's auth.token validation (auth.ValidToken)
// so an obviously invalid token is caught before the round trip.
const TOKEN_RULE = /^(|[A-Za-z0-9._~-]{12,128})$/;

export class SystemView {
  private tilesEl: HTMLElement | null;
  private infoEl: HTMLElement | null;
  private infoCardEl: HTMLElement | null;
  private rowsEl: HTMLElement | null;
  private system: SystemInfo | null = null;
  private status: ApplianceStatus | null = null;

  private netCardEl: HTMLElement | null;
  private netActionsEl: HTMLElement | null;
  private discoveryEl: HTMLInputElement | null;
  private netDirty = false;

  private authCardEl: HTMLElement | null;
  private authStateEl: HTMLElement | null;
  private authTokenEl: HTMLInputElement | null;
  private authErrorEl: HTMLElement | null;
  private authActionsEl: HTMLElement | null;
  private authDirty = false;
  private authSaving = false;

  private notifyCardEl: HTMLElement | null;
  private notifyActionsEl: HTMLElement | null;
  private notifyEnabledEl: HTMLInputElement | null;
  private notifyErrorEl: HTMLElement | null;
  // Threshold inputs and their .form-field wrappers, keyed by NotifyFieldSpec.key,
  // built once in bindNotifications so a config poll only rewrites their values.
  private notifyInputs = new Map<string, HTMLInputElement>();
  private notifyFields = new Map<string, HTMLElement>();
  private notifyDirty = false;
  private notifySaving = false;

  constructor() {
    this.tilesEl = document.getElementById("sys-tiles");
    this.infoEl = document.getElementById("sys-info");
    this.infoCardEl = document.getElementById("sys-info-card");
    this.rowsEl = document.getElementById("sys-device-rows");
    this.netCardEl = document.getElementById("sys-network-card");
    this.netActionsEl = document.getElementById("sys-network-actions");
    this.discoveryEl = document.getElementById("sys-discovery-enabled") as HTMLInputElement | null;
    this.authCardEl = document.getElementById("sys-auth-card");
    this.authStateEl = document.getElementById("sys-auth-state");
    this.authTokenEl = document.getElementById("sys-auth-token") as HTMLInputElement | null;
    this.authErrorEl = document.getElementById("sys-auth-error");
    this.authActionsEl = document.getElementById("sys-auth-actions");
    this.notifyCardEl = document.getElementById("sys-notifications-card");
    this.notifyActionsEl = document.getElementById("sys-notify-actions");
    this.notifyEnabledEl = document.getElementById("sys-notify-enabled") as HTMLInputElement | null;
    this.notifyErrorEl = document.getElementById("sys-notify-error");
    const btn = document.getElementById("btn-sys-restart") as HTMLButtonElement | null;
    if (btn) btn.addEventListener("click", () => triggerApplianceRestart());

    store.addEventListener("system", (e: Event) => {
      this.system = (e as CustomEvent<SystemInfo>).detail;
      this.renderTiles();
      this.renderInfo();
    });
    store.addEventListener("status", (e: Event) => {
      this.status = (e as CustomEvent<ApplianceStatus>).detail;
      this.renderTiles();
      this.renderInfo();
    });
    store.addEventListener("devices", (e: Event) => {
      this.renderDeviceRows((e as CustomEvent<Device[]>).detail);
    });
    store.addEventListener("config", (e: Event) => {
      const cfg = (e as CustomEvent<Config>).detail;
      if (!this.netDirty && cfg) this.populateNetwork(cfg);
      if (!this.authDirty && cfg) this.populateAuth(cfg);
      if (!this.notifyDirty && cfg) this.populateNotifications(cfg);
    });
    store.addEventListener("loaderror", (e: Event) => {
      const detail = (e as CustomEvent<LoadError>).detail;
      if (detail.systemFailed) this.renderLoadError(detail.message);
    });
    this.bindNetwork();
    this.bindAuth();
    this.bindNotifications();
  }

  // buildNotifyField builds one threshold input (label, number input with the
  // contract's min/max, error, hint) into its group container and records the
  // input and its .form-field wrapper for later population and error marking. It
  // mirrors the device form's field() helper; the server is the authority on
  // bounds, so there is no live validation here beyond the native min/max.
  private buildNotifyField(container: HTMLElement, spec: NotifyFieldSpec): void {
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
    input.addEventListener("input", () => this.markNotifyDirty(spec.key));
    field.appendChild(input);

    const error = elem("span", "field-error");
    error.id = `${id}-err`;
    field.appendChild(error);
    const hint = elem("span", "field-hint", spec.hint);
    hint.id = `${id}-hint`;
    field.appendChild(hint);

    container.appendChild(field);
    this.notifyInputs.set(spec.key, input);
    this.notifyFields.set(spec.key, field);
  }

  private bindNotifications(): void {
    const audioEl = document.getElementById("sys-notify-audio-fields");
    const hostEl = document.getElementById("sys-notify-host-fields");
    // Build the inputs once. Without both containers the card cannot render, so
    // leave it hidden rather than half-built.
    if (audioEl && hostEl) {
      for (const spec of NOTIFY_FIELDS) {
        this.buildNotifyField(spec.group === "audio" ? audioEl : hostEl, spec);
      }
    }
    this.notifyEnabledEl?.addEventListener("change", () => {
      this.markNotifyDirty();
      this.applyNotifyEnabledState();
    });
    document.getElementById("btn-notify-save")?.addEventListener("click", () => void this.saveNotifications());
    document.getElementById("btn-notify-discard")?.addEventListener("click", () => void this.discardNotifications());
  }

  // markNotifyDirty stages a change: it reveals the actions bar and clears the
  // edited field's error (and the form-level error) so a stale 422 does not
  // linger over a value the operator has since changed.
  private markNotifyDirty(key?: string): void {
    this.notifyDirty = true;
    if (this.notifyActionsEl) this.notifyActionsEl.hidden = false;
    if (key) this.notifyFields.get(key)?.classList.remove("invalid");
    if (this.notifyErrorEl) this.notifyErrorEl.textContent = "";
  }

  // applyNotifyEnabledState greys the thresholds when the monitors are off, so it
  // is clear they are inert. Their values are still read on save (a disabled
  // input keeps its value), so turning the monitors back on preserves them.
  private applyNotifyEnabledState(): void {
    const on = this.notifyEnabledEl?.checked ?? true;
    for (const input of this.notifyInputs.values()) input.disabled = !on;
  }

  private clearNotifyErrors(): void {
    for (const field of this.notifyFields.values()) field.classList.remove("invalid");
    if (this.notifyErrorEl) this.notifyErrorEl.textContent = "";
  }

  private populateNotifications(cfg: Config): void {
    if (this.notifyCardEl) this.notifyCardEl.hidden = false;
    const n = cfg.notifications;
    const enabled = n?.enabled ?? true;
    if (this.notifyEnabledEl && this.notifyEnabledEl.checked !== enabled) {
      this.notifyEnabledEl.checked = enabled;
    }
    for (const spec of NOTIFY_FIELDS) {
      const input = this.notifyInputs.get(spec.key);
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
    this.clearNotifyErrors();
    this.notifyDirty = false;
    this.applyNotifyEnabledState();
    if (this.notifyActionsEl) this.notifyActionsEl.hidden = true;
  }

  // discardNotifications reverts the thresholds to the saved config, confirming
  // first when there are unsaved edits so a stray click cannot drop them.
  private async discardNotifications(): Promise<void> {
    if (this.notifyDirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "The notification settings have unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    const cfg = store.getState().config;
    if (cfg) this.populateNotifications(cfg);
  }

  // saveNotifications persists the whole notifications block. On a 422 it marks
  // the offending input (mapping the server's field path via the core helper) so
  // a clear-above-onset error lands on that input rather than as a bare toast.
  private async saveNotifications(): Promise<void> {
    if (this.notifySaving) return;
    const values: Record<string, string> = {};
    for (const [key, input] of this.notifyInputs) values[key] = input.value;
    // Reject a blank or non-integer box before sending: the server treats an
    // absent field as unchanged, so omitting it (buildNotificationsPatch's
    // defensive behavior) would report "applied" while silently keeping the old
    // value. Mark the offending inputs and abort instead.
    const invalid = unparsedThresholds(values);
    if (invalid.length > 0) {
      this.clearNotifyErrors();
      for (const key of invalid) {
        this.notifyFields.get(key)?.classList.add("invalid");
        const errEl = document.getElementById(`sys-notify-${key}-err`);
        if (errEl) errEl.textContent = "Enter a whole number.";
      }
      if (this.notifyErrorEl) {
        this.notifyErrorEl.textContent = "Some thresholds are blank or not whole numbers.";
      }
      this.notifyInputs.get(invalid[0])?.focus();
      return;
    }
    const patch = buildNotificationsPatch(this.notifyEnabledEl?.checked ?? true, values);
    const saveBtn = document.getElementById("btn-notify-save") as HTMLButtonElement | null;
    const discardBtn = document.getElementById("btn-notify-discard") as HTMLButtonElement | null;
    this.notifySaving = true;
    if (saveBtn) {
      saveBtn.disabled = true;
      saveBtn.setAttribute("aria-busy", "true");
      saveBtn.textContent = "Saving...";
    }
    if (discardBtn) discardBtn.disabled = true;
    this.clearNotifyErrors();
    try {
      const res = await api.patchConfig({ notifications: patch });
      this.notifyDirty = false;
      store.applyConfig(res.config);
      await store.refreshStatus();
      if (this.notifyActionsEl) this.notifyActionsEl.hidden = true;
      showToast(res.restartRequired
        ? "Notification settings saved. Restart the appliance to finish applying the configuration."
        : "Notification settings applied.", res.restartRequired ? "warn" : "info");
      // Disabling the Save button the operator just activated dropped keyboard
      // focus to <body>, and hiding the actions bar on success removes the
      // landing spot; re-enabling in the finally does not restore focus. Move it
      // to the enabled toggle, the card's stable top control, matching the auth
      // card's focus-restoration convention. The error path leaves focus on the
      // offending input (see showNotifyError), so only the success path does this.
      this.notifyEnabledEl?.focus();
    } catch (err: unknown) {
      this.showNotifyError(err);
    } finally {
      this.notifySaving = false;
      if (saveBtn) {
        saveBtn.disabled = false;
        saveBtn.removeAttribute("aria-busy");
        saveBtn.textContent = "Save Changes";
      }
      if (discardBtn) discardBtn.disabled = false;
    }
  }

  // showNotifyError maps a failed save onto the form: a validation error marks
  // the named input (or the form-level region when the path is not one the card
  // owns) and refocuses it; any other failure is a toast.
  private showNotifyError(err: unknown): void {
    if (err instanceof ApiError && err.errors && err.errors.length > 0) {
      const item = err.errors[0];
      const spec = item.field ? fieldForServerPath(item.field) : null;
      const reason = item.reason ?? err.title;
      if (spec) {
        this.notifyFields.get(spec.key)?.classList.add("invalid");
        const errEl = document.getElementById(`sys-notify-${spec.key}-err`);
        if (errEl) errEl.textContent = reason;
        const input = this.notifyInputs.get(spec.key);
        input?.focus();
        return;
      }
      if (this.notifyErrorEl) {
        this.notifyErrorEl.textContent = reason;
        return;
      }
    }
    const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
    showToast(`Save failed: ${msg}`, "error");
  }

  private bindAuth(): void {
    const input = this.authTokenEl;
    if (!input) return;
    input.addEventListener("input", () => {
      this.authDirty = true;
      this.setAuthError("");
      if (this.authActionsEl) this.authActionsEl.hidden = false;
    });
    const reveal = document.getElementById("btn-auth-reveal");
    reveal?.addEventListener("click", () => {
      const show = input.type === "password";
      input.type = show ? "text" : "password";
      // The visible label and the accessible name both swap Show/Hide; there is
      // no aria-pressed, so the state is carried by the label rather than by a
      // pressed toggle contradicting a changing label.
      reveal.textContent = show ? "Hide" : "Show";
      reveal.setAttribute("aria-label", show ? "Hide access token" : "Show access token");
    });
    document.getElementById("btn-auth-copy")?.addEventListener("click", () => {
      const value = input.value.trim();
      if (!value) {
        showToast("No token to copy: the appliance is on open access.", "warn");
        return;
      }
      if (!navigator.clipboard) return;
      navigator.clipboard.writeText(value)
        .then(() => showToast("Access token copied."))
        .catch(() => showToast("Copy failed", "error"));
    });
    document.getElementById("btn-auth-generate")?.addEventListener("click", () => {
      input.value = generateToken();
      // Reveal the generated value: the operator needs to see it to copy it into
      // BirdNET-Go, and a masked random string cannot be verified by eye.
      input.type = "text";
      if (reveal) {
        reveal.textContent = "Hide";
        reveal.setAttribute("aria-label", "Hide access token");
      }
      input.dispatchEvent(new Event("input"));
      input.focus();
    });
    document.getElementById("btn-auth-save")?.addEventListener("click", () => void this.saveAuth());
    document.getElementById("btn-auth-discard")?.addEventListener("click", () => void this.discardAuth());
  }

  private setAuthError(message: string): void {
    if (this.authErrorEl) this.authErrorEl.textContent = message;
    this.authTokenEl?.setAttribute("aria-invalid", message ? "true" : "false");
    this.authTokenEl?.closest(".form-field")?.classList.toggle("invalid", !!message);
  }

  private populateAuth(cfg: Config): void {
    if (this.authCardEl) this.authCardEl.hidden = false;
    const token = cfg.auth?.token ?? "";
    // Only write when changed: this runs on every 3 s config poll (while not
    // dirty) and a redundant assignment to a field the operator has revealed is
    // needless churn.
    if (this.authTokenEl && this.authTokenEl.value !== token) this.authTokenEl.value = token;
    if (this.authStateEl) {
      const stateText = token
        ? "Token required: the API, this UI and the RTSP streams ask for credentials."
        : "Open access: anyone on the network can listen and change settings.";
      // #sys-auth-state is a role=status region rewritten on every config event;
      // setText only writes when the text actually changes so a steady state is
      // not re-announced to screen readers each poll.
      setText(this.authStateEl, stateText);
      this.authStateEl.classList.toggle("locked", !!token);
      this.authStateEl.classList.toggle("open", !token);
    }
    this.setAuthError("");
    this.authDirty = false;
    if (this.authActionsEl) this.authActionsEl.hidden = true;
  }

  // discardAuth reverts the token field to the saved value, confirming first
  // when there are unsaved edits so a stray click cannot drop a generated token.
  private async discardAuth(): Promise<void> {
    if (this.authDirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "The access token has unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    const cfg = store.getState().config;
    if (cfg) this.populateAuth(cfg);
  }

  // saveAuth persists the token. Clearing it opens the appliance to the network,
  // so that path confirms first. On success the UI's own stored token is swapped
  // synchronously before any follow-up request, so the next poll already carries
  // the new value and cannot be rejected by the freshly rotated appliance.
  private async saveAuth(): Promise<void> {
    if (this.authSaving || !this.authTokenEl) return;
    const token = this.authTokenEl.value.trim();
    if (!TOKEN_RULE.test(token)) {
      this.setAuthError("Use 12 to 128 characters: letters, digits, and . _ ~ - only.");
      this.authTokenEl.focus();
      return;
    }
    if (!token) {
      const ok = await confirmDialog({
        title: "Allow open access?",
        body: "Removing the token lets anyone on the network listen to the microphones and change settings.",
        confirmLabel: "Allow open access",
        danger: true,
      });
      if (!ok) return;
    }
    const saveBtn = document.getElementById("btn-auth-save") as HTMLButtonElement | null;
    const discardBtn = document.getElementById("btn-auth-discard") as HTMLButtonElement | null;
    this.authSaving = true;
    if (saveBtn) {
      saveBtn.disabled = true;
      saveBtn.setAttribute("aria-busy", "true");
      saveBtn.textContent = "Saving...";
    }
    if (discardBtn) discardBtn.disabled = true;
    // Open the store's rotation window so an in-flight poll rejected while the
    // appliance is switching tokens (it enforces the new one before the PATCH
    // response is fully written) does not pop the login prompt over a working
    // page. It is closed in the finally, after setToken has run.
    store.beginTokenSwap();
    try {
      const res = await api.patchConfig({ auth: { token } });
      this.authDirty = false;
      store.applyConfig(res.config);
      // The appliance enforces a patched token immediately, before the reload:
      // mgmtserver PatchConfig calls guard.Set(token) unconditionally right
      // after persisting and BEFORE invoking the reloader, so the new token is
      // live regardless of the outcome. restartRequired is computed only from
      // whether the reloader succeeded; it says nothing about the token, only
      // that the rest of the reload did not take effect. So adopt the new token
      // on both branches, before anything else runs (see setToken): skipping it
      // would leave the UI holding a credential the appliance no longer accepts.
      setToken(token || null);
      // applyConfig above already seeded the authoritative config, so only
      // refreshStatus is needed (it carries authRequired).
      await store.refreshStatus();
      if (res.restartRequired) {
        // The token is already live; it is the rest of the configuration that
        // needs a restart before it takes effect.
        showToast(token
          ? "Access token saved and active. Restart the appliance to finish applying the configuration."
          : "Open access saved and active. Restart the appliance to finish applying the configuration.", "warn");
      } else {
        showToast(token ? "Access token saved. BirdNET-Go and players now need it." : "Open access enabled.", token ? "info" : "warn");
      }
    } catch (err: unknown) {
      if (err instanceof ApiError && err.errors && err.errors.length > 0) {
        this.setAuthError(err.errors[0].reason ?? err.title);
      } else {
        const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
        showToast(`Save failed: ${msg}`, "error");
      }
    } finally {
      store.endTokenSwap();
      this.authSaving = false;
      if (saveBtn) {
        saveBtn.disabled = false;
        saveBtn.removeAttribute("aria-busy");
        saveBtn.textContent = "Save Token";
      }
      if (discardBtn) discardBtn.disabled = false;
      // Disabling the Save button the user just activated dropped keyboard focus
      // to <body>; re-enabling does not restore it. After a successful save the
      // actions bar is hidden, so the token input is the sensible landing spot in
      // every case. Restore focus explicitly, matching the convention the toggle
      // and settings paths in dashboard.ts already follow.
      this.authTokenEl?.focus();
    }
  }

  // renderLoadError swaps the telemetry placeholder for the failure cause and a
  // Retry button so the system view is not stuck loading when /system is
  // unreachable. A successful retry re-renders via the system event.
  private renderLoadError(message: string): void {
    if (!this.tilesEl) return;
    this.tilesEl.textContent = "";
    const p = elem("p", "cfg-empty");
    this.tilesEl.appendChild(p);
    renderLoadError(p, message, "Loading system telemetry...", () => void store.retry());
  }

  private bindNetwork(): void {
    if (this.discoveryEl) {
      this.discoveryEl.addEventListener("change", () => {
        this.netDirty = true;
        if (this.netActionsEl) this.netActionsEl.hidden = false;
      });
    }
    document.getElementById("btn-network-save")?.addEventListener("click", () => this.saveNetwork());
    document.getElementById("btn-network-discard")?.addEventListener("click", () => void this.discardNetwork());
  }

  // discardNetwork reverts the network form to the saved config, confirming
  // first when there are unsaved edits so a stray click cannot drop them.
  private async discardNetwork(): Promise<void> {
    if (this.netDirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "The network settings have unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    const cfg = store.getState().config;
    if (cfg) this.populateNetwork(cfg);
  }

  private populateNetwork(cfg: Config): void {
    if (this.netCardEl) this.netCardEl.hidden = false;
    // Config is polled every 3 s, so only write an input when its value actually
    // changes: a redundant assignment is wasteful and could disturb a field the
    // operator is reading. These run only while the form is not dirty (guarded by
    // the caller), so they never overwrite an in-progress edit.
    const rtsp = document.getElementById("sys-rtsp-listen") as HTMLInputElement | null;
    const rtspVal = cfg.listen ?? "";
    if (rtsp && rtsp.value !== rtspVal) rtsp.value = rtspVal;
    const mgmt = document.getElementById("sys-mgmt-listen") as HTMLInputElement | null;
    const mgmtVal = cfg.management?.listen ?? "(default)";
    if (mgmt && mgmt.value !== mgmtVal) mgmt.value = mgmtVal;
    const discovery = cfg.discovery?.enabled ?? true;
    if (this.discoveryEl && this.discoveryEl.checked !== discovery) this.discoveryEl.checked = discovery;
    this.netDirty = false;
    if (this.netActionsEl) this.netActionsEl.hidden = true;
  }

  private async saveNetwork(): Promise<void> {
    try {
      const res = await api.patchConfig({ discovery: { enabled: this.discoveryEl?.checked ?? true } });
      this.netDirty = false;
      if (this.netActionsEl) this.netActionsEl.hidden = true;
      await store.refreshConfig();
      showToast(res.restartRequired ? "Discovery setting saved. Restart the appliance to apply." : "Discovery setting applied.");
    } catch (err: unknown) {
      const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
      showToast(`Save failed: ${msg}`, "error");
    }
  }

  private tile(label: string, sub: string, value: string, unit?: string, barPct?: number): HTMLElement {
    const tile = elem("div", "system-tile");
    const header = elem("div", "tile-header");
    header.appendChild(elem("span", undefined, label));
    if (sub) header.appendChild(elem("span", "mono", sub));
    tile.appendChild(header);

    const val = elem("div", "tile-value mono");
    val.appendChild(elem("span", undefined, value));
    if (unit) val.appendChild(elem("span", "telemetry-unit", unit));
    tile.appendChild(val);

    if (barPct !== undefined) {
      const bg = elem("div", "progress-bar-bg");
      const fill = elem("div", "progress-bar-fill");
      fill.style.width = `${Math.min(100, Math.max(0, barPct)).toFixed(1)}%`;
      bg.appendChild(fill);
      tile.appendChild(bg);
    }
    return tile;
  }

  // The tile grid holds only the four live resource gauges. Host and appliance
  // facts live in the separate System Information card below.
  private renderTiles(): void {
    if (!this.tilesEl) return;
    const sys = this.system;
    if (!sys) return;

    this.tilesEl.textContent = "";

    const cores = sys.cpuCores > 0 ? `${sys.cpuCores} Cores` : "";
    this.tilesEl.appendChild(this.tile("CPU Utilization", cores,
      sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a", "%",
      sys.cpuPercent ?? 0));

    if (sys.memTotalBytes > 0) {
      const pct = (sys.memUsedBytes / sys.memTotalBytes) * 100;
      const usedMb = Math.round(sys.memUsedBytes / 1048576);
      const totalMb = Math.round(sys.memTotalBytes / 1048576);
      this.tilesEl.appendChild(this.tile("Memory", `${totalMb} MB Total`,
        String(usedMb), "MB used", pct));
    }

    this.tilesEl.appendChild(this.tile("SoC Temperature", "",
      sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a", "deg C",
      sys.tempCelsius !== undefined ? (sys.tempCelsius / 85) * 100 : undefined));

    if (sys.diskTotalBytes > 0) {
      const pct = (sys.diskUsedBytes / sys.diskTotalBytes) * 100;
      const usedGb = (sys.diskUsedBytes / 1073741824).toFixed(1);
      const totalGb = (sys.diskTotalBytes / 1073741824).toFixed(1);
      this.tilesEl.appendChild(this.tile("Disk", `${totalGb} GB Total`,
        usedGb, "GB used", pct));
    }
  }

  private renderInfo(): void {
    if (!this.infoEl) return;
    const sys = this.system;
    const st = this.status;
    if (!sys && !st) return;

    const rows: Array<[string, string]> = [];
    if (sys) {
      rows.push(["Hostname", sys.hostname || "-"]);
      rows.push(["Platform", sys.platform || "-"]);
      if (sys.cpuModel) rows.push(["CPU", `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`]);
      if (sys.os) rows.push(["OS", sys.os]);
      if (sys.kernel) rows.push(["Kernel", sys.kernel]);
    }
    if (st) {
      rows.push(["Version", st.version || "-"]);
      rows.push(["Uptime", formatUptime(st.uptimeSeconds)]);
      rows.push(["RTSP Listen", st.rtspListen]);
      rows.push(["Devices Serving", `${st.devicesServing} / ${st.devicesTotal}`]);
    }

    this.infoEl.textContent = "";
    for (const [k, v] of rows) {
      const dt = elem("dt", "info-key", k);
      const dd = elem("dd", "info-val mono", v);
      this.infoEl.appendChild(dt);
      this.infoEl.appendChild(dd);
    }
    if (this.infoCardEl) this.infoCardEl.hidden = rows.length === 0;
  }

  private renderDeviceRows(devices: Device[]): void {
    if (!this.rowsEl) return;
    this.rowsEl.textContent = "";

    if (devices.length === 0) {
      const tr = document.createElement("tr");
      const td = elem("td", undefined, "No devices configured.");
      td.setAttribute("colspan", "6");
      tr.appendChild(td);
      this.rowsEl.appendChild(tr);
      return;
    }

    for (const d of devices) {
      const tr = document.createElement("tr");
      tr.appendChild(this.td(d.name));
      tr.appendChild(this.td(d.device, true));
      tr.appendChild(this.td(d.path, true));
      const rate = d.negotiatedRate ?? d.rate;
      tr.appendChild(this.td(`${modeLabel(d.mode)} ${rate.toLocaleString("en-US")} Hz`, true));
      tr.appendChild(this.td(d.clientConnected ? "Connected" : "-", true));

      const stateTd = document.createElement("td");
      const badge = deviceStateBadge(d.state);
      stateTd.appendChild(elem("span", badge.cls, badge.label));
      tr.appendChild(stateTd);

      this.rowsEl.appendChild(tr);
    }
  }

  private td(text: string, mono = false): HTMLElement {
    return elem("td", mono ? "mono" : undefined, text);
  }
}
