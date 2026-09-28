import { showToast } from "../../components/toast.ts";
import type { VUMeter } from "../../components/vu-meter.ts";
import { api, isRefusal } from "../../lib/api.ts";
import { getToken } from "../../lib/auth.ts";
import { avatarLook, type AvatarIcon, bannerIsError, captureFormatLabel, cardShape, channelLabel, controlGoneMessage, downCauseTitle, footerMetrics, hardwareLine, type LevelsTarget, meterCount, nonServingFooterText, PENDING_STOP_TEXT, pendingStop, rtspUrl, runtimeEnabled, tokenHiddenMessage } from "../../lib/dashboard-core.ts";
import { hideInactiveKey, readBoolPref, writeBoolPref } from "../../lib/prefs.ts";
import { store } from "../../lib/store.ts";
import { deviceIdTitle } from "../../lib/text.ts";
import type { Device, DeviceConfig } from "../../lib/types.ts";
import { announce, clearBusy, deviceStateBadge, focusOnOrDropped, h, holdsFocus, ICON_COPY, iconSpan, modeLabel, reportClipboardFailure, setBusy, setHidden, setText, showUnconfirmed, svgIcon, switchControl, writeToClipboard } from "../../lib/ui.ts";
import { apiErrorToast, type ConfigQueue, STALE_BASE_TEXT } from "./config-queue.ts";
import { MeterConsole } from "./meter-console.ts";
import { SettingsPanel } from "./settings-panel.ts";

// Trusted static SVG icon markup (no interpolation of runtime data).
const ICON_MIC =
  svgIcon('<path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path>', 20);
const ICON_ULTRA =
  svgIcon('<path d="M2 12h2"></path><path d="M6 8v8"></path><path d="M10 4v16"></path><path d="M14 6v12"></path><path d="M18 9v6"></path><path d="M22 12h-2"></path>', 20);
const ICON_ERROR =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line>', 20);
const ICON_WARN =
  svgIcon('<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line>', 18);
const ICON_LOCK =
  svgIcon('<rect width="18" height="11" x="3" y="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 10 0v4"></path>', 11);
// Vertical faders (the mixing-desk "sliders" glyph) for the settings toggle:
// the panel adjusts capture and stream parameters, which reads closer to an
// audio console than a generic gear does.
const ICON_SLIDERS =
  svgIcon('<line x1="4" x2="4" y1="21" y2="14"></line><line x1="4" x2="4" y1="10" y2="3"></line><line x1="12" x2="12" y1="21" y2="12"></line><line x1="12" x2="12" y1="8" y2="3"></line><line x1="20" x2="20" y1="21" y2="16"></line><line x1="20" x2="20" y1="12" y2="3"></line><line x1="2" x2="6" y1="14" y2="14"></line><line x1="10" x2="14" y1="8" y2="8"></line><line x1="18" x2="22" y1="16" y2="16"></line>', 13);
const ICON_CHEVRON =
  svgIcon('<path d="m6 9 6 6 6-6"></path>', 12, 2.2);

const AVATAR_ICONS: Record<AvatarIcon, string> = { mic: ICON_MIC, ultra: ICON_ULTRA, error: ICON_ERROR };

// The copy button's resting and success labels, defined once so a copy
// restores the resting values by identity rather than re-reading the (possibly
// mid-swap) DOM: a rapid second click must not capture "Copied!" as the value to
// restore and leave the visible label or accessible name stuck on it.
const COPY_LABEL = "Copy URL";
const COPY_LABEL_DONE = "Copied!";
const COPY_ARIA = "Copy RTSP stream URL";
const COPY_ARIA_DONE = "RTSP stream URL copied";
const TOKEN_LABEL = "Token";
const TOKEN_ARIA = "Copy access token";
// Pending label-restore timer per copy button, so a second click clears the
// prior restore instead of letting two timers fight (WeakMap: entries GC with
// the button, no leak).
const copyResetTimers = new WeakMap<HTMLElement, number>();

