import { store } from "../lib/store.js";
import { VUMeter } from "../components/vu-meter.js";
import { DeviceSettingsForm } from "../components/device-settings.js";
import { showToast } from "../components/toast.js";
import { api, ApiError } from "../lib/api.js";
import { deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError, setHidden, setText } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
import { getToken } from "../lib/auth.js";
// Trusted static SVG icon markup (no interpolation of runtime data).
const ICON_MIC = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path></svg>';
const ICON_ULTRA = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12h2"></path><path d="M6 8v8"></path><path d="M10 4v16"></path><path d="M14 6v12"></path><path d="M18 9v6"></path><path d="M22 12h-2"></path></svg>';
const ICON_ERROR = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line></svg>';
const ICON_WARN = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>';
const ICON_COPY = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"></rect><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"></path></svg>';
const ICON_LOCK = '<svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="11" x="3" y="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 10 0v4"></path></svg>';
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
// nonServingFooterText is the footer message for a card that is not serving.
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
// meterCount is the number of VU meter rows a serving device shows: one per
// CAPTURED hardware channel (the negotiated count), not per streamed channel.
function meterCount(d) {
    return Math.max(1, d.negotiatedChannels ?? d.channels.length);
}
// shapeKey names the only genuinely structural facts about a card: whether it
// has a serving body (endpoint strip + meter console + metrics) or an idle body
// (error banner + footer), and how many meter rows the serving body has. A
// change to either forces a rebuild (mount); every other field is a syncCard
// write on the existing article. Mode is deliberately excluded: the avatar and
// chips are synced, not rebuilt.
function shapeKey(d) {
    return d.state === "serving" ? `serving:${meterCount(d)}` : "idle";
}
// deviceConfigKey serialises the settings-relevant fields of a device config in
// a fixed order so an out-of-band change to an open form can be detected by
// comparison. It deliberately EXCLUDES enabled: the settings form does not edit
// the enable flag (the card toggle does, and saveDevice sources it fresh), so a
// same-tab toggle must not flag the operator's own open form as changed
// elsewhere.
function deviceConfigKey(cd) {
    if (!cd)
        return "";
    return JSON.stringify([
        cd.name, cd.path, cd.mode, cd.rate,
        // Guard the spread: the runtime devices payload is normalized to always
        // carry a channels array (see the store), but a config payload is not, so a
        // missing or null channels field must not throw here and crash the pass.
        [...(cd.channels ?? [])], cd.format, cd.opus?.bitrate ?? null,
    ]);
}
export class DashboardView {
    // Cards keyed by immutable ALSA device id (the stable identity). byName maps
    // device name to entry for the levels stream, whose payload is keyed by name.
    cards = new Map();
    byName = new Map();
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
        // The view is a function of store state: devices, status and config each
        // trigger a full render() that reads store.getState(), rather than each
        // patching its own subset of the DOM. status is stored first because URLs
        // and the lock tag depend on it; config now arrives on every poll (see the
        // store) so an out-of-band change reflects within one interval.
        store.addEventListener("devices", () => this.render());
        store.addEventListener("config", () => this.render());
        store.addEventListener("status", (e) => {
            this.status = e.detail;
            this.updateTelemetryFromStatus();
            this.render();
        });
        store.addEventListener("system", (e) => {
            this.updateTelemetryFromSystem(e.detail);
        });
        store.addEventListener("levels", (e) => {
            const levels = e.detail;
            levels.forEach((dl, name) => {
                const entry = this.byName.get(name);
                if (!entry?.live)
                    return;
                for (const ch of dl.channels) {
                    const meter = entry.live.meters[ch.channel];
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
    // render is the single reconcile pass, driven by store state. For each runtime
    // device it gets or creates the stable entry, rebuilds the article only when
    // the shape changed, then syncs every field through the one write path. It
    // then removes gone cards, orders the rack with a diff (no DOM move in steady
    // state, which is what keeps keyboard focus from being dropped every poll),
    // rebuilds the name index for the levels stream, and reconciles open forms.
    render() {
        if (!this.rack)
            return;
        const devices = store.getState().devices;
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
        // Index the persisted config by ALSA id once per pass so the per-card
        // reconcile is a Map lookup rather than a linear scan of the config list.
        const cfgByDevice = new Map();
        for (const cd of store.getState().config?.devices ?? [])
            cfgByDevice.set(cd.device, cd);
        const seen = new Set();
        let clientCount = 0;
        for (const d of devices) {
            seen.add(d.device);
            if (d.clientConnected)
                clientCount++;
            let entry = this.cards.get(d.device);
            if (!entry) {
                entry = this.newEntry(d);
                this.cards.set(d.device, entry);
            }
            else if (entry.shape !== shapeKey(d)) {
                this.mount(entry, d);
            }
            this.syncCard(entry, d, cfgByDevice);
        }
        // Remove cards for devices that are gone.
        for (const [id, entry] of this.cards) {
            if (!seen.has(id)) {
                entry.live?.meters.forEach((m) => m.destroy());
                entry.settingsForm?.destroy();
                entry.article.remove();
                this.cards.delete(id);
            }
        }
        // Order the rack to match the device list with a diff: only move a node when
        // it is not already at its target position. Steady state performs no DOM
        // moves, so focus inside a card is never dropped by re-inserting its node.
        devices.forEach((d, i) => {
            const entry = this.cards.get(d.device);
            if (!entry || !this.rack)
                return;
            const current = this.rack.children[i];
            if (current !== entry.article)
                this.rack.insertBefore(entry.article, current ?? null);
        });
        // Rebuild the name index for the levels stream (cards are keyed by id, the
        // levels payload by name; a rename changes the name but not the id).
        this.byName.clear();
        for (const entry of this.cards.values())
            this.byName.set(entry.device.name, entry);
        // Reconcile any open settings form against the (possibly refreshed) config.
        for (const entry of this.cards.values()) {
            if (entry.expanded)
                this.syncSettings(entry, cfgByDevice);
        }
        const clientsEl = document.getElementById("total-clients-display");
        if (clientsEl)
            setText(clientsEl, String(clientCount));
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
    // newEntry creates the stable identity for a device: the settings panel node
    // (which outlives article rebuilds) and then a first mounted article.
    newEntry(d) {
        const settingsWrap = elem("div", "card-settings");
        settingsWrap.hidden = true;
        // Partial until mount fills the article and its named nodes; mount is called
        // immediately below, before the entry escapes to a caller.
        const entry = {
            id: d.device,
            device: d,
            settingsWrap,
            settingsForm: null,
            formSource: null,
            staleNote: null,
            expanded: false,
            dirty: false,
        };
        this.mount(entry, d);
        return entry;
    }
    // mount builds (or rebuilds) the article for a device's current shape, moving
    // the owned settings panel into the new article and preserving keyboard focus
    // across the swap. It is called once at creation and again whenever shapeKey
    // changes (serving <-> idle, or a change in the captured channel count).
    mount(entry, d) {
        const saved = this.captureFocus(entry);
        const oldArticle = entry.article;
        // The old serving body's meters own canvas rAF loops; stop them before the
        // article is discarded.
        entry.live?.meters.forEach((m) => m.destroy());
        this.buildArticle(entry, d);
        // Move the owned settings panel (and its live form, if open) into the new
        // article, and restore its expanded visual state.
        entry.article.appendChild(entry.settingsWrap);
        if (entry.expanded) {
            entry.settingsWrap.hidden = false;
            entry.article.classList.add("expanded");
        }
        entry.shape = shapeKey(d);
        // Swap in place if the card was already mounted in the rack; otherwise the
        // ordering pass in render() inserts it.
        if (oldArticle?.parentNode)
            oldArticle.replaceWith(entry.article);
        this.restoreFocus(entry, saved);
    }
    // buildArticle creates the DOM skeleton for a device's shape with NO device
    // data written: header nodes with empty text, chips and lock hidden, toggle
    // unchecked, the body for the shape. syncCard fills every value immediately
    // after. Handlers for the toggle and gear close over the stable entry, so they
    // survive later rebuilds. Trusted static SVG for the copy, lock and gear icons
    // is assigned here; the avatar icon depends on state and is written by syncCard.
    buildArticle(entry, d) {
        const serving = d.state === "serving";
        const article = elem("article", "rack-card");
        // Header
        const header = elem("div", "rack-header");
        const ident = elem("div", "device-ident");
        const avatar = elem("span", "device-avatar");
        const nameBlock = elem("div", "device-name-block");
        const titleEl = elem("span", "device-title");
        const hwEl = elem("span", "device-path mono");
        nameBlock.appendChild(titleEl);
        nameBlock.appendChild(hwEl);
        ident.appendChild(avatar);
        ident.appendChild(nameBlock);
        const tags = elem("div", "device-tags");
        const modeTag = elem("span", "tech-tag");
        const rateTag = elem("span", "tech-tag");
        const chTag = elem("span", "tech-tag");
        const lockEl = elem("span", "tech-tag lock-tag");
        lockEl.appendChild(iconSpan(ICON_LOCK));
        lockEl.appendChild(elem("span", undefined, "Token"));
        lockEl.title = "Pulling this stream requires the access token";
        const statusEl = elem("span");
        tags.append(modeTag, rateTag, chTag, lockEl, statusEl);
        // Streaming enable/disable toggle. A disabled device stays configured but is
        // not opened; toggling persists the flag and a config reload applies it at
        // once, starting or stopping the device. Reuses the shared switch style.
        const toggleLabel = elem("label", "switch-control device-toggle");
        toggleLabel.title = "Stream this device (applies immediately)";
        const toggleInput = document.createElement("input");
        toggleInput.type = "checkbox";
        toggleInput.className = "visually-hidden";
        // role=switch + aria-checked announces "switch, on/off" rather than the bare
        // "checkbox, checked"; syncCard keeps aria-checked in sync with checked.
        toggleInput.setAttribute("role", "switch");
        toggleInput.dataset.focus = "toggle";
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
        gearBtn.dataset.focus = "gear";
        gearBtn.innerHTML = ICON_GEAR;
        tags.appendChild(gearBtn);
        header.appendChild(ident);
        header.appendChild(tags);
        article.appendChild(header);
        // Persistent restart-required banner: shown when the device is still serving
        // while the config now disables it, so a toggle made this session is never
        // silently lost behind a still-live "Serving" card.
        const pendingNote = elem("div", "pending-restart-note");
        pendingNote.setAttribute("role", "status");
        pendingNote.hidden = true;
        article.appendChild(pendingNote);
        let live = null;
        let idle = null;
        if (serving) {
            // Endpoint strip
            const strip = elem("div", "endpoint-strip");
            const info = elem("div", "endpoint-info");
            info.appendChild(elem("span", "endpoint-label", "RTSP URL:"));
            const urlEl = elem("span", "endpoint-url mono");
            info.appendChild(urlEl);
            const copyBtn = elem("button", "copy-btn");
            copyBtn.setAttribute("type", "button");
            copyBtn.setAttribute("aria-label", "Copy RTSP stream URL");
            copyBtn.title = "Copy RTSP stream URL";
            copyBtn.dataset.focus = "copy";
            copyBtn.appendChild(iconSpan(ICON_COPY, "icon-copy"));
            copyBtn.appendChild(elem("span", "copy-label", "Copy URL"));
            copyBtn.addEventListener("click", () => this.handleCopyUrl(copyBtn, urlEl));
            strip.appendChild(info);
            strip.appendChild(copyBtn);
            article.appendChild(strip);
            // Meter console: one live VU meter per CAPTURED hardware channel. The level
            // meter is registered on the raw capture source with the negotiated channel
            // count and emits a zero-based index per hardware channel (the levels
            // handler indexes live.meters by ch.channel), so the rows are the device's
            // captured channels, not the streamed selection. A non-contiguous selection
            // still shows every captured channel here. The meters are decorative
            // real-time visualizations updating ~10 Hz, hidden from the accessibility
            // tree so they do not spam screen readers.
            const built = this.buildMeterConsole(meterCount(d));
            article.appendChild(built.console);
            // Footer
            const footer = elem("div", "rack-footer");
            const metrics = elem("div", "stream-metrics");
            const clientItem = elem("div", "metric-item");
            clientItem.appendChild(elem("span", undefined, "Clients:"));
            const clientsEl = elem("span", "metric-val mono");
            clientItem.appendChild(clientsEl);
            const dropItem = elem("div", "metric-item");
            dropItem.appendChild(elem("span", undefined, "Dropped Frames:"));
            const droppedEl = elem("span", "metric-val mono");
            dropItem.appendChild(droppedEl);
            metrics.appendChild(clientItem);
            metrics.appendChild(dropItem);
            footer.appendChild(metrics);
            const negotiated = elem("div");
            const negotiatedEl = elem("span");
            negotiated.appendChild(negotiatedEl);
            footer.appendChild(negotiated);
            article.appendChild(footer);
            live = { urlEl, clientsEl, droppedEl, negotiatedEl, meters: built.meters };
        }
        else {
            // Error / skipped / disabled body. The banner is always present and hidden
            // by syncCard when the device has no error, so an error whose text changes
            // while the card stays idle is still reflected in place.
            const banner = elem("div", "error-banner");
            const bannerIcon = iconSpan(ICON_WARN, "error-banner-icon");
            const body = elem("div", "error-banner-body");
            body.appendChild(elem("span", "error-banner-title", "Device excluded from streaming"));
            const bannerDesc = elem("span", "error-banner-desc");
            body.appendChild(bannerDesc);
            banner.appendChild(bannerIcon);
            banner.appendChild(body);
            banner.hidden = true;
            article.appendChild(banner);
            const footer = elem("div", "rack-footer");
            const footerNote = elem("span");
            footer.appendChild(footerNote);
            article.appendChild(footer);
            idle = { banner, bannerIcon, bannerDesc, footerNote };
        }
        entry.article = article;
        entry.avatar = avatar;
        entry.titleEl = titleEl;
        entry.hwEl = hwEl;
        entry.modeTag = modeTag;
        entry.rateTag = rateTag;
        entry.chTag = chTag;
        entry.lockEl = lockEl;
        entry.statusEl = statusEl;
        entry.toggleInput = toggleInput;
        entry.gearBtn = gearBtn;
        entry.pendingNote = pendingNote;
        entry.live = live;
        entry.idle = idle;
        gearBtn.addEventListener("click", () => this.toggleSettings(entry));
        toggleInput.addEventListener("change", () => void this.handleToggleEnabled(entry));
    }
    // buildMeterConsole builds the shared dB scale plus one metering row per
    // captured hardware channel and returns the console element and its VU meters,
    // indexed by zero-based hardware channel position (the levels event's
    // ch.channel indexes straight into this array). A mono device gets a single
    // unlabeled row; a multi-channel device labels each row with its 1-based
    // hardware channel number ("Ch 2" for the second captured channel).
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
            const chNum = c + 1;
            if (multi) {
                const label = elem("span", "meter-channel-label mono", `Ch ${chNum}`);
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
            clipBtn.setAttribute("aria-label", multi ? `Channel ${chNum} clip indicator, click to clear` : "Clip indicator, click to clear");
            clipBtn.title = "Click to clear clip latch";
            // Focus key so a rebuild that moves focus can restore it to the same row.
            clipBtn.dataset.focus = `clip-${c}`;
            stats.appendChild(dbReadout);
            stats.appendChild(clipBtn);
            wrapper.appendChild(canvasContainer);
            wrapper.appendChild(stats);
            meterConsole.appendChild(wrapper);
            meters.push(new VUMeter(canvas, dbReadout, clipBtn));
        }
        return { console: meterConsole, meters };
    }
    // syncCard is the ONE write path for device and config data. It runs right
    // after every build and on every render, writing each dynamic field through a
    // diffed helper (setText / setHidden / classList.toggle) so a steady state
    // does not dirty the DOM or re-announce a live region. A field that exists in
    // the DOM but is not written here renders blank, making an omission a visible
    // defect rather than a silent staleness bug.
    syncCard(entry, d, cfgByDevice) {
        entry.device = d;
        const serving = d.state === "serving";
        const disabled = d.state === "disabled";
        const isUltra = d.mode === "pcm";
        // The toggle reflects the persisted (desired) enabled flag, which can differ
        // from the runtime state only briefly while a config reload applies. Fall
        // back to the runtime state only when the config is not loaded.
        const cfgDev = cfgByDevice.get(d.device);
        const configEnabled = cfgDev?.enabled ?? runtimeEnabled(d.state);
        // A disabled device is off by intent, not broken: neutral styling; a
        // failed/skipped device gets the error styling.
        entry.article.classList.toggle("active-stream", serving);
        entry.article.classList.toggle("error-stream", !serving && !disabled);
        // Avatar icon depends on state and mode; rewrite innerHTML only when the icon
        // actually changes (keyed by data-icon) so we do not reparse SVG every poll.
        const iconKey = serving ? (isUltra ? "ultra" : "mic") : disabled ? "mic" : "error";
        if (entry.avatar.dataset.icon !== iconKey) {
            entry.avatar.dataset.icon = iconKey;
            entry.avatar.innerHTML = serving ? (isUltra ? ICON_ULTRA : ICON_MIC) : disabled ? ICON_MIC : ICON_ERROR;
        }
        const avatarColor = serving && isUltra ? "var(--ultrasonic-purple)" : !serving && !disabled ? "var(--signal-crit)" : "";
        if (entry.avatar.style.color !== avatarColor)
            entry.avatar.style.color = avatarColor;
        setText(entry.titleEl, d.name);
        // Surface the sound card's hardware model (friendlyName) next to the ALSA id
        // so a device is identifiable by what it physically is, not only its config
        // name. Omit it when absent or when it only repeats the configured name.
        const hw = d.friendlyName?.trim();
        const showHw = !!hw && hw.toLowerCase() !== d.name.trim().toLowerCase();
        setText(entry.hwEl, showHw ? `ALSA: ${d.device} · ${hw}` : `ALSA: ${d.device}`);
        // Chips describe the live stream and are shown only while serving.
        const rate = d.negotiatedRate ?? d.rate;
        setText(entry.modeTag, modeLabel(d.mode));
        entry.modeTag.classList.toggle("ultrasonic", isUltra);
        entry.modeTag.classList.toggle("highlight", !isUltra);
        setHidden(entry.modeTag, !serving);
        setText(entry.rateTag, `${rate.toLocaleString("en-US")} Hz`);
        setHidden(entry.rateTag, !serving);
        const chLabel = channelLabel(d.channels);
        setText(entry.chTag, chLabel);
        setHidden(entry.chTag, !serving || !chLabel);
        setHidden(entry.lockEl, !serving || !this.status?.authRequired);
        const badge = deviceStateBadge(d.state);
        if (entry.statusEl.className !== badge.cls)
            entry.statusEl.className = badge.cls;
        setText(entry.statusEl, badge.label);
        const toggleAria = `Stream ${d.name}`;
        if (entry.toggleInput.getAttribute("aria-label") !== toggleAria)
            entry.toggleInput.setAttribute("aria-label", toggleAria);
        // Do not fight the user mid-interaction (the input is disabled while a PATCH
        // is in flight); otherwise keep it in sync with the persisted flag.
        if (!entry.toggleInput.disabled && entry.toggleInput.checked !== configEnabled) {
            entry.toggleInput.checked = configEnabled;
            entry.toggleInput.setAttribute("aria-checked", String(configEnabled));
        }
        else if (entry.toggleInput.getAttribute("aria-checked") !== String(entry.toggleInput.checked)) {
            entry.toggleInput.setAttribute("aria-checked", String(entry.toggleInput.checked));
        }
        const showPending = pendingStop(configEnabled, d.state);
        setText(entry.pendingNote, showPending ? PENDING_STOP_TEXT : "");
        setHidden(entry.pendingNote, !showPending);
        if (entry.live) {
            const url = this.rtspUrl(d);
            setText(entry.live.urlEl, url);
            if (entry.live.urlEl.title !== url)
                entry.live.urlEl.title = url;
            setText(entry.live.clientsEl, d.clientConnected ? "1 connected" : "0 connected");
            setText(entry.live.droppedEl, String(d.droppedFrames));
            setText(entry.live.negotiatedEl, `Negotiated: ${rate.toLocaleString("en-US")} Hz`);
        }
        if (entry.idle) {
            setHidden(entry.idle.banner, !d.error);
            const bannerKey = d.state === "failed" ? "error" : "warn";
            if (entry.idle.bannerIcon.dataset.icon !== bannerKey) {
                entry.idle.bannerIcon.dataset.icon = bannerKey;
                entry.idle.bannerIcon.innerHTML = d.state === "failed" ? ICON_ERROR : ICON_WARN;
            }
            setText(entry.idle.bannerDesc, d.error ?? "");
            setText(entry.idle.footerNote, nonServingFooterText(d.state, configEnabled));
        }
    }
    // captureFocus records where keyboard focus is inside a card before its article
    // is rebuilt, so restoreFocus can put it back. Focus inside the settings panel
    // is remembered by element identity (the panel is moved, not rebuilt); focus on
    // a rebuilt control is remembered by its data-focus key (toggle, gear, copy,
    // clip-N), which the new article recreates.
    captureFocus(entry) {
        // entry.article is undefined on the first mount (the entry has no rendered
        // card yet), so treat it as possibly absent rather than trusting the type.
        const art = entry.article;
        const active = document.activeElement;
        if (!(active instanceof HTMLElement) || !art || !art.contains(active))
            return null;
        if (entry.settingsWrap.contains(active))
            return { el: active };
        const key = active.dataset.focus;
        return key ? { key } : null;
    }
    restoreFocus(entry, saved) {
        if (!saved)
            return;
        if (saved.el) {
            // The panel node was moved into the new article and is connected again.
            if (saved.el.isConnected)
                saved.el.focus();
            return;
        }
        if (saved.key) {
            const node = entry.article.querySelector(`[data-focus="${saved.key}"]`);
            // A control that exists only in the serving shape (copy, clip-N) is gone
            // after a flip to idle; keep focus on the card via the gear rather than
            // letting it fall to <body>.
            (node ?? entry.gearBtn).focus();
        }
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
    async handleToggleEnabled(entry) {
        const input = entry.toggleInput;
        const want = input.checked;
        const id = entry.device.device;
        const name = entry.device.name;
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
                // base from this change even if the GET refresh below fails.
                store.applyConfig(res.config);
                // A refresh failure afterwards must NOT revert the toggle: the change is
                // already applied and reflected in the cached config above.
                await Promise.all([store.refreshConfig(), store.refreshDevices()]);
                const verb = want ? "Enabled" : "Disabled";
                showToast(res.restartRequired
                    ? `${verb} ${name}. Restart the appliance to apply.`
                    : `${verb} ${name}.`);
            }
            catch (err) {
                // Only a failed PATCH reverts the toggle: the mutation did not persist.
                input.checked = !want;
                input.setAttribute("aria-checked", String(!want));
                this.apiErrorToast(err, "Toggle failed");
            }
            finally {
                // Re-read the current toggle: a poll may have rebuilt the card during the
                // PATCH (a serving<->idle flip, or a captured-channel change) and replaced
                // the node this closure captured. Clear the busy state and restore focus on
                // the live node, falling back to the gear if the toggle is gone, so a
                // keyboard user is never stranded on the document body.
                const toggle = entry.toggleInput;
                toggle.disabled = false;
                toggle.removeAttribute("aria-busy");
                if (hadFocus)
                    (toggle.isConnected ? toggle : entry.gearBtn).focus();
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
            // Stale notice, shown by syncSettings when the saved config changed under
            // an open form. A Reload rebuilds the form from the fresh config; Save
            // keeps last-writer-wins. Hidden until a drift is detected.
            const staleNote = elem("span", "stale-note");
            staleNote.setAttribute("role", "status");
            // The message text is written by syncSettings when drift is detected: a
            // role=status region announces on a content change, so writing the text is
            // reliable where merely un-hiding pre-filled text is not.
            staleNote.appendChild(elem("span", "stale-msg"));
            const reloadBtn = elem("button", "btn-link", "Reload");
            reloadBtn.setAttribute("type", "button");
            reloadBtn.addEventListener("click", () => void this.reloadSettings(entry));
            staleNote.appendChild(reloadBtn);
            staleNote.hidden = true;
            const spacer = elem("span", "settings-actions-spacer");
            const cancelBtn = elem("button", "btn btn-secondary", "Cancel");
            cancelBtn.setAttribute("type", "button");
            const saveBtn = elem("button", "btn btn-primary", "Save Changes");
            saveBtn.setAttribute("type", "button");
            actions.append(removeBtn, badge, staleNote, spacer, cancelBtn, saveBtn);
            removeBtn.addEventListener("click", () => void this.removeDevice(entry, removeBtn));
            // Build from the saved config (source of truth), matched by ALSA id. Prefer
            // the persisted config (it carries the enabled flag); fall back to the
            // runtime device projected through deviceToConfig so enabled is always
            // present and collect() cannot drop it.
            const cfg = store.getState().config;
            const configured = cfg?.devices.find((cd) => cd.device === entry.device.device) ?? deviceToConfig(entry.device);
            const form = new DeviceSettingsForm(configured, () => { badge.hidden = false; entry.dirty = true; }, {
                friendlyName: entry.device.friendlyName,
                supportedRates: entry.device.supportedRates,
                supportedChannels: entry.device.supportedChannels,
            });
            entry.settingsForm = form;
            // Record what the form was built from, so an out-of-band change is detected.
            entry.formSource = configured;
            entry.staleNote = staleNote;
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
            saveBtn.addEventListener("click", () => this.saveDevice(entry, saveBtn, cancelBtn));
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
        entry.formSource = null;
        entry.staleNote = null;
    }
    // reloadSettings rebuilds the open form from the current config after an
    // out-of-band change, confirming a discard first if the operator has unsaved
    // edits. It never rewrites the form under the operator without asking.
    async reloadSettings(entry) {
        if (entry.dirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "Reloading replaces your unsaved changes with the current saved settings.",
                confirmLabel: "Discard and reload",
                danger: true,
            });
            if (!ok)
                return;
        }
        this.closeSettings(entry);
        this.toggleSettings(entry);
        // The Reload button was just removed with the old form; move focus to the
        // gear (which survives the rebuild) rather than letting it fall to <body>.
        entry.gearBtn.focus();
    }
    // syncSettings shows or hides the "changed elsewhere" notice on an open form by
    // comparing the config the form was built from against the current config
    // (excluding enabled). It never mutates the form; the operator chooses Reload
    // or Save. Called from render() for entries with an open form.
    syncSettings(entry, cfgByDevice) {
        if (!entry.settingsForm || !entry.staleNote)
            return;
        const current = cfgByDevice.get(entry.id);
        const stale = deviceConfigKey(current) !== deviceConfigKey(entry.formSource ?? undefined);
        // Write the message text (not just toggle visibility) so the role=status
        // region announces the drift as it appears and clears when resolved.
        const msg = entry.staleNote.querySelector(".stale-msg");
        if (msg)
            setText(msg, stale ? "Settings changed elsewhere. Save overwrites them. " : "");
        setHidden(entry.staleNote, !stale);
    }
    async saveDevice(entry, btn, cancelBtn) {
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
        // Show the save in flight and block a second submit or a discard while the
        // queued PATCH runs; markBusy keeps Save focusable (aria-disabled) while
        // Cancel, which is not focused, can simply be disabled.
        this.markBusy(btn, "Saving...");
        cancelBtn.disabled = true;
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
                    // closeSettings above destroyed the focused Save button and collapsed the
                    // panel, dropping focus to <body>. Return it to the gear (which survives
                    // any rebuild the refresh triggered, via the stable entry), matching the
                    // Cancel and reload paths so a keyboard user is not stranded at the top.
                    entry.gearBtn.focus();
                }
                catch (err) {
                    this.apiErrorToast(err, "Save failed");
                }
            });
        }
        finally {
            // Restore the buttons whether the save succeeded (its panel is torn down,
            // so this is a harmless no-op on detached nodes) or failed (they stay for
            // retry).
            this.clearBusy(btn, "Save Changes");
            cancelBtn.disabled = false;
        }
    }
    // handleCopyUrl copies the stream URL. When the appliance requires the access
    // token and this browser holds it, the copied URL embeds it as RTSP
    // credentials (rtsp://mic:<token>@host:port/path) so it pastes straight into
    // BirdNET-Go, ffmpeg or VLC; the displayed URL stays credential-free. The
    // credentialed form is derived from the displayed URL (which syncCard keeps
    // current), not from a path captured when the card was built, so a device
    // path edit is reflected in the copied URL.
    handleCopyUrl(btn, urlEl) {
        const shown = urlEl?.textContent;
        if (!shown || !navigator.clipboard)
            return;
        let url = shown;
        const token = this.status?.authRequired ? getToken() : null;
        if (token) {
            url = shown.replace("rtsp://", `rtsp://mic:${token}@`);
        }
        navigator.clipboard.writeText(url).then(() => {
            if (token)
                showToast("Stream URL copied with the access token included.");
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
        // The open-access notice shows while no token is configured. Guard the write
        // so this role=status banner is not re-announced on every ~3s poll when its
        // state is unchanged.
        const banner = document.getElementById("open-access-banner");
        if (banner)
            setHidden(banner, this.status.authRequired);
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
