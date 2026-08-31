import { CustomDropdown } from "./custom-dropdown.js";
import { elem } from "../lib/ui.js";
import type { DeviceConfig, StreamMode } from "../lib/types.js";

const CHEVRON =
  '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"></path></svg>';
const CHECK =
  '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>';

// Standard ALSA capture rates offered for PCM when the device's own supported
// set is unknown (device unavailable at startup). Opus is always locked to
// 48000. When the backend reports supportedRates for the device, those win.
// Keep in sync with candidateRates in internal/audio/hardware.go.
const STANDARD_RATES = [16000, 22050, 32000, 44100, 48000, 88200, 96000, 176400, 192000, 256000, 384000];

const MIN_BITRATE = 64000;
const BITRATE_OPTIONS: DropdownOption[] = [
  { val: "64000", label: "64 kbps" },
  { val: "96000", label: "96 kbps" },
  { val: "128000", label: "128 kbps" },
  { val: "160000", label: "160 kbps" },
  { val: "192000", label: "192 kbps" },
  { val: "256000", label: "256 kbps" },
  { val: "320000", label: "320 kbps" },
  { val: "448000", label: "448 kbps" },
];

export interface DropdownOption {
  val: string;
  label: string;
  tag?: string;
  tagClass?: string;
}

// Hardware facts the backend reports for a device (from GET /devices). Both are
// optional: absent when the device id matched no enumerated hardware or could
// not be probed.
export interface DeviceHardware {
  friendlyName?: string;
  supportedRates?: number[];
  supportedChannels?: number[];
}

// Per-form counter so element ids are valid and unique regardless of the
// device name (which may contain spaces or other id-invalid characters).
let formSeq = 0;

/**
 * The editable settings controls for one device. Builds its own DOM (into
 * `element`), tracks dirtiness via the onDirty callback, validates field
 * formats, and returns the edited DeviceConfig via collect(). The ALSA device
 * id is fixed and not shown here; the caller supplies it back on save. Fields
 * are grouped Capture (what the hardware delivers) then Stream (how it is named,
 * addressed, and encoded).
 */
export class DeviceSettingsForm {
  readonly element: HTMLElement;
  private dropdowns: CustomDropdown[] = [];
  private nameEl!: HTMLInputElement;
  private nameErr!: HTMLElement;
  private pathEl!: HTMLInputElement;
  private pathErr!: HTMLElement;
  private rateErr!: HTMLElement;
  private chErr!: HTMLElement;
  private rateHidden!: HTMLInputElement;
  private channelsHidden!: HTMLInputElement;
  private bitrateHidden!: HTMLInputElement;
  private modeHidden!: HTMLInputElement;
  private bitrateField!: HTMLElement;
  private rateDrop!: CustomDropdown;
  private channelsDrop!: CustomDropdown;
  private device: DeviceConfig;
  private hardware: DeviceHardware;
  private onDirty: () => void;
  private ready = false;
  // loadCoercion is set when opening the form silently downgraded the saved codec
  // because the hardware no longer supports it, so the caller can tell the
  // operator instead of the change appearing to happen on its own.
  private loadCoercion: string | null = null;

  constructor(device: DeviceConfig, onDirty: () => void, hardware: DeviceHardware = {}) {
    this.device = device;
    this.hardware = hardware;
    this.onDirty = onDirty;
    this.element = elem("div", "device-settings");
    this.build();
    this.ready = true;
  }