// flashCopied plays the copy-success feedback on btn: the green "copied" pulse,
// the visible label swapped to "Copied!" and the accessible name to ariaDone
// (so the success is announced too), all restored after a moment. It restores
// to the fixed resting values rather than re-reading the DOM, and clears any
// pending restore first, so a rapid second click cannot strand the button on
// "Copied!".
function flashCopied(btn: HTMLElement, label: HTMLElement | null, rest: string, ariaRest: string, ariaDone: string): void {
  btn.classList.add("copied");
  if (label) label.textContent = COPY_LABEL_DONE;
  btn.setAttribute("aria-label", ariaDone);
  const prev = copyResetTimers.get(btn);
  if (prev !== undefined) window.clearTimeout(prev);
  copyResetTimers.set(btn, window.setTimeout(() => {
    btn.classList.remove("copied");
    if (label) label.textContent = rest;
    btn.setAttribute("aria-label", ariaRest);
    copyResetTimers.delete(btn);
  }, 1600));
}

// OVERRUNS_DESCRIPTION explains the capture-overrun counter, which the label
// alone cannot for an operator who has not met the term.
const OVERRUNS_DESCRIPTION =
  "Times capture fell behind the sound card, or the system suspended, and lost audio; " +
  "usually a busy host or unstable USB. Counted since the device opened.";

// metricItem builds one footer metric: its label and the value element sync
// fills. A description is read after the value by a screen reader from a
// visually hidden span, and shown to mouse users as a title on the visible
// label; a title alone reaches neither touch nor assistive-tech users. That
// label is hidden from assistive tech and a visually hidden copy speaks for it,
// so a screen reader that also announces titles does not read the description
// twice.
function metricItem(label: string, description?: string): { item: HTMLElement; value: HTMLElement } {
  const value = h("span", { class: "metric-val mono" });
  const item = description
    ? h("div", { class: "metric-item" },
        h("span", { title: description, "aria-hidden": "true" }, label),
        h("span", { class: "visually-hidden" }, label),
        value,
        h("span", { class: "visually-hidden" }, ` (${description})`),
      )
    : h("div", { class: "metric-item" }, h("span", label), value);
  return { item, value };
}

// Sequence for the token tag's description element ids (aria-describedby targets).
let tokenDescSeq = 0;

// LiveBody holds the nodes of a serving card's body (endpoint strip, meter
// console, metrics footer). IdleBody holds the nodes of a non-serving card's
// body (error banner, footer note). Exactly one is present on a card, chosen by
// its shape; sync writes into whichever exists.
interface LiveBody {
  urlEl: HTMLElement;
  clientsEl: HTMLElement;
  droppedEl: HTMLElement;
  overrunsEl: HTMLElement;
  negotiatedEl: HTMLElement;
  meterConsole: MeterConsole;
}

interface IdleBody {
  banner: HTMLElement;
  bannerIcon: HTMLElement;
  bannerTitle: HTMLElement;
  bannerDesc: HTMLElement;
  footerNote: HTMLElement;
}

// ArticleParts are the nodes build produces for a device's current shape: the
// <article> plus every header and body node sync writes into, assigned in one
// checked step so the compiler sees that build populates all of them.
interface ArticleParts {
  article: HTMLElement;
  // Header nodes, shape-independent (chips and lock are hidden on idle cards
  // rather than absent, so the header never has to be rebuilt on a shape flip):
  avatar: HTMLElement;
  titleEl: HTMLElement;
  hwEl: HTMLElement;
  modeTag: HTMLElement;
  rateTag: HTMLElement;
  chTag: HTMLElement;
  lockEl: HTMLElement;
  statusEl: HTMLElement;
  toggleInput: HTMLInputElement;
  settingsBtn: HTMLElement;
  pendingNote: HTMLElement;
  // Body: exactly one is present, matching the shape.
  live: LiveBody | null;
  idle: IdleBody | null;
}

// DeviceCardHost is what a device card needs from the Dashboard.
export interface DeviceCardHost {
  queue: ConfigQueue;
  announceEl: HTMLElement | null;
  // render asks for a reconcile of every card from the store.
  render(): void;
}

