import { store } from "../lib/store.js";
import { VUMeter } from "../components/vu-meter.js";
import { DeviceSettingsForm } from "../components/device-settings.js";
import { showToast } from "../components/toast.js";
import { api, ApiError } from "../lib/api.js";
import { deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
import type { ApplianceStatus, Device, DeviceConfig, DeviceLevels, LoadError, SystemInfo } from "../lib/types.js";

// Trusted static SVG icon markup (no interpolation of runtime data).
const ICON_MIC =
  '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path></svg>';
const ICON_ULTRA =
  '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12h2"></path><path d="M6 8v8"></path><path d="M10 4v16"></path><path d="M14 6v12"></path><path d="M18 9v6"></path><path d="M22 12h-2"></path></svg>';
const ICON_ERROR =
  '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line></svg>';
const ICON_WARN =
  '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>';
const ICON_COPY =
  '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"></rect><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"></path></svg>';
const ICON_GEAR =
  '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"></path><circle cx="12" cy="12" r="3"></circle></svg>';

interface CardEntry {
  article: HTMLElement;
  gearBtn: HTMLElement;
  serving: boolean;
  // One VU meter per capture channel (empty for a non-serving card). A mono
  // device has a single meter; a stereo (or higher) device has one per channel.
  meters: VUMeter[];
  urlEl: HTMLElement | null;
  statusEl: HTMLElement | null;
  clientsEl: HTMLElement | null;
  droppedEl: HTMLElement | null;
  toggleInput: HTMLInputElement;
  // Transient banner shown while the desired (config) enabled flag diverges from
  // the live runtime state, i.e. during the brief moment a config reload is
  // starting or stopping the device.
  pendingNote: HTMLElement;
  // The non-serving footer message, if this card is not serving; updated in
  // place so an in-session toggle does not leave stale disabled/enabled text.
  footerNote: HTMLElement | null;
  device: Device;
  settingsWrap: HTMLElement;
  settingsForm: DeviceSettingsForm | null;
  expanded: boolean;
  dirty: boolean;
}

// runtimeEnabled reports whether a device is on at runtime. A disabled device is
// off; anything else (serving, or a failed/skipped device that was configured
// to stream) is on until the operator changes it.
function runtimeEnabled(state: string): boolean {
  return state !== "disabled";
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

// nonServingFooterText is the footer message for a card that is not serving,
// shared by buildCard and updateCard so an in-session toggle never leaves stale
// enabled/disabled wording behind.
function nonServingFooterText(state: string, configEnabled: boolean): string {
  if (state === "disabled") {
    return configEnabled
      ? "Enabling; this device starts serving shortly."
      : "Streaming is disabled for this device. Enable it to start serving.";
  }
  return "Excluded from the RTSP stream server. Other active devices continue serving without interruption.";
}

function iconSpan(markup: string, className?: string): HTMLElement {
  const s = document.createElement("span");
  if (className) s.className = className;
  // Static trusted markup only; never runtime/user data.
  s.innerHTML = markup;
  return s;
}

function channelLabel(channels: number): string {
  if (channels === 1) return "Mono";
  if (channels === 2) return "Stereo";
  return `${channels} ch`;
}

// The runtime device carries all configured fields; project it to the config
// shape as a fallback base for the device-list patch.
function deviceToConfig(d: Device): DeviceConfig {
  const c: DeviceConfig = {
    name: d.name, device: d.device, path: d.path, mode: d.mode,
    rate: d.rate, channels: d.channels, format: d.format,
    enabled: runtimeEnabled(d.state),
  };
  if (d.opus) c.opus = d.opus;
  return c;
}

function rtspPort(listen: string | undefined): string {
  if (!listen) return "8554";
  const i = listen.lastIndexOf(":");
  return i >= 0 ? listen.slice(i + 1) : listen;
}

export class DashboardView {
  private cards: Map<string, CardEntry> = new Map();
  private rack: HTMLElement | null;
  private emptyEl: HTMLElement | null;
  private status: ApplianceStatus | null = null;
  // Serializes config mutations (device toggle + settings save) so each PATCH is
  // built from a fresh base only after the previous mutation settled. Prevents a
  // full-array PATCH from a stale base from clobbering a concurrent change.
  private mutationQueue: Promise<void> = Promise.resolve();

  constructor() {
    this.rack = document.getElementById("channel-rack");
    this.emptyEl = document.getElementById("rack-empty");
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
    store.addEventListener("devices", (e: Event) => {
      this.renderDevices((e as CustomEvent<Device[]>).detail);
    });
    store.addEventListener("status", (e: Event) => {
      this.status = (e as CustomEvent<ApplianceStatus>).detail;
      this.updateTelemetryFromStatus();
      // Recompute RTSP URLs now that the listen port is known.
      this.renderDevices(store.getState().devices);
    });
    store.addEventListener("system", (e: Event) => {
      this.updateTelemetryFromSystem((e as CustomEvent<SystemInfo>).detail);
    });
    store.addEventListener("levels", (e: Event) => {
      const levels = (e as CustomEvent<Map<string, DeviceLevels>>).detail;
      levels.forEach((dl, name) => {
        const entry = this.cards.get(name);
        if (!entry) return;
        for (const ch of dl.channels) {
          const meter = entry.meters[ch.channel];
          if (meter) meter.setLevels(ch.rmsDbfs, ch.peakDbfs, ch.clipped);
        }
      });
    });
    store.addEventListener("connection", (e: Event) => {
      this.updateConnection((e as CustomEvent<boolean>).detail);
    });
    store.addEventListener("loaderror", (e: Event) => {
      const detail = (e as CustomEvent<LoadError>).detail;
      if (detail.coreFailed) this.renderLoadError(detail.message);
    });
  }

  // renderLoadError replaces the "Loading..." placeholder with the failure cause
  // and a Retry button when the initial data fetch fails, so the view is not
  // stuck loading forever. A successful retry re-renders via the devices event.
  private renderLoadError(message: string): void {
    if (!this.emptyEl) return;
    renderLoadError(this.emptyEl, message, "Loading devices...", () => void store.retry());
  }

  private renderDevices(devices: Device[]): void {
    if (!this.rack) return;

    if (this.emptyEl) {
      this.emptyEl.hidden = devices.length > 0;
      // Clear the alert live-region role set by renderLoadError so the benign
      // empty/loaded state is not re-announced as an error.
      this.emptyEl.removeAttribute("role");
      // Replace the static "Loading devices..." placeholder once we know there
      // are genuinely zero configured devices (the element is visible here).
      if (devices.length === 0) this.emptyEl.textContent = "No capture devices are configured.";
    }

    const seen = new Set<string>();
    let clientCount = 0;

    // Index the persisted config by ALSA id once per pass so the per-card
    // reconcile is a Map lookup rather than a linear scan of the config list per
    // card (an N+1 over the device count).
    const cfgByDevice = new Map<string, DeviceConfig>();
    for (const cd of store.getState().config?.devices ?? []) cfgByDevice.set(cd.device, cd);

    for (const d of devices) {
      seen.add(d.name);
      if (d.clientConnected) clientCount++;

      const serving = d.state === "serving";
      const existing = this.cards.get(d.name);

      // Rebuild the card when it is new or when its serving state flips (which
      // changes whether a meter console exists). Otherwise update in place so
      // the live meter keeps running across the 3s device poll.
      if (!existing || existing.serving !== serving) {
        if (existing) {
          existing.meters.forEach((m) => m.destroy());
          existing.settingsForm?.destroy();
          existing.article.remove();
          this.cards.delete(d.name);
        }
        this.cards.set(d.name, this.buildCard(d, cfgByDevice));
      } else {
        this.updateCard(existing, d, cfgByDevice);
      }
    }

    // Remove cards for devices that are gone.
    for (const [name, entry] of this.cards) {
      if (!seen.has(name)) {
        entry.meters.forEach((m) => m.destroy());
        entry.settingsForm?.destroy();
        entry.article.remove();
        this.cards.delete(name);
      }
    }

    // Keep DOM order aligned with the device list (moves existing nodes,
    // preserving their meters).
    for (const d of devices) {
      const entry = this.cards.get(d.name);
      if (entry) this.rack.appendChild(entry.article);
    }

    const clientsEl = document.getElementById("total-clients-display");
    if (clientsEl) clientsEl.textContent = String(clientCount);
  }

  private rtspUrl(d: Device): string {
    const port = rtspPort(this.status?.rtspListen);
    return `rtsp://${window.location.hostname}:${port}${d.path}`;
  }

  private buildCard(d: Device, cfgByDevice: Map<string, DeviceConfig>): CardEntry {
    const serving = d.state === "serving";
    const disabled = d.state === "disabled";
    const isUltra = d.mode === "pcm";
    // The toggle reflects the persisted (desired) enabled flag, which can differ
    // from the runtime state only briefly while a config reload applies. Fall
    // back to the runtime state only when the config is not loaded.
    const cfgDev = cfgByDevice.get(d.device);
    const configEnabled = cfgDev?.enabled ?? runtimeEnabled(d.state);
    // A disabled device is off by intent, not broken: give it a neutral card and
    // avatar rather than the error styling reserved for a failed/skipped device.
    const article = elem("article", `rack-card ${serving ? "active-stream" : disabled ? "" : "error-stream"}`);

    // Header
    const header = elem("div", "rack-header");
    const ident = elem("div", "device-ident");
    const avatarIcon = serving ? (isUltra ? ICON_ULTRA : ICON_MIC) : disabled ? ICON_MIC : ICON_ERROR;
    const avatar = iconSpan(avatarIcon, "device-avatar");
    if (serving && isUltra) avatar.style.color = "var(--ultrasonic-purple)";
    if (!serving && !disabled) avatar.style.color = "var(--signal-crit)";
    const nameBlock = elem("div", "device-name-block");
    nameBlock.appendChild(elem("span", "device-title", d.name));
    // Surface the sound card's hardware model (friendlyName) next to the ALSA id
    // so a device is identifiable by what it physically is, not only its config
    // name. Omit it when absent (the device id matches no enumerated hardware) or
    // when it only repeats the configured name (e.g. "loopback" / "Loopback").
    const hw = d.friendlyName?.trim();
    const showHw = !!hw && hw.toLowerCase() !== d.name.trim().toLowerCase();
    nameBlock.appendChild(
      elem("span", "device-path mono", showHw ? `ALSA: ${d.device} · ${hw}` : `ALSA: ${d.device}`),
    );
    ident.appendChild(avatar);
    ident.appendChild(nameBlock);

    const tags = elem("div", "device-tags");
    if (serving) {
      const rate = d.negotiatedRate ?? d.rate;
      const modeTag = elem("span", `tech-tag ${isUltra ? "ultrasonic" : "highlight"}`, modeLabel(d.mode));
      tags.appendChild(modeTag);
      tags.appendChild(elem("span", "tech-tag", `${rate.toLocaleString("en-US")} Hz`));
      tags.appendChild(elem("span", "tech-tag", channelLabel(d.negotiatedChannels ?? d.channels)));
    }
    const statusEl = this.buildStatusBadge(d.state);
    tags.appendChild(statusEl);

    // Streaming enable/disable toggle. A disabled device stays configured but is
    // not opened; toggling persists the flag and a config reload applies it at
    // once, starting or stopping the device. Reuses the shared switch style.
    const toggleLabel = elem("label", "switch-control device-toggle");
    toggleLabel.title = "Stream this device (applies immediately)";
    const toggleInput = document.createElement("input");
    toggleInput.type = "checkbox";
    toggleInput.className = "visually-hidden";
    toggleInput.checked = configEnabled;
    // role=switch + aria-checked announces "switch, on/off" rather than the bare
    // "checkbox, checked"; keep aria-checked in sync wherever checked changes.
    toggleInput.setAttribute("role", "switch");
    toggleInput.setAttribute("aria-checked", String(configEnabled));
    toggleInput.setAttribute("aria-label", `Stream ${d.name}`);
    const toggleTrack = elem("span", "switch-track");
    toggleTrack.appendChild(elem("span", "switch-thumb"));
    // Visible caption so the bare track is not an unlabeled control, matching the
    // System-view discovery switch. Hidden from assistive tech (the input already
    // carries an aria-label) so it is not announced twice. It sits before the
    // input so the input stays adjacent to the track for the `input + .switch-track`
    // state selectors.
    const toggleCaption = elem("span", "switch-caption", "Stream");
    toggleCaption.setAttribute("aria-hidden", "true");
    toggleLabel.appendChild(toggleCaption);
    toggleLabel.appendChild(toggleInput);
    toggleLabel.appendChild(toggleTrack);
    tags.appendChild(toggleLabel);

    const gearBtn = elem("button", "card-gear");
    gearBtn.setAttribute("type", "button");
    gearBtn.setAttribute("aria-label", "Device settings");
    gearBtn.title = "Device settings";
    gearBtn.innerHTML = ICON_GEAR;
    tags.appendChild(gearBtn);

    header.appendChild(ident);
    header.appendChild(tags);
    article.appendChild(header);

    // Persistent restart-required banner: shown when the device is still serving
    // (or attempting to) while the config now disables it, so a toggle made this
    // session is never silently lost behind a still-live "Serving" card.
    const pendingNote = elem("div", "pending-restart-note");
    pendingNote.setAttribute("role", "status");
    const showPending = pendingStop(configEnabled, d.state);
    pendingNote.textContent = showPending ? PENDING_STOP_TEXT : "";
    pendingNote.hidden = !showPending;
    article.appendChild(pendingNote);

    let urlEl: HTMLElement | null = null;
    let meters: VUMeter[] = [];
    let clientsEl: HTMLElement | null = null;
    let droppedEl: HTMLElement | null = null;
    let footerNote: HTMLElement | null = null;

    if (serving) {
      // Endpoint strip
      const strip = elem("div", "endpoint-strip");
      const info = elem("div", "endpoint-info");
      info.appendChild(elem("span", "endpoint-label", "RTSP URL:"));
      const url = this.rtspUrl(d);
      urlEl = elem("span", "endpoint-url mono", url);
      urlEl.title = url;
      info.appendChild(urlEl);
      const copyBtn = elem("button", "copy-btn");
      copyBtn.setAttribute("type", "button");
      copyBtn.setAttribute("aria-label", "Copy RTSP stream URL");
      copyBtn.title = "Copy RTSP stream URL";
      copyBtn.appendChild(iconSpan(ICON_COPY, "icon-copy"));
      copyBtn.appendChild(elem("span", "copy-label", "Copy URL"));
      copyBtn.addEventListener("click", () => this.handleCopyUrl(copyBtn, urlEl));
      strip.appendChild(info);
      strip.appendChild(copyBtn);
      article.appendChild(strip);

      // Meter console: one live VU meter per capture channel. The meters are
      // decorative real-time visualizations updating ~10 Hz, hidden from the
      // accessibility tree so they do not spam screen readers.
      const channelCount = d.negotiatedChannels ?? d.channels;
      const built = this.buildMeterConsole(channelCount);
      meters = built.meters;
      article.appendChild(built.console);

      // Footer
      const footer = elem("div", "rack-footer");
      const metrics = elem("div", "stream-metrics");
      const clientItem = elem("div", "metric-item");
      clientItem.appendChild(elem("span", undefined, "Clients:"));
      clientsEl = elem("span", "metric-val mono", d.clientConnected ? "1 connected" : "0 connected");
      clientItem.appendChild(clientsEl);
      const dropItem = elem("div", "metric-item");
      dropItem.appendChild(elem("span", undefined, "Dropped Frames:"));
      droppedEl = elem("span", "metric-val mono", String(d.droppedFrames));
      dropItem.appendChild(droppedEl);
      metrics.appendChild(clientItem);
      metrics.appendChild(dropItem);
      footer.appendChild(metrics);
      const negotiated = elem("div");
      const nr = d.negotiatedRate ?? d.rate;
      negotiated.appendChild(elem("span", undefined, `Negotiated: ${nr.toLocaleString("en-US")} Hz`));
      footer.appendChild(negotiated);
      article.appendChild(footer);
    } else {
      // Error / skipped device
      if (d.error) {
        const banner = elem("div", "error-banner");
        banner.appendChild(iconSpan(d.state === "failed" ? ICON_ERROR : ICON_WARN, "error-banner-icon"));
        const body = elem("div", "error-banner-body");
        body.appendChild(elem("span", "error-banner-title", "Device excluded from streaming"));
        body.appendChild(elem("span", "error-banner-desc", d.error));
        banner.appendChild(body);
        article.appendChild(banner);
      }
      const footer = elem("div", "rack-footer");
      footerNote = elem("span", undefined, nonServingFooterText(d.state, configEnabled));
      footer.appendChild(footerNote);
      article.appendChild(footer);
    }

    const settingsWrap = elem("div", "card-settings");
    settingsWrap.hidden = true;
    article.appendChild(settingsWrap);

    const entry: CardEntry = {
      article, gearBtn, serving, meters, urlEl, statusEl, clientsEl, droppedEl,
      toggleInput, pendingNote, footerNote,
      device: d, settingsWrap, settingsForm: null, expanded: false, dirty: false,
    };
    gearBtn.addEventListener("click", () => this.toggleSettings(entry));
    toggleInput.addEventListener("change", () => void this.handleToggleEnabled(entry, toggleInput));
    return entry;
  }

  // buildMeterConsole builds the shared dB scale plus one metering row per
  // capture channel and returns the console element and its VU meters, indexed
  // by channel. A mono device gets a single unlabeled row (unchanged from the
  // single-meter layout); a multi-channel device labels each row "Ch N".
  private buildMeterConsole(count: number): { console: HTMLElement; meters: VUMeter[] } {
    const meterConsole = elem("div", "meter-console");
    const scale = elem("div", "meter-scale");
    for (const s of ["-60", "-48", "-36", "-24", "-18", "-12", "-6", "-3", "0 dBFS"]) {
      scale.appendChild(elem("span", undefined, s));
    }
    meterConsole.appendChild(scale);

    const meters: VUMeter[] = [];
    const n = Math.max(1, count);
    const multi = n > 1;
    // Cap the stack height for high-channel interfaces so a 6-8 channel device
    // does not grow the card tall enough to push the dashboard down; the rows
    // scroll within the console instead. Most appliance devices are mono/stereo.
    if (n > 4) meterConsole.classList.add("many-channels");
    for (let c = 0; c < n; c++) {
      const wrapper = elem("div", "meter-track-wrapper");
      if (multi) {
        const label = elem("span", "meter-channel-label mono", `Ch ${c + 1}`);
        label.setAttribute("aria-hidden", "true");
        wrapper.appendChild(label);
      }
      const canvasContainer = elem("div", "meter-canvas-container");
      const canvas = document.createElement("canvas");
      canvas.className = "meter-canvas";
      // The live meter and its dB readout update ~10 Hz; hide them from assistive
      // tech to avoid announcement spam. The clip button stays exposed.
      canvas.setAttribute("aria-hidden", "true");
      canvas.width = 700;
      canvas.height = 22;
      canvasContainer.appendChild(canvas);
      const stats = elem("div", "meter-stats");
      const dbReadout = elem("span", "db-readout mono", "-inf");
      dbReadout.setAttribute("aria-hidden", "true");
      const clipBtn = elem("button", "clip-latch-btn", "CLIP");
      clipBtn.setAttribute("type", "button");
      clipBtn.setAttribute(
        "aria-label",
        multi ? `Channel ${c + 1} clip indicator, click to clear` : "Clip indicator, click to clear",
      );
      clipBtn.title = "Click to clear clip latch";
      stats.appendChild(dbReadout);
      stats.appendChild(clipBtn);
      wrapper.appendChild(canvasContainer);
      wrapper.appendChild(stats);
      meterConsole.appendChild(wrapper);
      meters.push(new VUMeter(canvas, dbReadout, clipBtn));
    }
    return { console: meterConsole, meters };
  }

  // deviceConfigBase is the current device list to patch from: the persisted
  // config when loaded, else the runtime devices projected to config shape so a
  // patch built before the first config load still carries every device.
  private deviceConfigBase(): DeviceConfig[] {
    return store.getState().config?.devices ?? store.getState().devices.map(deviceToConfig);
  }

  // enqueue serializes config mutations. The queued task runs only after the
  // previous mutation's PATCH and refresh have settled, so it can build its
  // full-array PATCH from a fresh deviceConfigBase() and never clobber a
  // concurrent change with a stale base. The chain tail never rejects (errors are
  // handled inside each task), so one failure cannot wedge later mutations.
  private enqueue(task: () => Promise<void>): Promise<void> {
    const run = this.mutationQueue.then(() => task());
    this.mutationQueue = run.catch(() => {});
    return run;
  }

  // apiErrorToast surfaces a failed PATCH: a validation problem shows the first
  // field/reason, anything else shows the raw message, both under a prefix.
  private apiErrorToast(err: unknown, prefix: string): void {
    if (err instanceof ApiError && err.errors && err.errors.length > 0) {
      const first = err.errors[0];
      showToast(`Rejected: ${first.field ?? "config"} - ${first.reason ?? err.title}`, "error");
    } else {
      const msg = err instanceof Error ? err.message : String(err);
      showToast(`${prefix}: ${msg}`, "error");
    }
  }

  // handleToggleEnabled persists a device's streaming enable/disable flag. The
  // change is hot-applied to the running pipeline (the device is started or
  // stopped in place, other devices keep serving), so it takes effect at once;
  // the toggle reflects the desired state immediately and reverts if the PATCH is
  // rejected.
  private async handleToggleEnabled(entry: CardEntry, input: HTMLInputElement): Promise<void> {
    const want = input.checked;
    const id = entry.device.device;
    input.disabled = true;
    input.setAttribute("aria-checked", String(want));
    await this.enqueue(async () => {
      // Build merged from a FRESH base inside the queued task, after any prior
      // mutation's PATCH+refresh settled, so this full-array PATCH cannot clobber
      // a concurrent change with a stale base.
      const merged = this.deviceConfigBase().map((cd) => (cd.device === id ? { ...cd, enabled: want } : cd));
      try {
        const res = await api.patchConfig({ devices: merged });
        // The PATCH persisted. Seed the cached config with the authoritative
        // response before the refresh, so a later queued mutation rebuilds its
        // base from this change even if the GET refresh below fails (refreshConfig
        // swallows its error, and the 3s poll never refreshes config, so a stale
        // base would otherwise let the next full-array PATCH clobber this one).
        store.applyConfig(res.config);
        // A refresh failure afterwards must NOT revert the toggle: the change is
        // already applied and reflected in the cached config above.
        await Promise.all([store.refreshConfig(), store.refreshDevices()]);
        const verb = want ? "Enabled" : "Disabled";
        showToast(
          res.restartRequired
            ? `${verb} ${entry.device.name}. Restart the appliance to apply.`
            : `${verb} ${entry.device.name}.`,
        );
      } catch (err: unknown) {
        // Only a failed PATCH reverts the toggle: the mutation did not persist.
        input.checked = !want;
        input.setAttribute("aria-checked", String(!want));
        this.apiErrorToast(err, "Toggle failed");
      } finally {
        input.disabled = false;
      }
    });
  }

  private toggleSettings(entry: CardEntry): void {
    if (entry.expanded) {
      void this.requestCloseSettings(entry);
      return;
    }
    if (!entry.settingsForm) {
      entry.settingsWrap.textContent = "";

      const actions = elem("div", "settings-actions");
      const badge = elem("span", "staged-badge", "Unsaved changes");
      badge.hidden = true;
      const spacer = elem("span", "settings-actions-spacer");
      const cancelBtn = elem("button", "btn btn-secondary", "Cancel");
      cancelBtn.setAttribute("type", "button");
      const saveBtn = elem("button", "btn btn-primary", "Save Changes");
      saveBtn.setAttribute("type", "button");
      actions.append(badge, spacer, cancelBtn, saveBtn);

      // Build from the saved config (source of truth), matched by ALSA id.
      const cfg = store.getState().config;
      // Prefer the persisted config (it carries the enabled flag); fall back to
      // the runtime device projected through deviceToConfig so enabled is always
      // present and collect() cannot drop it.
      const configured = cfg?.devices.find((cd) => cd.device === entry.device.device) ?? deviceToConfig(entry.device);
      const form = new DeviceSettingsForm(configured, () => { badge.hidden = false; entry.dirty = true; }, {
        friendlyName: entry.device.friendlyName,
        supportedRates: entry.device.supportedRates,
        supportedChannels: entry.device.supportedChannels,
      });
      entry.settingsForm = form;
      entry.settingsWrap.append(form.element, actions);

      cancelBtn.addEventListener("click", () => void this.requestCloseSettings(entry));
      saveBtn.addEventListener("click", () => this.saveDevice(entry));
    }
    entry.expanded = true;
    entry.settingsWrap.hidden = false;
    entry.article.classList.add("expanded");
  }

  // requestCloseSettings collapses the panel, but first confirms the discard if
  // the form has unsaved edits. It guards every collapse path (the Cancel button
  // and the gear toggle), so a stray click cannot silently drop pending changes.
  private async requestCloseSettings(entry: CardEntry): Promise<void> {
    if (entry.dirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "This device has unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    this.closeSettings(entry);
    // closeSettings clears settingsWrap (including the Cancel button focus was on),
    // so return focus to the gear button, which always survives the collapse,
    // rather than letting focus fall to <body>.
    entry.gearBtn.focus();
  }

  private closeSettings(entry: CardEntry): void {
    entry.expanded = false;
    entry.dirty = false;
    entry.settingsWrap.hidden = true;
    entry.article.classList.remove("expanded");
    entry.settingsWrap.textContent = "";
    entry.settingsForm?.destroy();
    entry.settingsForm = null;
  }

  private async saveDevice(entry: CardEntry): Promise<void> {
    const form = entry.settingsForm;
    if (!form) return;
    if (!form.validate()) {
      showToast("Fix the highlighted fields before saving.", "warn");
      return;
    }
    const edited = form.collect();
    await this.enqueue(async () => {
      // Source the enabled flag and the patch base FRESH inside the queued task:
      // the settings form does not edit enabled, the card toggle may have changed
      // it since the panel opened, and a prior queued mutation may have changed
      // the base. Building here (not at collect time) avoids clobbering either.
      const curEnabled = store.getState().config?.devices.find((cd) => cd.device === edited.device)?.enabled;
      if (curEnabled !== undefined) edited.enabled = curEnabled;
      const merged = this.deviceConfigBase().map((cd) => (cd.device === edited.device ? edited : cd));
      if (!merged.some((cd) => cd.device === edited.device)) merged.push(edited);

      try {
        const res = await api.patchConfig({ devices: merged });
        this.closeSettings(entry);
        // Seed the cached config with the authoritative PATCH response before the
        // refresh so a later queued mutation cannot rebuild from a stale base if
        // the GET refresh fails (see applyConfig). A refresh failure after a
        // successful PATCH must not report "Save failed": the change persisted.
        store.applyConfig(res.config);
        await Promise.all([store.refreshConfig(), store.refreshDevices()]);
        showToast(res.restartRequired ? "Device settings saved. Restart the appliance to apply." : "Device settings applied.");
      } catch (err: unknown) {
        this.apiErrorToast(err, "Save failed");
      }
    });
  }

  private buildStatusBadge(state: string): HTMLElement {
    const badge = deviceStateBadge(state);
    return elem("span", badge.cls, badge.label);
  }

  private updateCard(entry: CardEntry, d: Device, cfgByDevice: Map<string, DeviceConfig>): void {
    entry.device = d;
    if (entry.statusEl) {
      const fresh = this.buildStatusBadge(d.state);
      entry.statusEl.className = fresh.className;
      entry.statusEl.textContent = fresh.textContent;
    }
    if (entry.urlEl) {
      const url = this.rtspUrl(d);
      entry.urlEl.textContent = url;
      entry.urlEl.title = url;
    }
    if (entry.clientsEl) entry.clientsEl.textContent = d.clientConnected ? "1 connected" : "0 connected";
    if (entry.droppedEl) entry.droppedEl.textContent = String(d.droppedFrames);

    // Reconcile the streaming toggle and the pending/footer copy against the
    // persisted config, so an in-session toggle (or an out-of-band config change)
    // is reflected without waiting for a serving-state flip and full rebuild.
    const cfgDev = cfgByDevice.get(d.device);
    const configEnabled = cfgDev?.enabled ?? runtimeEnabled(d.state);
    // Do not fight the user mid-interaction (the input is disabled while a PATCH
    // is in flight); otherwise keep it in sync with the persisted flag.
    if (!entry.toggleInput.disabled && entry.toggleInput.checked !== configEnabled) {
      entry.toggleInput.checked = configEnabled;
      entry.toggleInput.setAttribute("aria-checked", String(configEnabled));
    }
    // Guard the live-region and footer writes so a steady state is not
    // re-written on every ~3s poll (role=status re-announces to screen readers
    // on any content mutation, and the writes are otherwise wasted).
    const showPending = pendingStop(configEnabled, d.state);
    if (showPending === entry.pendingNote.hidden) {
      entry.pendingNote.hidden = !showPending;
      entry.pendingNote.textContent = showPending ? PENDING_STOP_TEXT : "";
    }
    if (entry.footerNote) {
      const footerText = nonServingFooterText(d.state, configEnabled);
      if (entry.footerNote.textContent !== footerText) entry.footerNote.textContent = footerText;
    }
  }

  private handleCopyUrl(btn: HTMLElement, urlEl: HTMLElement | null): void {
    const url = urlEl?.textContent;
    if (!url || !navigator.clipboard) return;
    navigator.clipboard.writeText(url).then(() => {
      btn.classList.add("copied");
      const labelSpan = btn.querySelector<HTMLElement>(".copy-label");
      const orig = labelSpan?.textContent ?? "Copy URL";
      if (labelSpan) labelSpan.textContent = "Copied!";
      window.setTimeout(() => {
        btn.classList.remove("copied");
        if (labelSpan) labelSpan.textContent = orig;
      }, 1600);
    }).catch(() => {
      showToast("Copy failed", "error");
    });
  }

  private updateTelemetryFromStatus(): void {
    if (!this.status) return;
    const uptimeEl = document.getElementById("uptime-display");
    if (uptimeEl) uptimeEl.textContent = formatUptime(this.status.uptimeSeconds, { seconds: true });

    const servingEl = document.getElementById("devices-serving-display");
    if (servingEl) servingEl.textContent = `${this.status.devicesServing} / ${this.status.devicesTotal}`;

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