  private build(): void {
    const d = this.device;
    const uid = ++formSeq;
    const grid = elem("div", "form-grid-2col");

    // Capture group: what the hardware delivers.
    this.groupTitle(grid, "Capture");

    // Rate
    const rateField = elem("div", "form-field");
    rateField.appendChild(this.label("Sample Rate (Hz)"));
    const rate = this.buildDropdown("Sample rate", this.rateOptions(d.rate), String(d.rate));
    this.rateHidden = rate.hidden;
    this.rateDrop = rate.dropdown;
    rateField.appendChild(rate.container);
    this.rateErr = this.error(`set-${uid}-rate-err`);
    rateField.appendChild(this.rateErr);
    rateField.appendChild(this.hint(this.rateHint()));
    grid.appendChild(rateField);

    // Channels: built from the device's probed channel capability. A
    // single-channel device fixes the control to Mono; an unknown capability
    // (device busy or missing) falls back to offering both.
    const chField = elem("div", "form-field");
    chField.appendChild(this.label("Channels"));
    const chOptions = this.channelOptions();
    const chInitial = this.pick(chOptions, String(d.channels));
    const channels = this.buildDropdown("Channels", chOptions, chInitial);
    this.channelsHidden = channels.hidden;
    this.channelsDrop = channels.dropdown;
    chField.appendChild(channels.container);
    this.chErr = this.error(`set-${uid}-ch-err`);
    chField.appendChild(this.chErr);
    // When the device supports a single channel count the dropdown has one
    // option and is therefore already fixed to it; the hint says which, and it
    // reads the same as the single-option mode dropdown (no special styling, so
    // the two one-option controls stay consistent).
    const chFixed = chOptions.length === 1;
    const fixedIsMono = chOptions[0]?.val === "1";
    chField.appendChild(this.hint(
      chFixed
        ? `This device supports only ${fixedIsMono ? "one channel (mono)" : "two channels (stereo)"}.`
        : "Opus requires mono.",
    ));
    grid.appendChild(chField);

    // Stream group: how the capture is named, addressed, and encoded.
    this.groupTitle(grid, "Stream");

    // Name, defaulting from the sound card's friendly label when blank.
    const initialName = d.name || this.hardware.friendlyName || "";
    const name = this.field(grid, `set-${uid}-name`, "Device Name", initialName, "text",
      "DNS-SD instance name and log label. Must be unique.");
    this.nameEl = name.input;
    this.nameErr = name.error;

    const path = this.field(grid, `set-${uid}-path`, "RTSP Path", d.path, "text",
      "Unique endpoint path on the RTSP server, e.g. /stream.");
    this.pathEl = path.input;
    this.pathErr = path.error;

    // Mode: Opus is offered only when the device can do 48 kHz mono (Opus is a
    // 48 kHz mono codec). On a device that cannot, only PCM L16 is offered and a
    // saved opus mode is coerced to pcm so the form is never in an unsaveable state.
    const modeField = elem("div", "form-field");
    modeField.appendChild(this.label("Stream Codec Mode"));
    const modeOpts = this.modeOptions();
    const modeInitial = this.pick(modeOpts, d.mode);
    const mode = this.buildDropdown("Stream codec mode", modeOpts, modeInitial);
    this.modeHidden = mode.hidden;
    modeField.appendChild(mode.container);
    const opusOffered = modeOpts.some((o) => o.val === "opus");
    // The saved config asked for Opus but the hardware can no longer satisfy it
    // (no 48 kHz mono), so the form opened on PCM. Record it so the operator is
    // told rather than seeing the codec change with no explanation.
    if (d.mode === "opus" && !opusOffered) {
      this.loadCoercion = `${d.name} does not support Opus (48 kHz mono); switched to PCM L16. Save to keep this change.`;
    }
    modeField.appendChild(this.hint(
      opusOffered
        ? "Opus is 48 kHz mono; PCM L16 is raw and supports ultrasonic rates."
        : "PCM L16 is raw and supports ultrasonic rates. Opus needs 48 kHz mono, which this device does not support.",
    ));
    grid.appendChild(modeField);

    // Bitrate
    this.bitrateField = elem("div", "form-field");
    this.bitrateField.appendChild(this.label("Opus Bitrate"));
    const saved = d.opus?.bitrate ?? MIN_BITRATE;
    const bitrate = this.buildDropdown("Opus bitrate", this.bitrateOptions(saved), this.selectedBitrate(saved));
    this.bitrateHidden = bitrate.hidden;
    this.bitrateField.appendChild(bitrate.container);
    this.bitrateField.appendChild(this.hint("Target bitrate for the Opus encoder."));
    this.bitrateField.hidden = modeInitial !== "opus";
    grid.appendChild(this.bitrateField);

    this.element.appendChild(grid);

    this.modeHidden.addEventListener("change", () => {
      const isOpus = this.modeHidden.value === "opus";
      this.bitrateField.hidden = !isOpus;
      if (isOpus) {
        this.rateDrop.select("48000");
        this.channelsDrop.select("1");
      }
      this.validate();
    });
  }