// DeviceCard is one configured device on the Dashboard, the stable identity
// the view keeps per device id. The <article> and its body are a disposable
// render swapped only when the card's shape changes (serving with a given
// channel count, versus idle); what must survive a rebuild (the settings panel
// and its form, a pending toggle, the hide-inactive preference) belongs to
// the card, not the article. Every node that shows DEVICE OR CONFIG data is
// written by exactly one method, sync; transient interaction state (the
// toggle's in-flight checked/busy and the copy button's "Copied!" feedback) is
// the deliberate exception. build creates the skeleton with no device data, so
// a data field sync forgets renders blank at development time instead of going
// silently stale in production.
export class DeviceCard {
  private readonly host: DeviceCardHost;
  private readonly panel: SettingsPanel;
  private p: ArticleParts;
  // The latest runtime record, set by sync; device.device is the id.
  private dev: Device;
  // The state an enable/disable PATCH asked for while it is queued or in
  // flight, else null. It is on the card rather than the toggle node, so a
  // card rebuilt meanwhile gets a busy toggle showing it that sync does not
  // reset.
  private pendingWant: boolean | null = null;
  // The per-device "hide inactive channels" display preference. Read from
  // storage once when the card is created, rather than on every poll; the
  // settings switch and a change saved in another tab (the storage event)
  // update it. The dashboard is the key's one owner: it alone writes it. When
  // the browser cannot persist it, the choice holds until the device leaves
  // the list.
  private hideInactive: boolean;

  constructor(d: Device, host: DeviceCardHost) {
    this.host = host;
    this.dev = d;
    this.hideInactive = readBoolPref(hideInactiveKey(d.device), true);
    this.panel = new SettingsPanel({
      queue: host.queue,
      announceEl: host.announceEl,
      device: () => this.dev,
      hideInactive: () => this.hideInactive,
      setHideInactive: (hide) => {
        this.hideInactive = hide;
        writeBoolPref(hideInactiveKey(this.dev.device), hide);
        host.render();
      },
      // The .expanded class flips the settings button's chevron; the button
      // says the same to assistive tech.
      expandedChanged: () => {
        this.p.article.classList.toggle("expanded", this.panel.expanded);
        this.p.settingsBtn.setAttribute("aria-expanded", String(this.panel.expanded));
      },
      settingsButton: () => this.p.settingsBtn,
    });
    this.p = this.build(d);
  }

  public get el(): HTMLElement {
    return this.p.article;
  }

  public get device(): Device {
    return this.dev;
  }

  // settingsButton is the card's settings disclosure, a stable place to put
  // focus on the card.
  public get settingsButton(): HTMLElement {
    return this.p.settingsBtn;
  }

  // removing is set while this card's own Remove is deleting the device.
  public get removing(): boolean {
    return this.panel.removing;
  }

  // levels is where a levels event goes: the serving card's meters, by the
  // device name the event is keyed by, or null for an idle card.
  public levels(): LevelsTarget<VUMeter> | null {
    return this.p.live ? { name: this.dev.name, meters: this.p.live.meterConsole.meters } : null;
  }

  // clearLevels puts the meters in their waiting state.
  public clearLevels(): void {
    this.p.live?.meterConsole.clear();
  }

  // update shows a devices read: it rebuilds the article when the card's shape
  // changed, writes every field, and checks an open settings form against cfg,
  // the device's entry in the current config (undefined before it loads).
  public update(d: Device, cfg: DeviceConfig | undefined): void {
    if (cardShape(d) !== cardShape(this.dev)) this.mount(d);
    this.sync(d, cfg);
    if (this.panel.expanded) this.panel.sync(cfg);
  }

  // setHideInactive applies a "hide inactive channels" change saved in
  // another tab, and reports whether it changed anything.
  public setHideInactive(hide: boolean): boolean {
    if (this.hideInactive === hide) return false;
    this.hideInactive = hide;
    this.panel.setHideInactive(hide);
    return true;
  }

  // destroy takes the card off the page and its meters off the frame loop.
  public destroy(): void {
    this.p.live?.meterConsole.destroy();
    this.panel.destroy();
    this.p.article.remove();
  }

  // mount rebuilds the article for a device's new shape (serving <-> idle, or
  // a change in the captured channel count), moving the settings panel into
  // the new article and preserving keyboard focus across the swap.
  private mount(d: Device): void {
    const saved = this.captureFocus();
    const oldArticle = this.p.article;
    // Take the old serving body's meters off the frame loop before the article
    // is discarded.
    this.p.live?.meterConsole.destroy();
    this.p = this.build(d);
    // Swap in place if the card was already mounted in the rack; otherwise the
    // view's ordering pass inserts it.
    if (oldArticle.parentNode) oldArticle.replaceWith(this.p.article);
    this.restoreFocus(saved);
  }

