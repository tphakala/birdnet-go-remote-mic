import { store } from "../lib/store.js";
import { VUMeter } from "../components/vu-meter.js";
import { DeviceSettingsForm } from "../components/device-settings.js";
import { showToast } from "../components/toast.js";
import { api, ApiError } from "../lib/api.js";
import { deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
// Trusted static SVG icon markup (no interpolation of runtime data).
const ICON_MIC = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path></svg>';
const ICON_ULTRA = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12h2"></path><path d="M6 8v8"></path><path d="M10 4v16"></path><path d="M14 6v12"></path><path d="M18 9v6"></path><path d="M22 12h-2"></path></svg>';
const ICON_ERROR = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line></svg>';
const ICON_WARN = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>';
const ICON_COPY = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"></rect><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"></path></svg>';
const ICON_GEAR = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"></path><circle cx="12" cy="12" r="3"></circle></svg>';
// capsSummary renders a short human summary of a device's probed capabilities
// (channel support and top sample rate) for the available-devices list.
function capsSummary(d) {
    const parts = [];
    const ch = d.supportedChannels ?? [];
    if (ch.length) {
        if (ch.includes(1) && ch.includes(2))
            parts.push("mono/stereo");
        else if (ch.includes(2))
            parts.push("stereo");
        else
            parts.push("mono");
    }
    const rates = d.supportedRates ?? [];
    if (rates.length) {
        const maxKhz = Math.max(...rates) / 1000;
        parts.push(`up to ${maxKhz.toLocaleString("en-US")} kHz`);
    }
    return parts.join(" · ");
}
// runtimeEnabled reports whether a device is on at runtime. A disabled device is
// off; anything else (serving, or a failed/skipped device that was configured
// to stream) is on until the operator changes it.
function runtimeEnabled(state) {
    return state !== "disabled";
}
// pendingStop reports that a device is currently serving while the config now
// disables it. A config change is hot-applied, so this divergence is only ever a
// brief moment while the reload stops the device; the banner labels that instant
// so the still-live "Serving" badge and meters are not unexplained. Only a
// serving device qualifies: a failed or skipped device is not serving (its footer
// already explains the exclusion), and the reverse (a disabled card now enabled)
// is explained by the non-serving footer, so neither needs a banner.
function pendingStop(configEnabled, state) {
    return state === "serving" && !configEnabled;
}
const PENDING_STOP_TEXT = "Disabling; this device stops serving shortly.";
// nonServingFooterText is the footer message for a card that is not serving,
// shared by buildCard and updateCard so an in-session toggle never leaves stale
// enabled/disabled wording behind.
function nonServingFooterText(state, configEnabled) {
    if (state === "disabled") {
        return configEnabled
            ? "Enabling; this device starts serving shortly."
            : "Streaming is disabled for this device. Enable it to start serving.";
    }
    return "Excluded from the RTSP stream server. Other active devices continue serving without interruption.";
}
function iconSpan(markup, className) {
    const s = document.createElement("span");
    if (className)
        s.className = className;
    // Static trusted markup only; never runtime/user data.
    s.innerHTML = markup;
    return s;
}
// channelLabel renders the streamed channel selection, e.g. "Ch 1", "Ch 1+2",
// or "Ch 1+3" for a non-contiguous pair. An empty selection renders nothing.
function channelLabel(channels) {
    if (!channels.length)
        return "";
    if (channels.length === 1)
        return `Ch ${channels[0]}`;
    return "Ch " + channels.join("+");
}
// The runtime device carries all configured fields; project it to the config
// shape as a fallback base for the device-list patch.
function deviceToConfig(d) {
    const c = {
        name: d.name, device: d.device, path: d.path, mode: d.mode,
        rate: d.rate, channels: d.channels, format: d.format,
        enabled: runtimeEnabled(d.state),
    };
    if (d.opus)
        c.opus = d.opus;
    return c;
}
function rtspPort(listen) {
    if (!listen)
        return "8554";
    const i = listen.lastIndexOf(":");
    return i >= 0 ? listen.slice(i + 1) : listen;
}
export class DashboardView {
    cards = new Map();
    rack;
    emptyEl;
    availableSection;
    availableRack;
    // Device ids with a provisioning request in flight, so the Enable button shows
    // progress and a second click cannot double-provision.
    provisioning = new Set();
    status = null;
    // Serializes config mutations (device toggle + settings save) so each PATCH is
    // built from a fresh base only after the previous mutation settled. Prevents a
    // full-array PATCH from a stale base from clobbering a concurrent change.
    mutationQueue = Promise.resolve();
    constructor() {
        this.rack = document.getElementById("channel-rack");
        this.emptyEl = document.getElementById("rack-empty");
        this.availableSection = document.getElementById("available-section");
        this.availableRack = document.getElementById("available-rack");
        this.initHeader();
        this.bindEvents();
    }
    initHeader() {
        const host = document.getElementById("appliance-hostname");
        if (host)
            host.textContent = window.location.hostname;
        const addr = document.getElementById("appliance-address");
        if (addr)
            addr.textContent = window.location.host;
    }
    bindEvents() {
        store.addEventListener("devices", (e) => {
            this.renderDevices(e.detail);
        });
        store.addEventListener("status", (e) => {
            this.status = e.detail;
            this.updateTelemetryFromStatus();
            // Recompute RTSP URLs now that the listen port is known.
            this.renderDevices(store.getState().devices);
        });
        store.addEventListener("system", (e) => {
            this.updateTelemetryFromSystem(e.detail);
        });
        store.addEventListener("levels", (e) => {
            const levels = e.detail;
            levels.forEach((dl, name) => {
                const entry = this.cards.get(name);
                if (!entry)
                    return;
                for (const ch of dl.channels) {
                    const meter = entry.meters[ch.channel];
                    if (meter)
                        meter.setLevels(ch.rmsDbfs, ch.peakDbfs, ch.clipped);
                }
            });
        });
        store.addEventListener("available", (e) => {
            this.renderAvailable(e.detail);
        });
        store.addEventListener("connection", (e) => {
            this.updateConnection(e.detail);
        });
        store.addEventListener("loaderror", (e) => {
            const detail = e.detail;
            if (detail.coreFailed)
                this.renderLoadError(detail.message);
        });
    }
    // renderLoadError replaces the "Loading..." placeholder with the failure cause
    // and a Retry button when the initial data fetch fails, so the view is not
    // stuck loading forever. A successful retry re-renders via the devices event.
    renderLoadError(message) {
        if (!this.emptyEl)
            return;
        renderLoadError(this.emptyEl, message, "Loading devices...", () => void store.retry());
    }
    renderDevices(devices) {
        if (!this.rack)
            return;
        if (this.emptyEl) {
            this.emptyEl.hidden = devices.length > 0;
            // Clear the alert live-region role set by renderLoadError so the benign
            // empty/loaded state is not re-announced as an error.
            this.emptyEl.removeAttribute("role");
            // Replace the static "Loading devices..." placeholder once we know there
            // are genuinely zero configured devices (the element is visible here).
            if (devices.length === 0)
                this.emptyEl.textContent = "No capture devices are configured.";
        }
        const seen = new Set();
        let clientCount = 0;
        // Index the persisted config by ALSA id once per pass so the per-card
        // reconcile is a Map lookup rather than a linear scan of the config list per
        // card (an N+1 over the device count).
        const cfgByDevice = new Map();
        for (const cd of store.getState().config?.devices ?? [])
            cfgByDevice.set(cd.device, cd);
        for (const d of devices) {
            seen.add(d.name);
            if (d.clientConnected)
                clientCount++;
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
            }
            else {
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
            if (entry)
                this.rack.appendChild(entry.article);
        }
        const clientsEl = document.getElementById("total-clients-display");
        if (clientsEl)
            clientsEl.textContent = String(clientCount);
    }
    // renderAvailable lists the host's detected-but-unconfigured capture devices,
    // each with an Enable button that provisions it. The whole section hides when
    // nothing is available, so a fully configured host shows no empty panel.
    renderAvailable(available) {
        if (!this.availableRack || !this.availableSection)
            return;
        this.availableSection.hidden = available.length === 0;
        this.availableRack.textContent = "";
        for (const d of available) {
            this.availableRack.appendChild(this.buildAvailableCard(d));
        }
    }
    buildAvailableCard(d) {
        const card = elem("div", "config-device-card available-card");
        const info = elem("div", "available-info");
        info.appendChild(elem("div", "device-title", d.friendlyName || d.device));
        const sub = elem("div", "available-sub");
        sub.appendChild(elem("span", "mono", d.device));
        const caps = capsSummary(d);
        if (caps)
            sub.appendChild(elem("span", "available-caps", caps));
        info.appendChild(sub);
        const enableBtn = elem("button", "btn btn-primary available-enable", "Enable");
        enableBtn.setAttribute("type", "button");
        // Name the device in the accessible label: there is one Enable button per
        // available device, so a bare "Enable" is ambiguous to a screen-reader user.
        enableBtn.setAttribute("aria-label", `Enable ${d.friendlyName || d.device}`);
        if (this.provisioning.has(d.device))
            this.markBusy(enableBtn, "Enabling...");
        enableBtn.addEventListener("click", () => void this.provisionDevice(d, enableBtn));
        card.append(info, enableBtn);
        return card;
    }
    async provisionDevice(d, btn) {
        if (this.provisioning.has(d.device))
            return;
        this.provisioning.add(d.device);
        this.markBusy(btn, "Enabling...");
        try {
            // Serialize through the same queue as toggles and settings saves: those
            // submit a full-array PATCH built from the cached config, so a provision
            // running concurrently could be clobbered by a stale PATCH (or vice versa).
            // The refreshes run inside the task so the next queued mutation rebuilds
            // from the post-provision config.
            await this.enqueue(async () => {
                const created = await api.provisionDevice({ device: d.device });
                await Promise.all([store.refreshDevices(), store.refreshAvailable(), store.refreshConfig()]);
                showToast(`Enabled ${created.name}. Streaming on ${created.path}.`);
            });
        }
        catch (err) {
            this.apiErrorToast(err, "Enable failed");
        }
        finally {
            this.provisioning.delete(d.device);
            this.clearBusy(btn, "Enable");
        }
    }
    // markBusy/clearBusy toggle a control's in-progress affordance without using
    // the disabled property on a focused element, which would steal keyboard focus
    // (a control removed from the tab order sends focus to the body). aria-disabled
    // plus a guard keeps the element focusable and announces the busy state.
    markBusy(el, label) {
        el.textContent = label;
        el.setAttribute("aria-disabled", "true");
        el.setAttribute("aria-busy", "true");
        el.classList.add("is-busy");
    }
    clearBusy(el, label) {
        el.textContent = label;
        el.removeAttribute("aria-disabled");
        el.removeAttribute("aria-busy");
        el.classList.remove("is-busy");
    }
    // removeDevice deletes a configured device after confirmation, returning its
    // hardware to the available list. On success the card disappears via the device
    // refresh; on failure the button is restored so it can be retried.
    async removeDevice(entry, btn) {
        // aria-disabled keeps the button focusable while busy, so guard against a
        // keyboard re-activation that pointer-events cannot block: without this a
        // second Enter opens a second confirm and issues a second DELETE.
        if (btn.getAttribute("aria-disabled") === "true")
            return;
        const ok = await confirmDialog({
            title: "Remove device",
            body: `Remove ${entry.device.name}? It stops streaming and returns to the available list, and its stream path is discarded.`,
            confirmLabel: "Remove",
            danger: true,
        });
        if (!ok)
            return;
        this.markBusy(btn, "Removing...");
        try {
            // Serialize with toggles and settings saves: a stale full-array PATCH from
            // one of those must not run interleaved with this delete and restore the
            // removed device (or drop a concurrently provisioned one).
            await this.enqueue(async () => {
                await api.deleteDevice(entry.device.name);
                // The card (and this button) is about to be destroyed by the refresh,
                // which would drop focus to the document body. Move it to the workspace
                // region first so a keyboard user keeps a sensible place.
                document.getElementById("main-content")?.focus();
                await Promise.all([store.refreshDevices(), store.refreshAvailable(), store.refreshConfig()]);
                showToast(`Removed ${entry.device.name}.`);
            });
        }
        catch (err) {
            this.apiErrorToast(err, "Remove failed");
            this.clearBusy(btn, "Remove");
        }
    }
    rtspUrl(d) {
        const port = rtspPort(this.status?.rtspListen);
        return `rtsp://${window.location.hostname}:${port}${d.path}`;
    }
    buildCard(d, cfgByDevice) {
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
        if (serving && isUltra)
            avatar.style.color = "var(--ultrasonic-purple)";
        if (!serving && !disabled)
            avatar.style.color = "var(--signal-crit)";
        const nameBlock = elem("div", "device-name-block");
        nameBlock.appendChild(elem("span", "device-title", d.name));
        // Surface the sound card's hardware model (friendlyName) next to the ALSA id
        // so a device is identifiable by what it physically is, not only its config
        // name. Omit it when absent (the device id matches no enumerated hardware) or
        // when it only repeats the configured name (e.g. "loopback" / "Loopback").
        const hw = d.friendlyName?.trim();
        const showHw = !!hw && hw.toLowerCase() !== d.name.trim().toLowerCase();
        nameBlock.appendChild(elem("span", "device-path mono", showHw ? `ALSA: ${d.device} · ${hw}` : `ALSA: ${d.device}`));
        ident.appendChild(avatar);
        ident.appendChild(nameBlock);
        const tags = elem("div", "device-tags");
        if (serving) {
            const rate = d.negotiatedRate ?? d.rate;
            const modeTag = elem("span", `tech-tag ${isUltra ? "ultrasonic" : "highlight"}`, modeLabel(d.mode));
            tags.appendChild(modeTag);
            tags.appendChild(elem("span", "tech-tag", `${rate.toLocaleString("en-US")} Hz`));
            tags.appendChild(elem("span", "tech-tag", channelLabel(d.channels)));
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
        let urlEl = null;
        let meters = [];
        let clientsEl = null;
        let droppedEl = null;
        let footerNote = null;
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
            const channelCount = d.negotiatedChannels ?? d.channels.length;
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
        }
        else {
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
        const entry = {
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
    buildMeterConsole(count) {
        const meterConsole = elem("div", "meter-console");
        const scale = elem("div", "meter-scale");
        for (const s of ["-60", "-48", "-36", "-24", "-18", "-12", "-6", "-3", "0 dBFS"]) {
            scale.appendChild(elem("span", undefined, s));
        }
        meterConsole.appendChild(scale);
        const meters = [];
        const n = Math.max(1, count);
        const multi = n > 1;
        // Cap the stack height for high-channel interfaces so a 6-8 channel device
        // does not grow the card tall enough to push the dashboard down; the rows
        // scroll within the console instead. Most appliance devices are mono/stereo.
        if (n > 4)
            meterConsole.classList.add("many-channels");
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
            clipBtn.setAttribute("aria-label", multi ? `Channel ${c + 1} clip indicator, click to clear` : "Clip indicator, click to clear");
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
    deviceConfigBase() {
        return store.getState().config?.devices ?? store.getState().devices.map(deviceToConfig);
    }
    // enqueue serializes config mutations. The queued task runs only after the
    // previous mutation's PATCH and refresh have settled, so it can build its
    // full-array PATCH from a fresh deviceConfigBase() and never clobber a
    // concurrent change with a stale base. The chain tail never rejects (errors are
    // handled inside each task), so one failure cannot wedge later mutations.
    enqueue(task) {
        const run = this.mutationQueue.then(() => task());
        this.mutationQueue = run.catch(() => { });
        return run;
    }
    // apiErrorToast surfaces a failed PATCH: a validation problem shows the first
    // field/reason, anything else shows the raw message, both under a prefix.
    apiErrorToast(err, prefix) {
        if (err instanceof ApiError && err.errors && err.errors.length > 0) {
            const first = err.errors[0];
            showToast(`Rejected: ${first.field ?? "config"} - ${first.reason ?? err.title}`, "error");
        }
        else {
            const msg = err instanceof Error ? err.message : String(err);
            showToast(`${prefix}: ${msg}`, "error");
        }
    }
    // handleToggleEnabled persists a device's streaming enable/disable flag. The
    // change is hot-applied to the running pipeline (the device is started or
    // stopped in place, other devices keep serving), so it takes effect at once;
    // the toggle reflects the desired state immediately and reverts if the PATCH is
    // rejected.
    async handleToggleEnabled(entry, input) {
        const want = input.checked;
        const id = entry.device.device;
        // Remember focus before disabling: re-enabling a disabled control drops focus
        // to the body, dumping a keyboard user at the top of the page.
        const hadFocus = document.activeElement === input;
        input.disabled = true;
        input.setAttribute("aria-busy", "true");
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
                showToast(res.restartRequired
                    ? `${verb} ${entry.device.name}. Restart the appliance to apply.`
                    : `${verb} ${entry.device.name}.`);
            }
            catch (err) {
                // Only a failed PATCH reverts the toggle: the mutation did not persist.
                input.checked = !want;
                input.setAttribute("aria-checked", String(!want));
                this.apiErrorToast(err, "Toggle failed");
            }
            finally {
                input.disabled = false;
                input.removeAttribute("aria-busy");
                // Return focus to the toggle so re-enabling it does not leave a keyboard
                // user stranded on the document body.
                if (hadFocus)
                    input.focus();
            }
        });
    }
    toggleSettings(entry) {
        if (entry.expanded) {
            void this.requestCloseSettings(entry);
            return;
        }
        if (!entry.settingsForm) {
            entry.settingsWrap.textContent = "";
            const actions = elem("div", "settings-actions");
            const removeBtn = elem("button", "btn btn-danger", "Remove");
            removeBtn.setAttribute("type", "button");
            removeBtn.setAttribute("aria-label", `Remove ${entry.device.name}`);
            const badge = elem("span", "staged-badge", "Unsaved changes");
            badge.hidden = true;
            const spacer = elem("span", "settings-actions-spacer");
            const cancelBtn = elem("button", "btn btn-secondary", "Cancel");
            cancelBtn.setAttribute("type", "button");
            const saveBtn = elem("button", "btn btn-primary", "Save Changes");
            saveBtn.setAttribute("type", "button");
            actions.append(removeBtn, badge, spacer, cancelBtn, saveBtn);
            removeBtn.addEventListener("click", () => void this.removeDevice(entry, removeBtn));
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
            // Tell the operator when opening the form silently downgraded an
            // unsupported saved codec, rather than the change appearing unexplained.
            const notice = form.loadNotice();
            if (notice) {
                badge.hidden = false;
                entry.dirty = true;
                showToast(notice, "warn");
            }
            cancelBtn.addEventListener("click", () => void this.requestCloseSettings(entry));
            saveBtn.addEventListener("click", () => this.saveDevice(entry, saveBtn));
        }
        entry.expanded = true;
        entry.settingsWrap.hidden = false;
        entry.article.classList.add("expanded");
    }
    // requestCloseSettings collapses the panel, but first confirms the discard if
    // the form has unsaved edits. It guards every collapse path (the Cancel button
    // and the gear toggle), so a stray click cannot silently drop pending changes.
    async requestCloseSettings(entry) {
        if (entry.dirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "This device has unsaved changes that will be lost.",
                confirmLabel: "Discard",
                danger: true,
            });
            if (!ok)
                return;
        }
        this.closeSettings(entry);
        // closeSettings clears settingsWrap (including the Cancel button focus was on),
        // so return focus to the gear button, which always survives the collapse,
        // rather than letting focus fall to <body>.
        entry.gearBtn.focus();
    }
    closeSettings(entry) {
        entry.expanded = false;
        entry.dirty = false;
        entry.settingsWrap.hidden = true;
        entry.article.classList.remove("expanded");
        entry.settingsWrap.textContent = "";
        entry.settingsForm?.destroy();
        entry.settingsForm = null;
    }
    async saveDevice(entry, btn) {
        if (btn.getAttribute("aria-disabled") === "true")
            return;
        const form = entry.settingsForm;
        if (!form)
            return;
        if (!form.validate()) {
            showToast("Fix the highlighted fields before saving.", "warn");
            return;
        }
        const edited = form.collect();
        this.markBusy(btn, "Saving...");
        try {
            await this.enqueue(async () => {
                // Source the enabled flag and the patch base FRESH inside the queued task:
                // the settings form does not edit enabled, the card toggle may have changed
                // it since the panel opened, and a prior queued mutation may have changed
                // the base. Building here (not at collect time) avoids clobbering either.
                const curEnabled = store.getState().config?.devices.find((cd) => cd.device === edited.device)?.enabled;
                if (curEnabled !== undefined)
                    edited.enabled = curEnabled;
                const merged = this.deviceConfigBase().map((cd) => (cd.device === edited.device ? edited : cd));
                if (!merged.some((cd) => cd.device === edited.device))
                    merged.push(edited);
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
                }
                catch (err) {
                    this.apiErrorToast(err, "Save failed");
                }
            });
        }
        finally {
            // Restore the button whether the save succeeded (its panel is torn down, so
            // this is a harmless no-op on a detached node) or failed (it stays for retry).
            this.clearBusy(btn, "Save Changes");
        }
    }
    buildStatusBadge(state) {
        const badge = deviceStateBadge(state);
        return elem("span", badge.cls, badge.label);
    }
    updateCard(entry, d, cfgByDevice) {
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
        if (entry.clientsEl)
            entry.clientsEl.textContent = d.clientConnected ? "1 connected" : "0 connected";
        if (entry.droppedEl)
            entry.droppedEl.textContent = String(d.droppedFrames);
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
            if (entry.footerNote.textContent !== footerText)
                entry.footerNote.textContent = footerText;
        }
    }
    handleCopyUrl(btn, urlEl) {
        const url = urlEl?.textContent;
        if (!url || !navigator.clipboard)
            return;
        navigator.clipboard.writeText(url).then(() => {
            btn.classList.add("copied");
            const labelSpan = btn.querySelector(".copy-label");
            const orig = labelSpan?.textContent ?? "Copy URL";
            if (labelSpan)
                labelSpan.textContent = "Copied!";
            window.setTimeout(() => {
                btn.classList.remove("copied");
                if (labelSpan)
                    labelSpan.textContent = orig;
            }, 1600);
        }).catch(() => {
            showToast("Copy failed", "error");
        });
    }
    updateTelemetryFromStatus() {
        if (!this.status)
            return;
        const uptimeEl = document.getElementById("uptime-display");
        if (uptimeEl)
            uptimeEl.textContent = formatUptime(this.status.uptimeSeconds, { seconds: true });
        const servingEl = document.getElementById("devices-serving-display");
        if (servingEl)
            servingEl.textContent = `${this.status.devicesServing} / ${this.status.devicesTotal}`;
        const badge = document.getElementById("appliance-status-badge");
        const text = document.getElementById("appliance-status-text");
        if (badge && text) {
            if (this.status.devicesServing > 0) {
                badge.className = "status-badge ok";
                text.textContent = "Streaming";
            }
            else {
                badge.className = "status-badge crit";
                text.textContent = "No Devices";
            }
        }
    }
    updateTelemetryFromSystem(sys) {
        const cpuEl = document.getElementById("cpu-load-display");
        if (cpuEl)
            cpuEl.textContent = sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a";
        const tempEl = document.getElementById("soc-temp-display");
        if (tempEl)
            tempEl.textContent = sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a";
    }
    updateConnection(connected) {
        const indicator = document.getElementById("connection-indicator");
        const text = document.getElementById("connection-text");
        if (text)
            text.textContent = connected ? "Online" : "Reconnecting";
        if (indicator)
            indicator.classList.toggle("offline", !connected);
    }
}
