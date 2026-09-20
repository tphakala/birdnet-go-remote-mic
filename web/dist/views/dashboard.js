import { store } from "../lib/store.js";
import { VUMeter } from "../components/vu-meter.js";
import { DeviceSettingsForm } from "../components/device-settings.js";
import { showToast } from "../components/toast.js";
import { api, ApiError } from "../lib/api.js";
import { clearBusy, deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError, reportClipboardFailure, setBusy, setHidden, setText, writeToClipboard } from "../lib/ui.js";
import { channelLabel, tallyStates } from "../lib/dashboard-core.js";
import { confirmDialog } from "../lib/modal.js";
import { getToken } from "../lib/auth.js";
// Trusted static SVG icon markup (no interpolation of runtime data).
const ICON_MIC = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path></svg>';
const ICON_ULTRA = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12h2"></path><path d="M6 8v8"></path><path d="M10 4v16"></path><path d="M14 6v12"></path><path d="M18 9v6"></path><path d="M22 12h-2"></path></svg>';
const ICON_ERROR = '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line></svg>';
const ICON_WARN = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>';
const ICON_COPY = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"></rect><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"></path></svg>';
const ICON_LOCK = '<svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="11" x="3" y="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 10 0v4"></path></svg>';
// Vertical faders (the mixing-desk "sliders" glyph) for the settings toggle:
// the panel adjusts capture and stream parameters, which reads closer to an
// audio console than a generic gear does.
const ICON_SLIDERS = '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="4" x2="4" y1="21" y2="14"></line><line x1="4" x2="4" y1="10" y2="3"></line><line x1="12" x2="12" y1="21" y2="12"></line><line x1="12" x2="12" y1="8" y2="3"></line><line x1="20" x2="20" y1="21" y2="16"></line><line x1="20" x2="20" y1="12" y2="3"></line><line x1="2" x2="6" y1="14" y2="14"></line><line x1="10" x2="14" y1="8" y2="8"></line><line x1="18" x2="22" y1="16" y2="16"></line></svg>';
const ICON_CHEVRON = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"></path></svg>';
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
// The copy button's resting and success labels, defined once so handleCopyUrl
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
const copyResetTimers = new WeakMap();
// flashCopied plays the copy-success feedback on btn: the green "copied" pulse,
// the visible label swapped to "Copied!" and the accessible name to ariaDone
// (so the success is announced too), all restored after a moment. It restores
// to the fixed resting values rather than re-reading the DOM, and clears any
// pending restore first, so a rapid second click cannot strand the button on
// "Copied!".
function flashCopied(btn, label, rest, ariaRest, ariaDone) {
    btn.classList.add("copied");
    if (label)
        label.textContent = COPY_LABEL_DONE;
    btn.setAttribute("aria-label", ariaDone);
    const prev = copyResetTimers.get(btn);
    if (prev !== undefined)
        window.clearTimeout(prev);
    copyResetTimers.set(btn, window.setTimeout(() => {
        btn.classList.remove("copied");
        if (label)
            label.textContent = rest;
        btn.setAttribute("aria-label", ariaRest);
        copyResetTimers.delete(btn);
    }, 1600));
}
// nonServingFooterText is the footer message for a card that is not serving.
function nonServingFooterText(state, configEnabled) {
    if (state === "disabled") {
        return configEnabled
            ? "Enabling; this device starts serving shortly."
            : "Streaming is disabled for this device. Enable it to start serving.";
    }
    return "Excluded from the RTSP stream server. Other active devices continue serving without interruption.";
}
// Sequence for the settings panels' element ids (aria-controls targets).
let settingsSeq = 0;
// Sequence for the token tag's description element ids (aria-describedby targets).
let tokenDescSeq = 0;
function iconSpan(markup, className) {
    const s = document.createElement("span");
    if (className)
        s.className = className;
    // Decorative: every icon built through here (copy, lock, error banner) sits
    // next to text that already carries its meaning, so hide it from assistive
    // tech rather than announcing an unlabeled graphic.
    s.setAttribute("aria-hidden", "true");
    // Static trusted markup only; never runtime/user data.
    s.innerHTML = markup;
    return s;
}
// The runtime device carries the runtime-visible configured fields; project it
// to the config shape as a fallback base for the device-list patch.
function deviceToConfig(d) {
    const c = {
        name: d.name, device: d.device, path: d.path, mode: d.mode,
        rate: d.rate, channels: d.channels, format: d.format,
        enabled: runtimeEnabled(d.state),
    };
    if (d.opus)
        c.opus = d.opus;
    // quietAlert is intentionally omitted: the runtime Device carries no such
    // field, so there is no value to project. This projection is a render/seed
    // fallback only; the mutating device PATCH paths (handleToggleEnabled,
    // saveDevice) refuse to run until GET /config has loaded, so quietAlert is
    // never persisted from this fallback and an existing opt-out cannot be reset.
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
// elsewhere. quietAlert IS included: the settings form edits it, so an
// out-of-band change to it should raise the "changed elsewhere" notice like
// every other form field. It is normalised with `?? true` (the backend
// absent-default) so an absent value and an explicit true hash identically.
function deviceConfigKey(cd) {
    if (!cd)
        return "";
    return JSON.stringify([
        cd.name, cd.path, cd.mode, cd.rate,
        // Guard the spread: the runtime devices payload is normalized to always
        // carry a channels array (see the store), but a config payload is not, so a
        // missing or null channels field must not throw here and crash the pass.
        [...(cd.channels ?? [])], cd.format, cd.opus?.bitrate ?? null,
        cd.quietAlert ?? true,
    ]);
}
export class DashboardView {
    // Cards keyed by immutable device id (the stable identity). byName maps
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
    // Set while a reconcile() is queued on the microtask, so the several store
    // events a single poll tick fires collapse into one pass (see render()).
    renderScheduled = false;
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
    // render coalesces the up-to-three store events per 3 s poll tick (devices,
    // status and config each request it) into a single reconcile on the microtask
    // queue, so same-tick events collapse into one pass instead of three. The pass
    // is idempotent and diffed, so this only drops redundant CPU. Focus
    // restoration lives in the mutation handlers (which read the live nodes) and in
    // mount(); both run inside microtask timing during their awaits, so coalescing
    // does not disturb them.
    render() {
        if (this.renderScheduled)
            return;
        this.renderScheduled = true;
        queueMicrotask(() => {
            this.renderScheduled = false;
            this.reconcile();
        });
    }
    // reconcile is the single reconcile pass, driven by store state. For each
    // runtime device it gets or creates the stable entry, rebuilds the article only
    // when the shape changed, then syncs every field through the one write path. It
    // then removes gone cards, orders the rack with a diff (no DOM move in steady
    // state, which is what keeps keyboard focus from being dropped every poll),
    // rebuilds the name index for the levels stream, and reconciles open forms.
    reconcile() {
        if (!this.rack)
            return;
        // Capture the narrowed rack: the intervening syncCard/mount calls below make
        // TS re-widen this.rack to include null, so hold a non-null local for the
        // ordering pass rather than re-guarding it.
        const rack = this.rack;
        const devices = store.getState().devices;
        if (this.emptyEl) {
            this.emptyEl.hidden = devices.length > 0;
            // Clear the alert live-region role set by renderLoadError so the benign
            // empty/loaded state is not re-announced as an error.
            this.emptyEl.removeAttribute("role");
            // Replace the static "Loading devices..." placeholder once we know there
            // are genuinely zero configured devices, and point to the next step: the
            // Available Devices section below is where a detected device is enabled.
            if (devices.length === 0) {
                const hasAvailable = store.getState().available.length > 0;
                setText(this.emptyEl, hasAvailable
                    ? "No capture devices are configured yet. Enable one from Available Devices below to start streaming."
                    : "No capture devices are configured. Connect capture hardware; it appears under Available Devices below, ready to enable.");
            }
        }
        // Index the persisted config by device id once per pass so the per-card
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
        // Order the rack to match the device list with a diff. Walk the articles by
        // previous sibling rather than indexing this.rack.children: #channel-rack
        // also holds the hidden #rack-empty placeholder, so an index-based compare was
        // off by one and moved a card every poll. Steady state performs no DOM moves,
        // so focus inside a card is never dropped by re-inserting its node.
        let prev = null;
        for (const d of devices) {
            const entry = this.cards.get(d.device);
            if (!entry)
                continue;
            const target = prev ? prev.nextElementSibling : rack.firstElementChild;
            if (entry.article !== target)
                rack.insertBefore(entry.article, target);
            prev = entry.article;
        }
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
        // Fall back to the short ALSA address, not the long stable id, when the card
        // has no friendly name: the address is what the rest of the card shows.
        info.appendChild(elem("div", "device-title", d.friendlyName || d.hwAddr || d.device));
        const sub = elem("div", "available-sub");
        const addr = elem("span", "mono", d.hwAddr ?? d.device);
        addr.title = `Device id: ${d.device}`;
        sub.appendChild(addr);
        if (d.idStable === false)
            sub.appendChild(elem("span", "available-caps", "no stable id"));
        const caps = capsSummary(d);
        if (caps)
            sub.appendChild(elem("span", "available-caps", caps));
        info.appendChild(sub);
        const enableBtn = elem("button", "btn btn-primary available-enable", "Enable");
        enableBtn.setAttribute("type", "button");
        // Name the device in the accessible label: there is one Enable button per
        // available device, so a bare "Enable" is ambiguous to a screen-reader user.
        enableBtn.setAttribute("aria-label", `Enable ${d.friendlyName || d.hwAddr || d.device}`);
        if (this.provisioning.has(d.device))
            setBusy(enableBtn, "Enabling...");
        enableBtn.addEventListener("click", () => void this.provisionDevice(d, enableBtn));
        card.append(info, enableBtn);
        return card;
    }
    async provisionDevice(d, btn) {
        if (this.provisioning.has(d.device))
            return;
        this.provisioning.add(d.device);
        setBusy(btn, "Enabling...");
        try {
            // Serialize through the same queue as toggles and settings saves: those
            // submit a full-array PATCH built from the cached config, so a provision
            // running concurrently could be clobbered by a stale PATCH (or vice versa).
            // The refreshes run inside the task so the next queued mutation rebuilds
            // from the post-provision config.
            await this.enqueue(async () => {
                const created = await api.provisionDevice({ device: d.device });
                await Promise.all([store.refreshDevices(), store.refreshAvailable(), store.refreshConfig()]);
                const ch = created.channels.length === 1 ? ` on channel ${created.channels[0]}` : "";
                showToast(`Enabled ${created.name}${ch}. Streaming on ${created.path}.`);
            });
        }
        catch (err) {
            this.apiErrorToast(err, "Enable failed");
        }
        finally {
            this.provisioning.delete(d.device);
            clearBusy(btn, "Enable");
        }
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
        setBusy(btn, "Removing...");
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
            clearBusy(btn, "Remove");
        }
    }
    rtspUrl(d) {
        const port = rtspPort(this.status?.rtspListen);
        return `rtsp://${window.location.hostname}:${port}${d.path}`;
    }
    // newEntry creates the stable identity for a device: the settings panel node
    // (which outlives article rebuilds) plus a first built article, assembled into
    // a fully-typed CardEntry in one checked literal (no partial `as` cast).
    newEntry(d) {
        const settingsWrap = elem("div", "card-settings");
        settingsWrap.id = `card-settings-${++settingsSeq}`;
        settingsWrap.hidden = true;
        const parts = this.buildArticle(d);
        const entry = {
            ...parts,
            device: d,
            shape: shapeKey(d),
            settingsWrap,
            settingsForm: null,
            formSource: null,
            formSourceKey: "",
            staleNote: null,
            expanded: false,
            dirty: false,
        };
        this.wireArticleHandlers(entry);
        entry.article.appendChild(entry.settingsWrap);
        return entry;
    }
    // mount rebuilds the article for a device's current shape, moving the owned
    // settings panel into the new article and preserving keyboard focus across the
    // swap. It is called whenever shapeKey changes (serving <-> idle, or a change in
    // the captured channel count); the first article is built directly in newEntry.
    mount(entry, d) {
        const saved = this.captureFocus(entry);
        const oldArticle = entry.article;
        // The old serving body's meters own canvas rAF loops; stop them before the
        // article is discarded.
        entry.live?.meters.forEach((m) => m.destroy());
        // Refresh the article and its named nodes in one checked assignment, then
        // re-wire the fresh controls to the stable entry.
        Object.assign(entry, this.buildArticle(d));
        this.wireArticleHandlers(entry);
        // Move the owned settings panel (and its live form, if open) into the new
        // article, and restore its expanded visual state.
        entry.article.appendChild(entry.settingsWrap);
        if (entry.expanded) {
            entry.settingsWrap.hidden = false;
            entry.article.classList.add("expanded");
        }
        entry.shape = shapeKey(d);
        // Swap in place if the card was already mounted in the rack; otherwise the
        // ordering pass in reconcile() inserts it.
        if (oldArticle.parentNode)
            oldArticle.replaceWith(entry.article);
        this.restoreFocus(entry, saved);
    }
    // syncSettingsButton reflects whether the settings panel is open on the
    // disclosure button: aria-expanded for assistive tech, the chevron flip and
    // accent via the card's .expanded class. It runs wherever expanded changes
    // and after every rebuild, which creates a fresh button.
    syncSettingsButton(entry) {
        entry.settingsBtn.setAttribute("aria-expanded", String(entry.expanded));
        entry.settingsBtn.setAttribute("aria-controls", entry.settingsWrap.id);
    }
    // wireArticleHandlers attaches the settings and toggle handlers to a freshly built
    // article's controls. They close over the stable entry (not the disposable
    // nodes), so a later rebuild simply re-wires the new nodes to the same entry.
    wireArticleHandlers(entry) {
        entry.settingsBtn.addEventListener("click", () => this.toggleSettings(entry));
        entry.toggleInput.addEventListener("change", () => void this.handleToggleEnabled(entry));
        this.syncSettingsButton(entry);
    }
    // buildArticle creates the DOM skeleton for a device's shape with NO device
    // data written: header nodes with empty text, chips and lock hidden, toggle
    // unchecked, the body for the shape. It returns the node bundle; syncCard fills
    // every value and wireArticleHandlers binds the controls to the stable entry.
    // Trusted static SVG for the copy, lock and settings icons is assigned here; the
    // avatar icon depends on state and is written by syncCard.
    buildArticle(d) {
        const serving = d.state === "serving";
        const article = elem("article", "rack-card");
        // Header
        const header = elem("div", "rack-header");
        const ident = elem("div", "device-ident");
        const avatar = elem("span", "device-avatar");
        // Decorative: the avatar icon repeats the state shown by the status badge.
        avatar.setAttribute("aria-hidden", "true");
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
        // "Token" marks a stream that needs the access token, and copies it: the
        // same feedback as Copy URL. Its aria-label names the action; the title
        // adds why the token matters for a pointer user.
        const lockEl = elem("button", "tech-tag lock-tag");
        lockEl.setAttribute("type", "button");
        lockEl.setAttribute("aria-label", TOKEN_ARIA);
        lockEl.title = "Pulling this stream requires the access token. Click to copy it.";
        lockEl.dataset.focus = "token";
        // Build it with the SAME predicate syncCard uses (serving and auth required)
        // rather than always hidden: mount() runs restoreFocus BEFORE the following
        // syncCard, so a keyboard user who was on the Token tag before a rebuild
        // lands back on it when it should be visible, and still falls back cleanly to
        // the settings button when it should not.
        lockEl.hidden = !(serving && !!this.status?.authRequired);
        lockEl.appendChild(iconSpan(ICON_LOCK, "icon-copy"));
        lockEl.appendChild(elem("span", "copy-label", TOKEN_LABEL));
        // aria-label names the action ("Copy access token"); a described-by span adds
        // why the token matters, which a title attribute alone does not reliably
        // reach a screen-reader user.
        const tokenDesc = elem("span", "visually-hidden", "Pulling this stream requires the access token");
        tokenDesc.id = `token-desc-${++tokenDescSeq}`;
        lockEl.setAttribute("aria-describedby", tokenDesc.id);
        lockEl.appendChild(tokenDesc);
        lockEl.addEventListener("click", () => this.handleCopyToken(lockEl));
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
        // Settings disclosure. It sits at the right end of the footer, directly
        // above the panel it expands, in both body shapes (a disabled or failed
        // device still needs its settings). syncSettingsButton writes its
        // expanded state.
        const settingsBtn = elem("button", "card-settings-toggle");
        settingsBtn.setAttribute("type", "button");
        settingsBtn.dataset.focus = "settings";
        settingsBtn.appendChild(iconSpan(ICON_SLIDERS, "settings-toggle-icon"));
        settingsBtn.appendChild(elem("span", undefined, "Settings"));
        settingsBtn.appendChild(iconSpan(ICON_CHEVRON, "settings-toggle-chevron"));
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
            copyBtn.setAttribute("aria-label", COPY_ARIA);
            copyBtn.title = COPY_ARIA;
            copyBtn.dataset.focus = "copy";
            copyBtn.appendChild(iconSpan(ICON_COPY, "icon-copy"));
            copyBtn.appendChild(elem("span", "copy-label", COPY_LABEL));
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
            // The negotiated rate and the settings toggle share the right end.
            const footerEnd = elem("div", "rack-footer-end");
            footerEnd.append(negotiated, settingsBtn);
            footer.appendChild(footerEnd);
            article.appendChild(footer);
            live = { urlEl, clientsEl, droppedEl, negotiatedEl, meters: built.meters, rows: built.rows };
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
            footer.appendChild(settingsBtn);
            article.appendChild(footer);
            idle = { banner, bannerIcon, bannerDesc, footerNote };
        }
        return {
            article, avatar, titleEl, hwEl, modeTag, rateTag, chTag, lockEl,
            statusEl, toggleInput, settingsBtn, pendingNote, live, idle,
        };
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
        const rows = [];
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
                // A tally light in front of the channel number: lit while a stream
                // carries the channel (syncCard sets it), so the streamed channels stand
                // out on a multi-channel interface. A mono device has a single row that
                // is always streamed, so it gets no label and no light.
                const label = elem("span", "meter-channel-label mono");
                label.setAttribute("aria-hidden", "true");
                label.appendChild(elem("span", "meter-tally"));
                label.appendChild(elem("span", undefined, `Ch ${chNum}`));
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
            rows.push(wrapper);
            meters.push(new VUMeter(canvas, dbReadout, clipBtn));
        }
        return { console: meterConsole, meters, rows };
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
        // Show the hardware the configured id resolves to right now: its current ALSA
        // address and the sound card's model (friendlyName). When the id resolved to
        // no single present device the address is absent: a serving card-index device
        // opened without a resolution (the container fallback) still shows its
        // configured id, and anything else shows "No matching hardware". Omit the
        // model when absent or when it only repeats the configured name. The persisted
        // id is long, so it goes in the tooltip rather than the line.
        const hw = d.friendlyName?.trim();
        const showHw = !!hw && hw.toLowerCase() !== d.name.trim().toLowerCase();
        const addr = d.hwAddr ? `ALSA: ${d.hwAddr}` : serving ? `ALSA: ${d.device}` : "No matching hardware";
        let hwText = showHw ? `${addr} · ${hw}` : addr;
        // A card-index id can name a different device after a reboot or replug; the
        // settings panel's Device id hint carries the remedy (remove and re-add).
        if (d.idStable === false)
            hwText += " · card index (can change after a reboot)";
        setText(entry.hwEl, hwText);
        if (entry.hwEl.title !== `Device id: ${d.device}`)
            entry.hwEl.title = `Device id: ${d.device}`;
        // Chips describe the live stream and are shown only while serving.
        const rate = d.negotiatedRate ?? d.rate;
        setText(entry.modeTag, modeLabel(d.mode));
        entry.modeTag.classList.toggle("ultrasonic", isUltra);
        entry.modeTag.classList.toggle("highlight", !isUltra);
        setHidden(entry.modeTag, !serving);
        setText(entry.rateTag, `${rate.toLocaleString("en-US")} Hz`);
        setHidden(entry.rateTag, !serving);
        // Show every streamed channel (the union across the device's streams), so the
        // header channel tag agrees with the per-channel tally lights below rather
        // than showing only the first stream's channels (d.channels).
        const chLabel = channelLabel(d.streamedChannels ?? d.channels);
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
        // The settings disclosure reads "Settings" for every card; name the device so
        // a screen-reader user can tell which card's settings the button opens.
        const settingsAria = `Settings for ${d.name}`;
        if (entry.settingsBtn.getAttribute("aria-label") !== settingsAria)
            entry.settingsBtn.setAttribute("aria-label", settingsAria);
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
            // Mark each captured channel live when a stream carries it. Rows index
            // hardware channels from 0, selections number them from 1.
            if (entry.live.rows.length > 1) {
                const states = tallyStates(d.streamedChannels ?? d.channels, entry.live.rows.length);
                entry.live.rows.forEach((row, i) => {
                    const on = states[i];
                    row.classList.toggle("ch-live", on);
                    row.classList.toggle("ch-off", !on);
                    const title = on ? `Channel ${i + 1}: streamed` : `Channel ${i + 1}: not streamed`;
                    if (row.title !== title)
                        row.title = title;
                    // The tally light is aria-hidden, so carry its streamed/not-streamed
                    // meaning on the row's own exposed control: the clip button.
                    const clip = row.querySelector(".clip-latch-btn");
                    const clipAria = `Channel ${i + 1} (${on ? "streamed" : "not streamed"}) clip indicator, click to clear`;
                    if (clip && clip.getAttribute("aria-label") !== clipAria)
                        clip.setAttribute("aria-label", clipAria);
                });
            }
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
    // a rebuilt control is remembered by its data-focus key (toggle, settings, copy,
    // token, clip-N), which the new article recreates.
    captureFocus(entry) {
        // captureFocus runs only from mount(), which rebuilds an existing article, so
        // entry.article is always present (the first article is built in newEntry).
        const art = entry.article;
        const active = document.activeElement;
        if (!(active instanceof HTMLElement) || !art.contains(active))
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
            // A control that is gone or hidden in the rebuilt shape (copy and clip-N
            // after a flip to idle, or the token tag when the stream no longer needs
            // the token) cannot take focus; keep focus on the card via the settings
            // button rather than letting it fall to <body>.
            (node && !node.hidden && !node.closest("[hidden]") ? node : entry.settingsBtn).focus();
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
        // Refuse to mutate the device list from the runtime fallback: until GET
        // /config has loaded, deviceConfigBase() projects via deviceToConfig, which
        // omits config-only fields (quietAlert), so a full-array PATCH would reset
        // every device's opt-out. config only ever goes null -> loaded, so checking
        // here is equivalent to checking inside the queued task.
        if (!store.getState().config) {
            input.checked = !want;
            input.setAttribute("aria-checked", String(!want));
            showToast("Configuration has not loaded yet. Try again in a moment.", "warn");
            return;
        }
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
                // the live node, falling back to the settings button if the toggle is gone, so a
                // keyboard user is never stranded on the document body.
                const toggle = entry.toggleInput;
                toggle.disabled = false;
                toggle.removeAttribute("aria-busy");
                if (hadFocus)
                    (toggle.isConnected ? toggle : entry.settingsBtn).focus();
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
            // Build from the saved config (source of truth), matched by device id. Prefer
            // the persisted config (it carries the enabled flag); fall back to the
            // runtime device projected through deviceToConfig so enabled is always
            // present and collect() cannot drop it.
            const cfg = store.getState().config;
            const configured = cfg?.devices.find((cd) => cd.device === entry.device.device) ?? deviceToConfig(entry.device);
            const form = new DeviceSettingsForm(configured, () => { badge.hidden = false; entry.dirty = true; }, {
                friendlyName: entry.device.friendlyName,
                supportedRates: entry.device.supportedRates,
                supportedChannels: entry.device.supportedChannels,
                idStable: entry.device.idStable,
            });
            entry.settingsForm = form;
            // Record what the form was built from, so an out-of-band change is detected.
            // Cache its config key now: formSource is fixed until the form is rebuilt,
            // so syncSettings compares against this instead of re-serialising it every
            // render while the form is open.
            entry.formSource = configured;
            entry.formSourceKey = deviceConfigKey(configured);
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
        this.syncSettingsButton(entry);
    }
    // requestCloseSettings collapses the panel, but first confirms the discard if
    // the form has unsaved edits. It guards every collapse path (the Cancel button
    // and the settings button), so a stray click cannot silently drop pending changes.
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
        // so return focus to the settings button, which always survives the collapse,
        // rather than letting focus fall to <body>.
        entry.settingsBtn.focus();
    }
    closeSettings(entry) {
        entry.expanded = false;
        entry.dirty = false;
        entry.settingsWrap.hidden = true;
        entry.article.classList.remove("expanded");
        this.syncSettingsButton(entry);
        entry.settingsWrap.textContent = "";
        entry.settingsForm?.destroy();
        entry.settingsForm = null;
        entry.formSource = null;
        entry.formSourceKey = "";
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
        // settings button (which survives the rebuild) rather than letting it fall to <body>.
        entry.settingsBtn.focus();
    }
    // syncSettings shows or hides the "changed elsewhere" notice on an open form by
    // comparing the config the form was built from (cached formSourceKey) against
    // the current config (excluding enabled). It never mutates the form; the
    // operator chooses Reload or Save. Called from reconcile() for entries with an
    // open form.
    syncSettings(entry, cfgByDevice) {
        if (!entry.settingsForm || !entry.staleNote)
            return;
        const current = cfgByDevice.get(entry.device.device);
        const stale = deviceConfigKey(current) !== entry.formSourceKey;
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
            // Move focus to the first flagged field, matching saveAuth, so a keyboard
            // user is taken to what needs fixing instead of staying on the Save button.
            form.focusFirstInvalid();
            return;
        }
        // Refuse to save from the runtime fallback: until GET /config has loaded,
        // deviceConfigBase() projects via deviceToConfig, which omits config-only
        // fields (quietAlert), so this full-array PATCH would reset every device's
        // opt-out. config only ever goes null -> loaded.
        if (!store.getState().config) {
            showToast("Configuration has not loaded yet. Try again in a moment.", "warn");
            return;
        }
        const edited = form.collect();
        // Show the save in flight and block a second submit or a discard while the
        // queued PATCH runs; markBusy keeps Save focusable (aria-disabled) while
        // Cancel, which is not focused, can simply be disabled.
        setBusy(btn, "Saving...");
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
                    // panel, dropping focus to <body>. Return it to the settings button (which survives
                    // any rebuild the refresh triggered, via the stable entry), matching the
                    // Cancel and reload paths so a keyboard user is not stranded at the top.
                    entry.settingsBtn.focus();
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
            clearBusy(btn, "Save Changes");
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
        if (!shown)
            return;
        let url = shown;
        const token = this.status?.authRequired ? getToken() : null;
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
            if (token)
                showToast("Stream URL copied with the access token included.");
            // With the token included the toast already announces the copy, so the
            // accessible name stays put rather than announcing it twice.
            flashCopied(btn, btn.querySelector(".copy-label"), COPY_LABEL, COPY_ARIA, token ? COPY_ARIA : COPY_ARIA_DONE);
        });
    }
    // handleCopyToken copies the access token this browser signed in with. The
    // tag only shows while the appliance requires a token, so a signed-in browser
    // holds one; without it (or without a clipboard, as on a plain http origin)
    // the operator is told rather than getting a silent no-op.
    handleCopyToken(btn) {
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
            flashCopied(btn, btn.querySelector(".copy-label"), TOKEN_LABEL, TOKEN_ARIA, TOKEN_ARIA);
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