  // build creates the DOM skeleton for a device's shape with NO device data
  // written: header nodes with empty text, chips hidden, the toggle unchecked
  // (or showing a pending change, see pendingWant), the body for the shape, and
  // the settings panel moved in. sync fills every value. Trusted static SVG for the copy, lock and settings icons
  // is assigned here; the avatar icon depends on state and is written by sync.
  private build(d: Device): ArticleParts {
    const serving = d.state === "serving";
    // Decorative: the avatar icon repeats the state shown by the status badge.
    const avatar = h("span", { class: "device-avatar", "aria-hidden": "true" });
    const titleEl = h("span", { class: "device-title" });
    const hwEl = h("span", { class: "device-path mono" });
    const modeTag = h("span", { class: "tech-tag" });
    const rateTag = h("span", { class: "tech-tag" });
    const chTag = h("span", { class: "tech-tag" });
    // "Token" marks a stream that needs the access token, and copies it: the
    // same feedback as Copy URL. Its aria-label names the action; the title
    // adds why the token matters for a pointer user, and a described-by span
    // says so to a screen-reader user, whom a title alone does not reliably
    // reach. It is built with the SAME predicate sync uses (serving and auth
    // required) rather than always hidden: mount runs restoreFocus BEFORE the
    // following sync, so a keyboard user who was on the Token tag before a
    // rebuild lands back on it when it should be visible, and still falls back
    // cleanly to the settings button when it should not.
    const tokenDescId = `token-desc-${++tokenDescSeq}`;
    const lockEl = h("button", {
      class: "tech-tag lock-tag",
      type: "button",
      "aria-label": TOKEN_ARIA,
      title: "Pulling this stream requires the access token. Click to copy it.",
      "data-focus": "token",
      hidden: !(serving && !!store.getState().status?.authRequired),
      "aria-describedby": tokenDescId,
    },
      iconSpan(ICON_LOCK, "icon-copy"),
      h("span", { class: "copy-label" }, TOKEN_LABEL),
      h("span", { class: "visually-hidden", id: tokenDescId }, "Pulling this stream requires the access token"),
    );
    lockEl.addEventListener("click", () => this.copyToken(lockEl));
    const statusEl = h("span");

    // Streaming enable/disable toggle. A disabled device stays configured but is
    // not opened; toggling persists the flag and a config reload applies it at
    // once, starting or stopping the device. Reuses the shared switch style.
    // The visible "Stream" caption keeps the bare track from reading as an
    // unlabeled control; it is hidden from assistive tech because the input is
    // named by an aria-label (sync keeps it current across a rename), and it
    // would otherwise be announced twice. The native checkbox exposes its own
    // checked state, so no aria-checked is written.
    const { el: toggleLabel, input: toggleInput } = switchControl({
      ariaLabel: `Stream ${d.name}`,
      caption: "Stream",
      extraClass: "device-toggle",
      title: "Stream this device (applies immediately)",
    });
    toggleInput.dataset.focus = "toggle";
    toggleInput.addEventListener("change", () => void this.toggleEnabled());
    if (this.pendingWant !== null) {
      // Show the change asked for, not the unchecked default of a new switch.
      toggleInput.checked = this.pendingWant;
      setBusy(toggleInput);
    }

    // Settings disclosure. It sits at the right end of the footer, directly
    // above the panel it expands, in both body shapes (a disabled or failed
    // device still needs its settings). expandedChanged keeps its expanded
    // state current.
    const settingsBtn = h("button", {
      class: "card-settings-toggle",
      type: "button",
      "data-focus": "settings",
      "aria-expanded": String(this.panel.expanded),
      "aria-controls": this.panel.el.id,
    },
      iconSpan(ICON_SLIDERS, "settings-toggle-icon"),
      h("span", "Settings"),
      iconSpan(ICON_CHEVRON, "settings-toggle-chevron"),
    );
    settingsBtn.addEventListener("click", () => this.panel.toggle());

    // Persistent note shown while the device is still serving though the
    // config now disables it, so a toggle made this session is never silently
    // lost behind a still-live "Serving" card.
    const pendingNote = h("div", { class: "pending-restart-note", role: "status", hidden: true });

    const article = h("article", { class: this.panel.expanded ? "rack-card expanded" : "rack-card" },
      h("div", { class: "rack-header" },
        h("div", { class: "device-ident" }, avatar, h("div", { class: "device-name-block" }, titleEl, hwEl)),
        h("div", { class: "device-tags" }, modeTag, rateTag, chTag, lockEl, statusEl, toggleLabel),
      ),
      pendingNote,
    );

    let live: LiveBody | null = null;
    let idle: IdleBody | null = null;
    if (serving) {
      const urlEl = h("span", { class: "endpoint-url mono" });
      const copyBtn = h("button", { class: "copy-btn", type: "button", "aria-label": COPY_ARIA, title: COPY_ARIA, "data-focus": "copy" },
        iconSpan(ICON_COPY, "icon-copy"),
        h("span", { class: "copy-label" }, COPY_LABEL),
      );
      copyBtn.addEventListener("click", () => this.copyUrl(copyBtn, urlEl));
      const meterConsole = new MeterConsole(meterCount(d), settingsBtn, this.host.announceEl);
      const clients = metricItem("Clients:");
      const dropped = metricItem("Dropped Frames:");
      const overruns = metricItem("Capture Overruns:", OVERRUNS_DESCRIPTION);
      const negotiatedEl = h("span");
      article.append(
        h("div", { class: "endpoint-strip" },
          h("div", { class: "endpoint-info" }, h("span", { class: "endpoint-label" }, "RTSP URL:"), urlEl),
          copyBtn,
        ),
        meterConsole.el,
        h("div", { class: "rack-footer" },
          h("div", { class: "stream-metrics" }, clients.item, dropped.item, overruns.item),
          // The negotiated rate and the settings toggle share the right end.
          h("div", { class: "rack-footer-end" }, h("div", negotiatedEl), settingsBtn),
        ),
      );
      live = { urlEl, clientsEl: clients.value, droppedEl: dropped.value, overrunsEl: overruns.value, negotiatedEl, meterConsole };
    } else {
      // Error / skipped / disabled body. The banner is always present and
      // hidden by sync when the device has no error, so an error whose text
      // changes while the card stays idle is still reflected in place. sync
      // titles it by the device's down cause.
      const bannerIcon = iconSpan(ICON_WARN, "error-banner-icon");
      const bannerTitle = h("span", { class: "error-banner-title" }, downCauseTitle(undefined));
      const bannerDesc = h("span", { class: "error-banner-desc" });
      const banner = h("div", { class: "error-banner", hidden: true },
        bannerIcon,
        h("div", { class: "error-banner-body" }, bannerTitle, bannerDesc),
      );
      const footerNote = h("span");
      article.append(banner, h("div", { class: "rack-footer" }, footerNote, settingsBtn));
      idle = { banner, bannerIcon, bannerTitle, bannerDesc, footerNote };
    }
    // The settings panel (and its live form, if open) moves into each new
    // article.
    article.append(this.panel.el);

    return {
      article, avatar, titleEl, hwEl, modeTag, rateTag, chTag, lockEl,
      statusEl, toggleInput, settingsBtn, pendingNote, live, idle,
    };
  }