  public destroy(): void {
    for (const dropdown of this.dropdowns) dropdown.destroy();
    this.dropdowns = [];
  }

  // loadNotice returns a message when opening the form silently coerced an
  // unsupported saved codec (Opus on hardware that cannot do 48 kHz mono) down to
  // PCM, or null when nothing was coerced. The caller surfaces it to the operator.
  public loadNotice(): string | null {
    return this.loadCoercion;
  }

  public validate(): boolean {
    const mode = this.modeHidden.value as StreamMode;
    const rate = Number(this.rateHidden.value);
    const channels = Number(this.channelsHidden.value);
    let ok = true;
    ok = this.mark(this.nameEl, this.nameErr, this.nameEl.value.trim().length > 0,
      "Name is required.") && ok;
    const path = this.pathEl.value.trim();
    ok = this.mark(this.pathEl, this.pathErr, path.startsWith("/") && path.length >= 2,
      "Path must start with / and be at least 2 characters.") && ok;
    let rateOk = rate >= 8000 && rate <= 384000;
    let chOk = channels === 1 || channels === 2;
    if (mode === "opus") {
      rateOk = rate === 48000;
      chOk = channels === 1;
    }
    ok = this.markControl(this.rateErr, rateOk,
      mode === "opus" ? "Opus requires 48000 Hz." : "Rate must be 8000-384000 Hz.") && ok;
    ok = this.markControl(this.chErr, chOk, "Opus requires mono (1 channel).") && ok;
    return ok;
  }

  // markControl toggles the invalid state on a dropdown field (which has no text
  // input to carry aria-invalid) and writes the failed rule into its error
  // element, so a codec-constraint failure highlights the actual field.
  private markControl(error: HTMLElement, ok: boolean, message: string): boolean {
    const field = error.closest(".form-field");
    field?.classList.toggle("invalid", !ok);
    error.textContent = ok ? "" : message;
    return ok;
  }

  public collect(): DeviceConfig {
    const mode = this.modeHidden.value as StreamMode;
    const dev: DeviceConfig = {
      name: this.nameEl.value.trim(),
      device: this.device.device,
      path: this.pathEl.value.trim(),
      mode,
      rate: Number(this.rateHidden.value),
      channels: Number(this.channelsHidden.value),
      format: this.device.format || "s16",
      // Preserve the streaming enable/disable flag: this form does not edit it,
      // but saveDevice replaces the whole device entry in the PATCH, so dropping
      // it here would silently re-enable a disabled device on save.
      enabled: this.device.enabled,
    };
    if (mode === "opus") {
      dev.opus = { bitrate: Number(this.bitrateHidden.value) || MIN_BITRATE };
    } else if (this.device.opus) {
      // Preserve a saved Opus bitrate when the mode is not Opus (e.g. it was
      // coerced to PCM because the device cannot do 48 kHz mono), so a temporary
      // mode change does not silently discard the operator's bitrate.
      dev.opus = this.device.opus;
    }
    return dev;
  }

  // groupTitle appends a full-width heading that visually separates the field
  // groups within the two-column grid.
  private groupTitle(grid: HTMLElement, text: string): void {
    grid.appendChild(elem("div", "form-group-title", text));
  }

