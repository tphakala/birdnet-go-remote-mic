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
  private chHint!: HTMLElement;
  private channelsGroup!: HTMLElement;
  private rateHidden!: HTMLInputElement;
  // channelBoxes are the per-channel selection checkboxes (Ch1..ChN), in channel
  // order. The stream carries the checked channels; their 1-based numbers are the
  // config's channels array.
  private channelBoxes: HTMLInputElement[] = [];
  private bitrateHidden!: HTMLInputElement;
  private modeHidden!: HTMLInputElement;
  private bitrateField!: HTMLElement;
  private rateDrop!: CustomDropdown;
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

    // Resolve the codec mode the form actually opens on BEFORE building the
    // channel field. modeOptions() may not offer the saved mode (Opus on
    // hardware that cannot do 48 kHz mono), and pick() then coerces it to PCM.
    // The channel group's caption and accessible name must describe this
    // resolved mode, not the saved one, or they would claim "Opus streams one
    // channel" while the mode dropdown is actually showing PCM. Computed once
    // and reused where the mode dropdown is built below.
    const modeOpts = this.modeOptions();
    // pick() returns one of modeOpts' values ("opus" or "pcm"), so it is always
    // a valid StreamMode.
    const modeInitial = this.pick(modeOpts, d.mode) as StreamMode;

    // Capture group: what the hardware delivers.
    this.groupTitle(grid, "Capture");

    // Rate
    const rateField = elem("div", "form-field");
    rateField.appendChild(this.label("Sample Rate (Hz)"));
    const rateOpts = this.rateOptions(d.rate);
    const rate = this.buildDropdown("Sample rate", rateOpts, this.pick(rateOpts, String(d.rate)));
    this.rateHidden = rate.hidden;
    this.rateDrop = rate.dropdown;
    rateField.appendChild(rate.container);
    this.rateErr = this.error(`set-${uid}-rate-err`);
    rateField.appendChild(this.rateErr);
    const rateHint = this.hint(this.rateHint(), `set-${uid}-rate-hint`);
    rateField.appendChild(rateHint);
    this.describe(rate.container, this.rateErr.id, rateHint.id);
    grid.appendChild(rateField);

    // Channels: a per-channel selection built from the device's probed channel
    // capability. The operator picks which capture channels the stream carries;
    // one selected channel is a mono stream (the only kind Opus accepts), two or
    // more is a multi-channel PCM stream. The number of selectable channels is the
    // largest probed channel count, defaulting to stereo when unknown.
    const chField = elem("div", "form-field");
    chField.appendChild(this.label("Channels"));
    const chGroup = this.buildChannelSelect(this.maxChannels(), d.channels);
    this.channelsGroup = chGroup;
    chField.appendChild(chGroup);
    this.chErr = this.error(`set-${uid}-ch-err`);
    chField.appendChild(this.chErr);
    // The hint depends on the codec mode (Opus streams one channel, and the Opus
    // clause is dropped on a device that cannot offer Opus). Start it empty:
    // applyChannelMode is the single writer of both this caption and the group's
    // accessible name, and it is called with the resolved mode just below (and
    // again on every mode change).
    this.chHint = this.hint("", `set-${uid}-ch-hint`);
    chField.appendChild(this.chHint);
    // Associate the validation error and the hint with the checkbox group so a
    // screen reader announces both when focus is inside the group (parity with
    // the text inputs).
    chGroup.setAttribute("aria-describedby", `${this.chErr.id} ${this.chHint.id}`);
    this.applyChannelMode(modeInitial);
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
    const mode = this.buildDropdown("Stream codec mode", modeOpts, modeInitial);
    this.modeHidden = mode.hidden;
    modeField.appendChild(mode.container);
    const opusOffered = modeOpts.some((o) => o.val === "opus");
    // The saved config asked for Opus but the hardware can no longer satisfy it
    // (no 48 kHz mono), so the form opened on PCM. Record it so the operator is
    // told rather than seeing the codec change with no explanation.
    if (d.mode === "opus" && !opusOffered) {
      this.loadCoercion = `${d.name} does not support Opus (48 kHz mono); switched to PCM L16. Save to keep this change.`;
      // Opus pinned the rate to 48000. If the hardware does not offer 48000 for
      // PCM either, move the rate control to a supported value so Save does not
      // persist a rate the device cannot open.
      const rates = this.hardware.supportedRates;
      if (rates && rates.length && !rates.includes(d.rate)) {
        this.rateDrop.select(String(rates[0]));
      }
    }
    const modeHint = this.hint(
      opusOffered
        ? "Opus is 48 kHz mono; PCM L16 is raw and supports ultrasonic rates."
        : "PCM L16 is raw and supports ultrasonic rates. Opus needs 48 kHz mono, which this device does not support.",
      `set-${uid}-mode-hint`,
    );
    modeField.appendChild(modeHint);
    this.describe(mode.container, modeHint.id);
    grid.appendChild(modeField);

    // Bitrate
    this.bitrateField = elem("div", "form-field");
    this.bitrateField.appendChild(this.label("Opus Bitrate"));
    const saved = d.opus?.bitrate ?? MIN_BITRATE;
    const bitrate = this.buildDropdown("Opus bitrate", this.bitrateOptions(saved), this.selectedBitrate(saved));
    this.bitrateHidden = bitrate.hidden;
    this.bitrateField.appendChild(bitrate.container);
    const bitrateHint = this.hint("Target bitrate for the Opus encoder.", `set-${uid}-bitrate-hint`);
    this.bitrateField.appendChild(bitrateHint);
    this.describe(bitrate.container, bitrateHint.id);
    this.bitrateField.hidden = modeInitial !== "opus";
    grid.appendChild(this.bitrateField);

    this.element.appendChild(grid);

    this.modeHidden.addEventListener("change", () => {
      const isOpus = this.modeHidden.value === "opus";
      this.bitrateField.hidden = !isOpus;
      this.applyChannelMode(this.modeHidden.value as StreamMode);
      if (isOpus) {
        this.rateDrop.select("48000");
        // Opus is a single mono channel: collapse the selection to one channel
        // (keeping the lowest already-selected, or Ch1) so the form can never hold
        // an unsaveable multi-channel Opus selection.
        const first = this.selectedChannels()[0] ?? 1;
        this.setChannelSelection([first]);
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
    const channels = this.selectedChannels();
    let ok = true;
    ok = this.mark(this.nameEl, this.nameErr, this.nameEl.value.trim().length > 0,
      "Name is required.") && ok;
    const path = this.pathEl.value.trim();
    ok = this.mark(this.pathEl, this.pathErr, path.startsWith("/") && path.length >= 2,
      "Path must start with / and be at least 2 characters.") && ok;
    let rateOk = rate >= 8000 && rate <= 384000;
    let chOk = channels.length >= 1;
    let chMsg = "Select at least one channel.";
    if (mode === "opus") {
      rateOk = rate === 48000;
      chOk = channels.length === 1;
      chMsg = "Opus requires exactly one channel.";
    }
    ok = this.markControl(this.rateErr, rateOk,
      mode === "opus" ? "Opus requires 48000 Hz." : "Rate must be 8000-384000 Hz.") && ok;
    ok = this.markControl(this.chErr, chOk, chMsg) && ok;
    this.channelsGroup.setAttribute("aria-invalid", String(!chOk));
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
      channels: this.selectedChannels(),
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
    const hintId = `${id}-hint`;
    // Describe the input with both its error and its hint, so a screen reader
    // reads the guidance with the field (parity with the channel group, whose
    // comment claims this).
    input.setAttribute("aria-describedby", `${errId} ${hintId}`);
    input.addEventListener("input", () => {
      if (this.ready) {
        this.validate();
        this.onDirty();
      }
    });
    field.appendChild(input);
    const error = this.error(errId);
    field.appendChild(error);
    field.appendChild(this.hint(hint, hintId));
    grid.appendChild(field);
    return { input, error };
  }

  private label(text: string): HTMLElement {
    return elem("label", "field-label", text);
  }

  private hint(text: string, id?: string): HTMLElement {
    const h = elem("span", "field-hint", text);
    if (id) h.id = id;
    return h;
  }

  // describe points a dropdown's trigger (its focusable, announced element) at
  // the given description ids, so the hint and error text are read out with the
  // control rather than being bare, unassociated captions.
  private describe(container: HTMLElement, ...ids: string[]): void {
    container.querySelector(".dropdown-trigger")?.setAttribute("aria-describedby", ids.join(" "));
  }

  // channelHint is the caption under the channel group for the given mode. In
  // Opus mode the group is single-select, and that is said plainly; on a device
  // that cannot offer Opus the Opus clause is omitted rather than left dangling.
  private channelHint(mode: StreamMode): string {
    if (mode === "opus") return "Opus streams one channel: choosing a channel clears the others.";
    if (this.opusSupported()) return "Select which capture channels to stream. One channel is a mono stream; Opus requires exactly one.";
    return "Select which capture channels to stream. One channel is a mono stream.";
  }

  // applyChannelMode refreshes the channel group's caption and accessible name
  // for the codec mode, so the single-select behaviour in Opus mode is announced
  // (the caption says choosing a channel clears the others, and the group's
  // aria-label names it) instead of being an unexplained checkbox quirk. It is
  // the single writer of both, so buildChannelSelect sets neither.
  private applyChannelMode(mode: StreamMode): void {
    this.chHint.textContent = this.channelHint(mode);
    this.channelsGroup.setAttribute(
      "aria-label",
      mode === "opus" ? "Capture channel to stream (Opus streams one channel)" : "Capture channels to stream",
    );
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
  // Opus runs at 48 kHz mono internally (RFC 7587). Mono is always achievable by
  // selecting a single channel (the appliance extracts one channel from whatever
  // contiguous count the device opens), so only 48 kHz capture support gates Opus
  // here. A rate set that was not probed (empty/absent) is treated as supported,
  // the same graceful degradation the rate control uses, so a device that was
  // merely busy at startup is not stripped of Opus.
  private opusSupported(): boolean {
    const rates = this.hardware.supportedRates;
    return !rates?.length || rates.includes(48000);
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

  // maxChannels is how many channels the per-channel selection control offers
  // (Ch1..ChN). It is the largest probed channel count, capped at the config
  // maximum (8) and floored at stereo when capability is unknown, and never below
  // an already-selected channel so a saved selection is not truncated by a stale
  // or empty probe.
  private maxChannels(): number {
    const probed = this.hardware.supportedChannels;
    const probedMax = probed?.length ? Math.max(...probed) : 0;
    const savedMax = this.device.channels.length ? Math.max(...this.device.channels) : 0;
    return Math.min(8, Math.max(probedMax || 2, savedMax, 1));
  }

  // buildChannelSelect builds the per-channel checkbox group (Ch1..ChN). In Opus
  // mode the group behaves like a radio: checking one channel clears the rest, so
  // an Opus selection is always a single mono channel.
  private buildChannelSelect(maxCh: number, selected: number[]): HTMLElement {
    const want = new Set(selected);
    const group = elem("div", "channel-select");
    group.setAttribute("role", "group");
    // The accessible name is written by applyChannelMode (the single writer, it
    // varies with the codec mode), called right after this in build().
    this.channelBoxes = [];
    for (let ch = 1; ch <= maxCh; ch++) {
      const wrap = elem("label", "channel-checkbox");
      const box = document.createElement("input");
      box.type = "checkbox";
      box.value = String(ch);
      box.checked = want.has(ch);
      box.setAttribute("aria-label", `Channel ${ch}`);
      box.addEventListener("change", () => {
        const isOpus = this.modeHidden?.value === "opus";
        if (isOpus && box.checked) {
          // Opus streams one channel: checking one clears the rest (radio-like).
          for (const other of this.channelBoxes) {
            if (other !== box) other.checked = false;
          }
        } else if (isOpus && !box.checked && this.selectedChannels().length === 0) {
          // ...and it cannot be emptied: unchecking the only channel re-checks it
          // so an Opus device never lands in the "no channel" invalid state.
          box.checked = true;
          return;
        }
        if (this.ready) this.onDirty();
        this.validate();
      });
      wrap.appendChild(box);
      wrap.appendChild(elem("span", undefined, `Ch ${ch}`));
      group.appendChild(wrap);
      this.channelBoxes.push(box);
    }
    return group;
  }

  // selectedChannels returns the checked channel numbers in ascending order.
  private selectedChannels(): number[] {
    return this.channelBoxes
      .filter((b) => b.checked)
      .map((b) => Number(b.value))
      .sort((a, b) => a - b);
  }

  // setChannelSelection checks exactly the given channels and clears the rest.
  private setChannelSelection(chs: number[]): void {
    const want = new Set(chs);
    for (const box of this.channelBoxes) box.checked = want.has(Number(box.value));
  }

  // pick returns want if it is one of options, else the first option's value, so
  // a dropdown whose saved value is no longer offered (an Opus mode or a stereo
  // count the device cannot do) lands on a valid selection instead of blank.
  private pick(options: DropdownOption[], want: string): string {
    return options.some((o) => o.val === want) ? want : (options[0]?.val ?? want);
  }

  private rateOptions(current: number): DropdownOption[] {
    const probed = this.hardware.supportedRates;
    if (probed?.length) {
      // The probed set is authoritative here, exactly as channelOptions treats
      // supportedChannels: constrain the dropdown to rates the device can actually
      // open, and do NOT re-add a saved value the probe rejects (a stale rate is
      // steered back to a valid one by the pick() on the initial selection). 48000
      // needs no forced re-add: the Opus mode-change snaps the rate to 48000, but
      // Opus is only offered when opusSupported() is true, which requires 48000 to
      // be in the probed set already, so the snap target is present whenever it can
      // be reached.
      const rates = new Set<number>(probed);
      return [...rates].sort((a, b) => a - b).map((r) => ({ val: String(r), label: `${r.toLocaleString("en-US")} Hz` }));
    }
    // Capability unknown (device busy or missing): fall back to the common rates and
    // retain the saved value so the control is never empty. STANDARD_RATES already
    // includes 48000, so the Opus snap still lands on a real option here.
    const rates = new Set<number>(STANDARD_RATES);
    if (current > 0) rates.add(current);
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