  // sync is the ONE write path for device and config data. It runs right
  // after every build and on every render, writing each dynamic field through a
  // diffed helper (setText / setHidden / classList.toggle) so a steady state
  // does not dirty the DOM or re-announce a live region. A field that exists in
  // the DOM but is not written here renders blank, making an omission a visible
  // defect rather than a silent staleness bug.
  private sync(d: Device, cfg: DeviceConfig | undefined): void {
    this.dev = d;
    const p = this.p;
    const serving = d.state === "serving";
    const disabled = d.state === "disabled";
    const isUltra = d.mode === "pcm";
    const status = store.getState().status;
    // The toggle reflects the persisted (desired) enabled flag, which can differ
    // from the runtime state only briefly while a config reload applies. Fall
    // back to the runtime state only when the config is not loaded.
    const configEnabled = cfg?.enabled ?? runtimeEnabled(d.state);

    // A disabled device is off by intent, not broken: neutral styling; a
    // failed/skipped device gets the error styling.
    p.article.classList.toggle("active-stream", serving);
    p.article.classList.toggle("error-stream", !serving && !disabled);

    // Rewrite the avatar's markup only when its icon changes (keyed by
    // data-icon), so the SVG is not reparsed every poll.
    const look = avatarLook(d.state, d.mode);
    if (p.avatar.dataset.icon !== look.icon) {
      p.avatar.dataset.icon = look.icon;
      p.avatar.innerHTML = AVATAR_ICONS[look.icon]; // static, trusted markup
    }
    if (p.avatar.style.color !== look.color) p.avatar.style.color = look.color;

    setText(p.titleEl, d.name);
    // The persisted id is long, so it goes in the tooltip rather than the line.
    setText(p.hwEl, hardwareLine(d));
    const hwTitle = deviceIdTitle(d.device);
    if (p.hwEl.title !== hwTitle) p.hwEl.title = hwTitle;

    // Chips describe the live stream and are shown only while serving.
    const rate = d.negotiatedRate ?? d.rate;
    setText(p.modeTag, modeLabel(d.mode));
    p.modeTag.classList.toggle("ultrasonic", isUltra);
    p.modeTag.classList.toggle("highlight", !isUltra);
    setHidden(p.modeTag, !serving);
    setText(p.rateTag, `${rate.toLocaleString("en-US")} Hz`);
    setHidden(p.rateTag, !serving);
    // Show every streamed channel (the union across the device's streams), so the
    // header channel tag agrees with the per-channel tally lights below rather
    // than showing only the first stream's channels (d.channels).
    const chLabel = channelLabel(d.streamedChannels ?? d.channels);
    setText(p.chTag, chLabel);
    setHidden(p.chTag, !serving || !chLabel);
    // The Token tag hides in place when access control is turned off elsewhere
    // (the card shape does not change, so mount's focus capture never runs);
    // if it held focus, keep focus on the card and say why it moved.
    const hideLock = !serving || !status?.authRequired;
    const lockHadFocus = hideLock && !p.lockEl.hidden && holdsFocus(p.lockEl);
    setHidden(p.lockEl, hideLock);
    if (lockHadFocus) {
      p.settingsBtn.focus();
      announce(this.host.announceEl, tokenHiddenMessage(d.name));
    }

    const badge = deviceStateBadge(d.state);
    if (p.statusEl.className !== badge.cls) p.statusEl.className = badge.cls;
    setText(p.statusEl, badge.label);

    const toggleAria = `Stream ${d.name}`;
    if (p.toggleInput.getAttribute("aria-label") !== toggleAria) p.toggleInput.setAttribute("aria-label", toggleAria);
    // The settings disclosure reads "Settings" for every card; name the device so
    // a screen-reader user can tell which card's settings the button opens.
    const settingsAria = `Settings for ${d.name}`;
    if (p.settingsBtn.getAttribute("aria-label") !== settingsAria) p.settingsBtn.setAttribute("aria-label", settingsAria);
    // Do not fight the user mid-interaction (a PATCH is queued or in flight);
    // otherwise keep it in sync with the persisted flag.
    if (this.pendingWant === null && p.toggleInput.checked !== configEnabled) {
      p.toggleInput.checked = configEnabled;
    }

    const showPending = pendingStop(configEnabled, d.state);
    setText(p.pendingNote, showPending ? PENDING_STOP_TEXT : "");
    setHidden(p.pendingNote, !showPending);

    if (p.live) {
      const url = rtspUrl(window.location.hostname, status?.rtspListen, d.path);
      setText(p.live.urlEl, url);
      if (p.live.urlEl.title !== url) p.live.urlEl.title = url;
      const counters = footerMetrics(d);
      setText(p.live.clientsEl, counters.clients);
      setText(p.live.droppedEl, counters.dropped);
      setText(p.live.overrunsEl, counters.overruns);
      const negFormat = d.negotiatedFormat ? ` · ${captureFormatLabel(d.negotiatedFormat)}` : "";
      setText(p.live.negotiatedEl, `Negotiated: ${rate.toLocaleString("en-US")} Hz${negFormat}`);
      // The label is the hardware capture format; the RTSP stream is always 16-bit,
      // so name that in a tooltip rather than let "· 24-bit" read as the stream depth.
      const negTitle = d.negotiatedFormat
        ? "Hardware capture format. The RTSP stream is 16-bit; a wider capture is downconverted."
        : "";
      if (p.live.negotiatedEl.title !== negTitle) p.live.negotiatedEl.title = negTitle;
      // The tally lights follow every streamed channel, as the channel tag does.
      p.live.meterConsole.sync(d.streamedChannels ?? d.channels, this.hideInactive);
    }
    if (p.idle) {
      setHidden(p.idle.banner, !d.error);
      const isError = bannerIsError(d.state, d.downCause);
      const bannerKey = isError ? "error" : "warn";
      if (p.idle.bannerIcon.dataset.icon !== bannerKey) {
        p.idle.bannerIcon.dataset.icon = bannerKey;
        p.idle.bannerIcon.innerHTML = isError ? ICON_ERROR : ICON_WARN; // static, trusted markup
      }
      setText(p.idle.bannerTitle, downCauseTitle(d.downCause));
      setText(p.idle.bannerDesc, d.error ?? "");
      setText(p.idle.footerNote, nonServingFooterText(d.state, configEnabled));
    }
  }