  private buildDropdown(
    ariaLabel: string, options: DropdownOption[], selected: string
  ): { container: HTMLElement; hidden: HTMLInputElement; dropdown: CustomDropdown } {
    const container = elem("div", "custom-dropdown");
    container.dataset.value = selected;
    const hidden = document.createElement("input");
    hidden.type = "hidden";
    hidden.value = selected;
    container.appendChild(hidden);

    const trigger = elem("div", "dropdown-trigger");
    trigger.tabIndex = 0;
    trigger.setAttribute("role", "combobox");
    trigger.setAttribute("aria-expanded", "false");
    trigger.setAttribute("aria-haspopup", "listbox");
    trigger.setAttribute("aria-label", ariaLabel);
    trigger.appendChild(elem("div", "dropdown-value-group"));
    const chevron = elem("span", "dropdown-chevron");
    chevron.innerHTML = CHEVRON;
    trigger.appendChild(chevron);
    container.appendChild(trigger);

    const menu = elem("div", "dropdown-menu");
    menu.setAttribute("role", "listbox");
    menu.setAttribute("aria-label", ariaLabel);
    for (const opt of options) {
      const item = elem("div", "dropdown-item");
      item.dataset.val = opt.val;
      if (opt.tag) item.dataset.tag = opt.tag;
      if (opt.tagClass) item.dataset.tagClass = opt.tagClass;
      item.dataset.label = opt.label;
      item.setAttribute("role", "option");
      item.setAttribute("aria-selected", String(opt.val === selected));
      if (opt.val === selected) item.classList.add("selected");
      const titleWrap = elem("div", "item-title");
      titleWrap.appendChild(elem("span", undefined, opt.label));
      item.appendChild(titleWrap);
      const check = elem("span", "item-check");
      check.innerHTML = CHECK;
      item.appendChild(check);
      menu.appendChild(item);
    }
    container.appendChild(menu);

    const dropdown = new CustomDropdown(container, () => {
      if (this.ready) this.onDirty();
    });
    dropdown.select(selected);
    this.dropdowns.push(dropdown);
    return { container, hidden, dropdown };
  }

  private field(
    grid: HTMLElement, id: string, labelText: string, value: string, type: string, hint: string
  ): { input: HTMLInputElement; error: HTMLElement } {
    const field = elem("div", "form-field");
    const l = this.label(labelText);
    l.setAttribute("for", id);
    field.appendChild(l);
    const input = document.createElement("input");
    input.type = type;
    input.className = "field-input mono";
    input.id = id;
    input.value = value;
    const errId = `${id}-err`;
    input.setAttribute("aria-describedby", errId);
    input.addEventListener("input", () => {
      if (this.ready) {
        this.validate();
        this.onDirty();
      }
    });
    field.appendChild(input);
    const error = this.error(errId);
    field.appendChild(error);
    field.appendChild(this.hint(hint));
    grid.appendChild(field);
    return { input, error };
  }

  private label(text: string): HTMLElement {
    return elem("label", "field-label", text);
  }

  private hint(text: string): HTMLElement {
    return elem("span", "field-hint", text);
  }

  private error(id: string): HTMLElement {
    const e = elem("span", "field-error");
    e.id = id;
    return e;
  }

  // mark toggles the field's invalid state: the red border/message via the
  // .invalid class on the form-field, the specific rule text in the .field-error
  // element, and aria-invalid on the input so a screen reader announces it.
  private mark(input: HTMLElement, error: HTMLElement, ok: boolean, message: string): boolean {
    const field = input.closest(".form-field");
    field?.classList.toggle("invalid", !ok);
    input.setAttribute("aria-invalid", ok ? "false" : "true");
    error.textContent = ok ? "" : message;
    return ok;
  }

  // opusSupported reports whether the device can carry Opus in this appliance.
  // Opus runs at 48 kHz internally (RFC 7587), and this appliance requires mono
  // for Opus (config.Validate rejects opus with more than one channel), so it
  // needs both 48 kHz and mono capture. A capability that was not probed
  // (empty/absent) is treated as supported, the same graceful degradation the
  // rate control uses, so a device that was merely busy at startup is not
  // stripped of Opus.
  private opusSupported(): boolean {
    const rates = this.hardware.supportedRates;
    const chans = this.hardware.supportedChannels;
    const rate48kOK = !rates?.length || rates.includes(48000);
    const monoOK = !chans?.length || chans.includes(1);
    return rate48kOK && monoOK;
  }

