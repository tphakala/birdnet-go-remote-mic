import { CustomDropdown } from "./custom-dropdown.js";
import { button, copyText, elem, hideInactiveKey, ICON_COPY, readBoolPref, switchControl, writeBoolPref } from "../lib/ui.js";
import {
  MAX_NAME_LEN,
  MAX_PATH_LEN,
  bitrateFollowsDefault,
  defaultOpusBitrate,
  inputMaxLength,
  lengthError,
} from "../lib/device-settings-core.js";
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

// Hardware facts the backend reports for a device (from GET /devices). All are
// optional: absent when the device id matched no enumerated hardware or could
// not be probed. idStable is false when the id is a card index (which can name a
// different device after a reboot); used only for the read-only identity hint.
export interface DeviceHardware {
  friendlyName?: string;
  supportedRates?: number[];
  supportedChannels?: number[];
  idStable?: boolean;
}

// Per-form counter so element ids are valid and unique regardless of the
// device name (which may contain spaces or other id-invalid characters).
let formSeq = 0;

/**
 * The editable settings controls for one device. Builds its own DOM (into
 * `element`), tracks dirtiness via the onDirty callback, validates field
 * formats, and returns the edited DeviceConfig via collect(). The device id is
 * fixed: it is shown read-only at the top (reachable, with a Copy button) and
 * the caller supplies it back on save. Fields are grouped Capture (what the
 * hardware delivers) then Stream (how it is named, addressed, and encoded).
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
  private bitrateDrop!: CustomDropdown;
  // bitrateFollows is true while the bitrate is the per-channel default, so a
  // change to the channel count moves it along (128 kbps mono, 256 kbps
  // stereo). Picking a bitrate by hand turns it off. settingBitrate marks the
  // form's own select() calls, which fire the same change event as a pick.
  private bitrateFollows = false;
  private settingBitrate = false;
  private modeHidden!: HTMLInputElement;
  private bitrateField!: HTMLElement;
  private quietAlertEl!: HTMLInputElement;
  private rateDrop!: CustomDropdown;
  private device: DeviceConfig;
  private hardware: DeviceHardware;
  private onDirty: () => void;
  // Applied when a display-only preference (hide inactive channels) changes, so
  // the dashboard re-renders the meter at once. Not part of the save/dirty flow.
  private onDisplayChange: () => void;
  private ready = false;
  // loadCoercion is set when opening the form silently downgraded the saved codec
  // because the hardware no longer supports it, so the caller can tell the
  // operator instead of the change appearing to happen on its own.
  private loadCoercion: string | null = null;

  constructor(device: DeviceConfig, onDirty: () => void, hardware: DeviceHardware = {}, onDisplayChange: () => void = () => {}) {
    this.device = device;
    this.hardware = hardware;
    this.onDirty = onDirty;
    this.onDisplayChange = onDisplayChange;
    this.element = elem("div", "device-settings");
    this.build();
    this.ready = true;
  }

  private build(): void {
    const d = this.device;
    const uid = ++formSeq;

    // Read-only device identity, shown first: the persisted id that pins this
    // entry to its hardware, reachable as focusable, selectable text with a Copy
    // button. A title tooltip alone (the old treatment) does not reach keyboard,
    // touch, or screen-reader users, which matters for telling two identical
    // units apart. It is fixed metadata: no change listener, never marks the
    // form dirty, and not part of collect().
    this.buildIdentity(uid);

    const grid = elem("div", "form-grid-2col");

    // Resolve the codec mode the form actually opens on BEFORE building the
    // channel field. modeOptions() may not offer the saved mode (Opus on
    // hardware that cannot do 48 kHz), and pick() then coerces it to PCM.
    // The channel group's caption and accessible name must describe this
    // resolved mode, not the saved one, or they would claim "Opus streams one
    // or two channels" while the mode dropdown is actually showing PCM. Computed
    // once and reused where the mode dropdown is built below.
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
    // one channel is a mono stream, two is stereo (Opus accepts one or two), and
    // three or more is a multi-channel PCM stream. The number of selectable
    // channels is the largest probed channel count, defaulting to stereo when unknown.
    const chField = elem("div", "form-field");
    chField.appendChild(this.label("Channels"));
    const chGroup = this.buildChannelSelect(this.maxChannels(), d.channels);
    this.channelsGroup = chGroup;
    chField.appendChild(chGroup);
    this.chErr = this.error(`set-${uid}-ch-err`);
    chField.appendChild(this.chErr);
    // The hint depends on the codec mode (Opus streams one or two channels, and
    // the Opus clause is dropped on a device that cannot offer Opus). Start it empty:
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
      `DNS-SD instance name and log label. Must be unique. Up to ${MAX_NAME_LEN} characters.`);
    this.nameEl = name.input;
    this.nameErr = name.error;
    this.nameEl.maxLength = inputMaxLength(MAX_NAME_LEN);

    const path = this.field(grid, `set-${uid}-path`, "RTSP Path", d.path, "text",
      `Unique endpoint path on the RTSP server, e.g. /stream. Up to ${MAX_PATH_LEN} characters.`);
    this.pathEl = path.input;
    this.pathErr = path.error;
    this.pathEl.maxLength = inputMaxLength(MAX_PATH_LEN);

    // Mode: Opus is offered only when the device can do 48 kHz (Opus is a 48 kHz
    // codec, mono or stereo). On a device that cannot, only PCM L16 is offered and
    // a saved opus mode is coerced to pcm so the form is never in an unsaveable state.
    const modeField = elem("div", "form-field");
    modeField.appendChild(this.label("Stream Codec Mode"));
    const mode = this.buildDropdown("Stream codec mode", modeOpts, modeInitial);
    this.modeHidden = mode.hidden;
    modeField.appendChild(mode.container);
    const opusOffered = modeOpts.some((o) => o.val === "opus");
    // The saved config asked for Opus but the hardware can no longer satisfy it
    // (no 48 kHz), so the form opened on PCM. Record it so the operator is
    // told rather than seeing the codec change with no explanation.
    if (d.mode === "opus" && !opusOffered) {
      this.loadCoercion = `${d.name} does not support Opus (needs 48 kHz); switched to PCM L16. Save to keep this change.`;
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
        ? "Opus is 48 kHz (mono or stereo); PCM L16 is raw and supports ultrasonic rates."
        : "PCM L16 is raw and supports ultrasonic rates. Opus needs 48 kHz, which this device does not support.",
      `set-${uid}-mode-hint`,
    );
    modeField.appendChild(modeHint);
    this.describe(mode.container, modeHint.id);
    grid.appendChild(modeField);

    // Bitrate
    this.bitrateField = elem("div", "form-field");
    this.bitrateField.appendChild(this.label("Opus Bitrate"));
    // An unset bitrate (absent or 0) is the per-channel default, and so is a
    // saved value that equals it: both keep following the channel count. Opus
    // carries at most two channels, so the default is seeded from at most two
    // even on a device with more capture channels (a PCM-shaped selection).
    const defaultRate = defaultOpusBitrate(Math.min(2, d.channels.length));
    const saved = d.opus?.bitrate || defaultRate;
    this.bitrateFollows = bitrateFollowsDefault(d.opus?.bitrate, d.channels.length);
    const bitrate = this.buildDropdown("Opus bitrate", this.bitrateOptions(saved), this.selectedBitrate(saved));
    this.bitrateHidden = bitrate.hidden;
    this.bitrateDrop = bitrate.dropdown;
    this.bitrateHidden.addEventListener("change", () => {
      if (this.ready && !this.settingBitrate) this.bitrateFollows = false;
    });
    this.bitrateField.appendChild(bitrate.container);
    const bitrateHint = this.hint(
      "Target bitrate for the Opus encoder. Defaults to 128 kbps per channel (128 kbps mono, 256 kbps stereo).",
      `set-${uid}-bitrate-hint`,
    );
    this.bitrateField.appendChild(bitrateHint);
    this.describe(bitrate.container, bitrateHint.id);
    this.bitrateField.hidden = modeInitial !== "opus";
    grid.appendChild(this.bitrateField);

    // Very-quiet alert opt-out. Defaults on (quietAlert absent means true); turn
    // it off for a device expected to be silent for long stretches (a bat mic by
    // day) so it does not raise the very-quiet warning. Stuck-at-zero and
    // clipping are unaffected. Stored via collect() as DeviceConfig.quietAlert.
    const quietField = elem("div", "form-field");
    const quietId = `set-${uid}-quiet`;
    const quietLabel = this.label("Very Quiet Alert");
    quietLabel.setAttribute("for", quietId);
    quietField.appendChild(quietLabel);
    const quietHintId = `${quietId}-hint`;
    const quietSwitch = switchControl({
      id: quietId,
      label: "Warn when this device stays very quiet",
      checked: d.quietAlert ?? true,
      describedBy: quietHintId,
    });
    this.quietAlertEl = quietSwitch.input;
    this.quietAlertEl.addEventListener("change", () => {
      if (this.ready) this.onDirty();
    });
    quietField.appendChild(quietSwitch.el);
    quietField.appendChild(this.hint(
      "On by default. Turn off for a device expected to be silent for long stretches; the no-signal and clipping alerts still apply.",
      quietHintId,
    ));
    grid.appendChild(quietField);

    // Meter display preference (not appliance config): hide the channels no
    // stream carries from the VU meter. On by default. Stored client-side and
    // applied at once via onDisplayChange, so it stays outside collect() and the
    // save/dirty flow. Only meaningful when the device has more than one channel.
    if (this.maxChannels() > 1) {
      const hideField = elem("div", "form-field");
      const hideId = `set-${uid}-hideinactive`;
      const hideLabel = this.label("Hide Inactive Channels");
      hideLabel.setAttribute("for", hideId);
      hideField.appendChild(hideLabel);
      const hideHintId = `${hideId}-hint`;
      const { el: hideSwitch, input: hideInput } = switchControl({
        id: hideId,
        label: "Hide channels no stream carries",
        checked: readBoolPref(hideInactiveKey(this.device.device), true),
        describedBy: hideHintId,
      });
      hideInput.addEventListener("change", () => {
        writeBoolPref(hideInactiveKey(this.device.device), hideInput.checked);
        this.onDisplayChange();
      });
      hideField.appendChild(hideSwitch);
      hideField.appendChild(this.hint(
        "On by default. Shows only the channels the current selection streams; turn off to see every captured channel, dimmed when inactive.",
        hideHintId,
      ));
      grid.appendChild(hideField);
    }

    this.element.appendChild(grid);

    this.modeHidden.addEventListener("change", () => {
      const isOpus = this.modeHidden.value === "opus";
      this.bitrateField.hidden = !isOpus;
      this.applyChannelMode(this.modeHidden.value as StreamMode);
      if (isOpus) {
        this.rateDrop.select("48000");
        // Cap the selection at two channels on entering Opus (Opus takes one or
        // two): keep a valid stereo pair, drop any extra, and fall back to Ch1
        // when nothing is selected so the form is never left in an unsaveable
        // (zero-channel) state.
        const sel = this.selectedChannels();
        this.setChannelSelection(sel.length ? sel.slice(0, 2) : [1]);
        this.syncDefaultBitrate();
      }
      this.validate();
    });
    // Validate on a rate change too, so picking a non-48 kHz rate in Opus mode
    // shows the "switch to PCM" guidance at once rather than on save.
    this.rateHidden.addEventListener("change", () => {
      if (this.ready) this.validate();
    });
  }

  // buildIdentity renders the read-only "Device id" row (a focusable, selectable
  // mono input plus a Copy button) and a hint that explains the id's stability
  // and, for a card-index id, the remedy (remove and re-add to pin by identity).
  // Read-only: it carries no change listener and is never read by collect().
  private buildIdentity(uid: number): void {
    const field = elem("div", "form-field");
    const inputId = `set-${uid}-devid`;
    const label = this.label("Device id");
    label.setAttribute("for", inputId);
    field.appendChild(label);

    const row = elem("div", "auth-token-row");
    const input = document.createElement("input");
    input.type = "text";
    input.className = "field-input mono";
    input.id = inputId;
    input.value = this.device.device;
    input.readOnly = true;
    input.spellcheck = false;
    const hintId = `${inputId}-hint`;
    input.setAttribute("aria-describedby", hintId);
    const copyBtn = button({
      variant: "secondary",
      icon: ICON_COPY,
      label: "Copy",
      ariaLabel: "Copy device id",
      onClick: () => copyText(this.device.device, "Device id copied."),
    });
    row.append(input, copyBtn);
    field.appendChild(row);

    field.appendChild(this.hint(this.identityHint(), hintId));
    this.element.appendChild(field);
  }

  // identityHint explains the persisted id: for a card-index id (idStable false)
  // it warns it can change and gives the remedy; for a stable id it says the id
  // follows the hardware; and when the stability is unknown (idStable absent,
  // e.g. the hardware could not be matched or probed) it says so rather than
  // claiming the id is stable.
  private identityHint(): string {
    if (this.hardware.idStable === false) {
      return "This id is a card index and can change after a reboot or replug. Remove and re-add this device to pin it by its stable identity.";
    }
    if (this.hardware.idStable === true) {
      return "The stable id this device is pinned to. It follows the hardware across reboots and re-plugging into another port.";
    }
    return "The stability of this id is unknown: the hardware could not be matched or probed.";
  }

  // syncDefaultBitrate moves the Opus bitrate to the default for the selected
  // channel count while it is still following the default. It is a no-op outside
  // Opus mode: in PCM the bitrate control is hidden and a >2 channel count would
  // otherwise pick a value the dropdown does not carry, deselecting every option.
  private syncDefaultBitrate(): void {
    if (this.modeHidden.value !== "opus") return;
    if (!this.bitrateFollows) return;
    const want = String(defaultOpusBitrate(this.selectedChannels().length));
    if (this.bitrateHidden.value === want) return;
    this.settingBitrate = true;
    try {
      this.bitrateDrop.select(want);
    } finally {
      this.settingBitrate = false;
    }
  }

  public destroy(): void {
    for (const dropdown of this.dropdowns) dropdown.destroy();
    this.dropdowns = [];
  }

  // loadNotice returns a message when opening the form silently coerced an
  // unsupported saved codec (Opus on hardware that cannot do 48 kHz) down to
  // PCM, or null when nothing was coerced. The caller surfaces it to the operator.
  public loadNotice(): string | null {
    return this.loadCoercion;
  }

  // focusFirstInvalid moves keyboard focus to the first field validate() flagged
  // invalid, so a rejected save lands the user on what needs fixing rather than
  // leaving focus on the Save button. It targets the field's focusable control: a
  // text input, a dropdown trigger, or the first channel checkbox.
  public focusFirstInvalid(): void {
    const field = this.element.querySelector<HTMLElement>(".form-field.invalid");
    if (!field) return;
    const target = field.querySelector<HTMLElement>(
      'input:not([type="hidden"]):not(.visually-hidden), .dropdown-trigger',
    );
    target?.focus();
  }

  public validate(): boolean {
    const mode = this.modeHidden.value as StreamMode;
    const rate = Number(this.rateHidden.value);
    const channels = this.selectedChannels();
    let ok = true;
    const name = this.nameEl.value.trim();
    // The length limits are checked here, in characters, so an over-long value
    // shows beside its field instead of failing the save with a 422.
    const nameLenErr = lengthError(name, this.device.name, MAX_NAME_LEN, "Name");
    ok = this.mark(this.nameEl, this.nameErr, name.length > 0 && !nameLenErr,
      nameLenErr || "Name is required.") && ok;
    const path = this.pathEl.value.trim();
    const pathLenErr = lengthError(path, this.device.path, MAX_PATH_LEN, "Path");
    ok = this.mark(this.pathEl, this.pathErr, path.startsWith("/") && path.length >= 2 && !pathLenErr,
      pathLenErr || "Path must start with / and be at least 2 characters.") && ok;
    let rateOk = rate >= 8000 && rate <= 384000;
    let chOk = channels.length >= 1;
    let chMsg = "Select at least one channel.";
    if (mode === "opus") {
      rateOk = rate === 48000;
      chOk = channels.length >= 1 && channels.length <= 2;
      chMsg = "Opus requires one or two channels.";
    }
    ok = this.markControl(this.rateErr, rateOk,
      mode === "opus"
        ? `Opus runs at 48000 Hz only. To capture at ${rate.toLocaleString("en-US")} Hz, switch Stream Codec Mode to PCM L16, which supports the other rates this device offers.`
        : "Rate must be 8000-384000 Hz.") && ok;
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
      // The very-quiet alert opt-out is written explicitly (true is equivalent
      // to absent), so toggling it off persists quietAlert:false.
      quietAlert: this.quietAlertEl.checked,
    };
    if (mode === "opus") {
      // While the bitrate is still following the channel-count default, persist 0
      // so the server applies its own default (OpusDefaultBitrate) rather than
      // freezing today's number; a value the operator picked is sent as chosen.
      dev.opus = { bitrate: this.bitrateFollows ? 0 : Number(this.bitrateHidden.value) || 0 };
    } else if (this.device.opus) {
      // Preserve a saved Opus bitrate when the mode is not Opus (e.g. it was
      // coerced to PCM because the device cannot do 48 kHz), so a temporary
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
  // Opus mode the group is limited to one or two channels, and that is said
  // plainly; on a device that cannot offer Opus the Opus clause is omitted
  // rather than left dangling.
  private channelHint(mode: StreamMode): string {
    if (mode === "opus") return "Opus streams one or two channels: pick one for mono or two for stereo.";
    if (this.opusSupported()) return "Select which capture channels to stream. One channel is a mono stream; Opus takes one or two.";
    return "Select which capture channels to stream. One channel is a mono stream.";
  }

  // applyChannelMode refreshes the channel group's caption and accessible name
  // for the codec mode, so the one-or-two-channel constraint in Opus mode is
  // announced (the caption says pick one or two, and the group's aria-label
  // names it) instead of being an unexplained checkbox quirk. It is the single
  // writer of both, so buildChannelSelect sets neither.
  private applyChannelMode(mode: StreamMode): void {
    const opus = mode === "opus";
    this.chHint.textContent = this.channelHint(mode);
    // Opus is limited to one or two channels (the change handler caps it). Flag
    // the group so the stylesheet renders it as a constrained "pick one or two"
    // set (pill chips), making the constraint visible rather than an unexplained
    // checkbox quirk.
    this.channelsGroup.classList.toggle("opus-select", opus);
    this.channelsGroup.setAttribute(
      "aria-label",
      opus ? "Capture channels to stream (Opus: one or two channels)" : "Capture channels to stream",
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
  // Opus runs at 48 kHz (RFC 7587), mono or stereo. A mono stream is always
  // achievable by selecting one channel (the appliance extracts channels from
  // whatever contiguous count the device opens), so only 48 kHz capture support
  // gates Opus here. A rate set that was not probed (empty/absent) is treated
  // as supported,
  // the same graceful degradation the rate control uses, so a device that was
  // merely busy at startup is not stripped of Opus.
  private opusSupported(): boolean {
    const rates = this.hardware.supportedRates;
    return !rates?.length || rates.includes(48000);
  }

  // modeOptions is the codec-mode list for this device: PCM L16 always, and Opus
  // only when the device supports 48 kHz. Gating Opus here prevents offering
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
  // mode the change handler caps the selection at two channels (mono or stereo),
  // clearing the lowest-numbered other channel if a further box is checked.
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
          // Opus carries one or two channels: cap the selection at two. When a
          // further box is checked, clear the lowest-numbered of the other checked
          // boxes (channelBoxes is in channel-number order), keeping the just-checked
          // box and the highest-numbered prior selection.
          const checked = this.channelBoxes.filter((b) => b.checked);
          if (checked.length > 2) {
            for (const other of checked) {
              if (other !== box) {
                other.checked = false;
                break;
              }
            }
          }
        } else if (isOpus && !box.checked && this.selectedChannels().length === 0) {
          // ...and it cannot be emptied: unchecking the only channel re-checks it
          // so an Opus device never lands in the "no channel" invalid state.
          box.checked = true;
          return;
        }
        this.syncDefaultBitrate();
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