  // captureFocus records where keyboard focus is inside the card before its
  // article is rebuilt, so restoreFocus can put it back. Focus inside the
  // settings panel is remembered by element identity (the panel is moved, not
  // rebuilt); focus on a rebuilt control is remembered by its data-focus key
  // (toggle, settings, copy, token, clip-N), which the new article recreates.
  private captureFocus(): { el?: HTMLElement; key?: string } | null {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || !this.p.article.contains(active)) return null;
    if (this.panel.el.contains(active)) return { el: active };
    const key = active.dataset.focus;
    return key ? { key } : null;
  }

  // restoreFocus puts focus back after mount's rebuild. It runs before sync
  // updates the device record, so whether the device serves comes from the
  // rebuilt body.
  private restoreFocus(saved: { el?: HTMLElement; key?: string } | null): void {
    const serving = this.p.live !== null;
    if (!saved) return;
    if (saved.el) {
      // The panel node was moved into the new article and is connected again.
      if (saved.el.isConnected) saved.el.focus();
      return;
    }
    if (saved.key) {
      const node = this.p.article.querySelector<HTMLElement>(`[data-focus="${saved.key}"]`);
      // A control that is gone or hidden in the rebuilt shape (copy and clip-N
      // after a flip to idle, or the token tag when the stream no longer needs
      // the token) cannot take focus; keep focus on the card via the settings
      // button rather than letting it fall to <body>, and say why it moved.
      if (node && !node.hidden && !node.closest("[hidden]")) {
        node.focus();
      } else {
        this.p.settingsBtn.focus();
        // The token message only when the tag went because the token is no
        // longer needed; a device that stopped serving loses it too, and there
        // the token is still required.
        const tokenDropped = saved.key === "token" && serving && !store.getState().status?.authRequired;
        const name = this.dev.name;
        announce(this.host.announceEl, tokenDropped ? tokenHiddenMessage(name) : controlGoneMessage(name, saved.key, serving));
      }
    }
  }

  // toggleEnabled persists the device's streaming enable/disable flag. The
  // change is hot-applied to the running pipeline (the device is started or
  // stopped in place, other devices keep serving), so it takes effect at once;
  // the toggle reflects the desired state immediately and reverts if the PATCH
  // is rejected.
  private async toggleEnabled(): Promise<void> {
    const input = this.p.toggleInput;
    // A busy switch stays enabled (aria-disabled) so it keeps keyboard focus, so
    // it still takes a click: undo it, the pending change decides the value.
    if (this.pendingWant !== null) {
      input.checked = this.pendingWant;
      return;
    }
    const want = input.checked;
    const id = this.dev.device;
    const name = this.dev.name;
    const queue = this.host.queue;
    if (!queue.requireConfig()) {
      input.checked = !want;
      return;
    }
    const hadFocus = document.activeElement === input;
    this.pendingWant = want;
    // aria-disabled, not disabled: a disabled control cannot hold focus, so a
    // card rebuilt while the change is pending could not hand it back.
    setBusy(input);
    await queue.enqueue(async () => {
      // Build merged from a FRESH base inside the queued task, after any prior
      // mutation's PATCH+refresh settled, so this full-array PATCH cannot clobber
      // a concurrent change with a stale base.
      let merged: DeviceConfig[] = [];
      const verb = want ? "Enabled" : "Disabled";
      try {
        if (!(await queue.freshBase())) {
          this.p.toggleInput.checked = !want;
          showToast(STALE_BASE_TEXT, "warn");
          return;
        }
        merged = queue.base().map((cd) => (cd.device === id ? { ...cd, enabled: want } : cd));
        const res = await api.patchConfig({ devices: merged });
        // The PATCH persisted: seed the cached config before the refresh (see
        // ConfigQueue.applied).
        queue.applied(res.config);
        // A refresh failure afterwards must NOT revert the toggle: the change is
        // already applied and reflected in the cached config above.
        await Promise.all([store.refreshConfig(), store.refreshDevices()]);
        showToast(
          res.restartRequired
            ? `${verb} ${name}. Restart the appliance to apply.`
            : `${verb} ${name}.`,
        );
      } catch (err: unknown) {
        if (isRefusal(err)) {
          // A refusal did not persist, so the toggle reverts (the live node:
          // a poll may have rebuilt the card meanwhile).
          this.p.toggleInput.checked = !want;
          apiErrorToast(err, `Could not ${want ? "enable" : "disable"} ${name}`, merged);
        } else {
          // The change may have persisted: re-read inside the queue, so the
          // next queued change builds from what the appliance holds, and say
          // what the re-read found.
          const read = await queue.refreshConfigViews();
          const now = queue.configFor(id);
          if (!read) showUnconfirmed(`the change to ${name}`, "check the switch before trying again");
          else if ((now?.enabled ?? true) === want) showToast(`${verb} ${name}.`);
          else showToast(`The change to ${name} does not appear to have applied; check again shortly.`, "warn");
        }
      } finally {
        // Re-read the current toggle: a poll may have rebuilt the card during the
        // PATCH (a serving<->idle flip, or a captured-channel change) and replaced
        // the node this closure captured. Clear the busy state on the live node and
        // keep focus there, falling back to the settings button if the toggle is
        // gone, so a keyboard user is never stranded on the document body.
        this.pendingWant = null;
        // A card rebuilt while the change was pending skipped the sync, so
        // render once more to set its toggle from the config.
        this.host.render();
        const toggle = this.p.toggleInput;
        clearBusy(toggle);
        // Only if focus is still on the toggle or dropped: the operator may
        // have moved on while the change was queued.
        if (hadFocus && focusOnOrDropped(toggle)) (toggle.isConnected ? toggle : this.p.settingsBtn).focus();
      }
    });
  }

  // copyUrl copies the stream URL. When the appliance requires the access
  // token and this browser holds it, the copied URL embeds it as RTSP
  // credentials (rtsp://mic:<token>@host:port/path) so it pastes straight into
  // BirdNET-Go, ffmpeg or VLC; the displayed URL stays credential-free. The
  // credentialed form is derived from the displayed URL (which sync keeps
  // current), not from a path captured when the card was built, so a device
  // path edit is reflected in the copied URL.
  private copyUrl(btn: HTMLElement, urlEl: HTMLElement): void {
    const shown = urlEl.textContent;
    if (!shown) return;
    let url = shown;
    const token = store.getState().status?.authRequired ? getToken() : null;
    if (token) {
      // Anchor the scheme to the start so only the leading rtsp:// is rewritten,
      // never a literal "rtsp://" that appears later in the path.
      url = shown.replace(/^rtsp:\/\//, `rtsp://mic:${token}@`);
    }
    // Route through the shared clipboard primitive so a plain-http origin (no
    // Clipboard API) reports the same "unavailable" toast as every other Copy
    // button instead of silently doing nothing.
    void writeToClipboard(url).then((result) => {
      if (result !== "ok") {
        reportClipboardFailure(result);
        return;
      }
      if (token) showToast("Stream URL copied with the access token included.");
      // With the token included the toast already announces the copy, so the
      // accessible name stays put rather than announcing it twice.
      flashCopied(btn, btn.querySelector<HTMLElement>(".copy-label"), COPY_LABEL, COPY_ARIA, token ? COPY_ARIA : COPY_ARIA_DONE);
    });
  }

  // copyToken copies the access token this browser signed in with. The tag
  // only shows while the appliance requires a token, so a signed-in browser
  // holds one; without it (or without a clipboard, as on a plain http origin)
  // the operator is told rather than getting a silent no-op.
  private copyToken(btn: HTMLElement): void {
    const token = getToken();
    if (!token) {
      showToast("This browser does not hold the access token. Run remote-mic token get on the appliance.", "warn");
      return;
    }
    // Route through the shared clipboard primitive so an unavailable or failed
    // copy reports the same toast as every other Copy button.
    void writeToClipboard(token).then((result) => {
      if (result !== "ok") {
        reportClipboardFailure(result);
        return;
      }
      showToast("Access token copied.");
      // The toast announces the copy, so the accessible name stays fixed rather
      // than announcing it twice (mirrors the credentialed Copy URL path).
      flashCopied(btn, btn.querySelector<HTMLElement>(".copy-label"), TOKEN_LABEL, TOKEN_ARIA, TOKEN_ARIA);
    });
  }
}