  // modeOptions is the codec-mode list for this device: PCM L16 always, and Opus
  // only when the device supports 48 kHz mono. Gating Opus here prevents offering
  // a mode the hardware cannot satisfy (which would force an unsupported 48 kHz
  // and be rejected at open).
  private modeOptions(): DropdownOption[] {
    const opts: DropdownOption[] = [];
    if (this.opusSupported()) {
      opts.push({ val: "opus", label: "Opus (Compressed, 48 kHz)", tag: "OPUS", tagClass: "highlight" });
    }
    opts.push({ val: "pcm", label: "PCM L16 (Uncompressed Raw)", tag: "PCM L16", tagClass: "ultrasonic" });
    return opts;
  }

  // channelOptions is the Channels list for this device. When the device's
  // supported channel counts are known, the control is constrained to them (a
  // single-channel device is fixed to Mono), and an unsupported saved value is
  // NOT re-added, so a stale stereo value on a mono-only device is steered back
  // to a valid Mono selection on save. When capability is unknown (device busy or
  // missing) it falls back to offering both, matching the rate control. When Opus
  // is offered, mono stays selectable so the mode-change snap to mono lands on a
  // real option.
  private channelOptions(): DropdownOption[] {
    const probed = this.hardware.supportedChannels?.filter((c) => c === 1 || c === 2) ?? [];
    let chans: Set<number>;
    if (probed.length) {
      // The probed set is authoritative here. Mono (1) needs no forced re-add for
      // Opus: opusSupported() can only be true when supportedChannels includes 1,
      // so mono is already present whenever the mode-change snap to mono runs.
      chans = new Set<number>(probed);
    } else {
      chans = new Set<number>([1, 2]);
      const current = this.device.channels;
      if (current === 1 || current === 2) chans.add(current);
    }
    return [...chans].sort((a, b) => a - b).map((c) => ({
      val: String(c), label: c === 1 ? "1 (Mono)" : "2 (Stereo)",
    }));
  }

  // pick returns want if it is one of options, else the first option's value, so
  // a dropdown whose saved value is no longer offered (an Opus mode or a stereo
  // count the device cannot do) lands on a valid selection instead of blank.
  private pick(options: DropdownOption[], want: string): string {
    return options.some((o) => o.val === want) ? want : (options[0]?.val ?? want);
  }

  private rateOptions(current: number): DropdownOption[] {
    const base = this.hardware.supportedRates?.length ? this.hardware.supportedRates : STANDARD_RATES;
    const rates = new Set<number>(base);
    if (current > 0) rates.add(current);
    // Opus locks the rate to 48000, so it must always be selectable even when a
    // device's probed set omits it. Otherwise the mode-change snap to 48000
    // silently no-ops and the form lands in an unsaveable state with nothing
    // highlighted.
    rates.add(48000);
    return [...rates].sort((a, b) => a - b).map((r) => ({ val: String(r), label: `${r.toLocaleString("en-US")} Hz` }));
  }

  private rateHint(): string {
    const base = "Capture rate delivered by the device and streamed as-is. Opus is fixed at 48000.";
    return this.hardware.supportedRates?.length ? base : base + " Showing common rates (device set unavailable).";
  }

  private bitrateOptions(current: number): DropdownOption[] {
    if (current > MIN_BITRATE && !BITRATE_OPTIONS.some((o) => o.val === String(current))) {
      return [...BITRATE_OPTIONS, { val: String(current), label: `${Math.round(current / 1000)} kbps` }]
        .sort((a, b) => Number(a.val) - Number(b.val));
    }
    return BITRATE_OPTIONS;
  }

  private selectedBitrate(current: number | undefined): string {
    return String(Math.max(MIN_BITRATE, current ?? MIN_BITRATE));
  }
}
