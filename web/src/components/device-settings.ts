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

    // Channels
    const chField = elem("div", "form-field");
    chField.appendChild(this.label("Channels"));
    const channels = this.buildDropdown("Channels", [
      { val: "1", label: "1 (Mono)" },
      { val: "2", label: "2 (Stereo)" },
    ], String(d.channels));
    this.channelsHidden = channels.hidden;
    this.channelsDrop = channels.dropdown;
    chField.appendChild(channels.container);
    this.chErr = this.error(`set-${uid}-ch-err`);
    chField.appendChild(this.chErr);
    chField.appendChild(this.hint("Opus requires mono."));
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

    // Mode
    const modeField = elem("div", "form-field");
    modeField.appendChild(this.label("Stream Codec Mode"));
    const mode = this.buildDropdown("Stream codec mode", [
      { val: "opus", label: "Opus (Compressed, 48 kHz)", tag: "OPUS", tagClass: "highlight" },
      { val: "pcm", label: "PCM L16 (Uncompressed Raw)", tag: "PCM L16", tagClass: "ultrasonic" },
    ], d.mode);
    this.modeHidden = mode.hidden;
    modeField.appendChild(mode.container);
    modeField.appendChild(this.hint("Opus is 48 kHz mono; PCM L16 is raw and supports ultrasonic rates."));
    grid.appendChild(modeField);

    // Bitrate
    this.bitrateField = elem("div", "form-field");
    this.bitrateField.appendChild(this.label("Opus Bitrate"));
    const saved = d.opus?.bitrate ?? MIN_BITRATE;
    const bitrate = this.buildDropdown("Opus bitrate", this.bitrateOptions(saved), this.selectedBitrate(saved));
    this.bitrateHidden = bitrate.hidden;
    this.bitrateField.appendChild(bitrate.container);
    this.bitrateField.appendChild(this.hint("Target bitrate for the Opus encoder."));
    this.bitrateField.hidden = d.mode !== "opus";
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
    };
    if (mode === "opus") dev.opus = { bitrate: Number(this.bitrateHidden.value) || MIN_BITRATE };
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
