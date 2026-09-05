import { api, ApiError } from "../lib/api.js";
import { store } from "../lib/store.js";
import { deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError, setText } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
import { triggerApplianceRestart } from "../components/restart-modal.js";
import { showToast } from "../components/toast.js";
import { generateToken, setToken } from "../lib/auth.js";
// TOKEN_RULE mirrors the appliance's auth.token validation (auth.ValidToken)
// so an obviously invalid token is caught before the round trip.
const TOKEN_RULE = /^(|[A-Za-z0-9._~-]{12,128})$/;
export class SystemView {
    tilesEl;
    infoEl;
    infoCardEl;
    rowsEl;
    system = null;
    status = null;
    netCardEl;
    netActionsEl;
    discoveryEl;
    netDirty = false;
    authCardEl;
    authStateEl;
    authTokenEl;
    authErrorEl;
    authActionsEl;
    authDirty = false;
    authSaving = false;
    constructor() {
        this.tilesEl = document.getElementById("sys-tiles");
        this.infoEl = document.getElementById("sys-info");
        this.infoCardEl = document.getElementById("sys-info-card");
        this.rowsEl = document.getElementById("sys-device-rows");
        this.netCardEl = document.getElementById("sys-network-card");
        this.netActionsEl = document.getElementById("sys-network-actions");
        this.discoveryEl = document.getElementById("sys-discovery-enabled");
        this.authCardEl = document.getElementById("sys-auth-card");
        this.authStateEl = document.getElementById("sys-auth-state");
        this.authTokenEl = document.getElementById("sys-auth-token");
        this.authErrorEl = document.getElementById("sys-auth-error");
        this.authActionsEl = document.getElementById("sys-auth-actions");
        const btn = document.getElementById("btn-sys-restart");
        if (btn)
            btn.addEventListener("click", () => triggerApplianceRestart());
        store.addEventListener("system", (e) => {
            this.system = e.detail;
            this.renderTiles();
            this.renderInfo();
        });
        store.addEventListener("status", (e) => {
            this.status = e.detail;
            this.renderTiles();
            this.renderInfo();
        });
        store.addEventListener("devices", (e) => {
            this.renderDeviceRows(e.detail);
        });
        store.addEventListener("config", (e) => {
            const cfg = e.detail;
            if (!this.netDirty && cfg)
                this.populateNetwork(cfg);
            if (!this.authDirty && cfg)
                this.populateAuth(cfg);
        });
        store.addEventListener("loaderror", (e) => {
            const detail = e.detail;
            if (detail.systemFailed)
                this.renderLoadError(detail.message);
        });
        this.bindNetwork();
        this.bindAuth();
    }
    bindAuth() {
        const input = this.authTokenEl;
        if (!input)
            return;
        input.addEventListener("input", () => {
            this.authDirty = true;
            this.setAuthError("");
            if (this.authActionsEl)
                this.authActionsEl.hidden = false;
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
            if (!navigator.clipboard)
                return;
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
    setAuthError(message) {
        if (this.authErrorEl)
            this.authErrorEl.textContent = message;
        this.authTokenEl?.setAttribute("aria-invalid", message ? "true" : "false");
        this.authTokenEl?.closest(".form-field")?.classList.toggle("invalid", !!message);
    }
    populateAuth(cfg) {
        if (this.authCardEl)
            this.authCardEl.hidden = false;
        const token = cfg.auth?.token ?? "";
        // Only write when changed: this runs on every 3 s config poll (while not
        // dirty) and a redundant assignment to a field the operator has revealed is
        // needless churn.
        if (this.authTokenEl && this.authTokenEl.value !== token)
            this.authTokenEl.value = token;
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
        if (this.authActionsEl)
            this.authActionsEl.hidden = true;
    }
    // discardAuth reverts the token field to the saved value, confirming first
    // when there are unsaved edits so a stray click cannot drop a generated token.
    async discardAuth() {
        if (this.authDirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "The access token has unsaved changes that will be lost.",
                confirmLabel: "Discard",
                danger: true,
            });
            if (!ok)
                return;
        }
        const cfg = store.getState().config;
        if (cfg)
            this.populateAuth(cfg);
    }
    // saveAuth persists the token. Clearing it opens the appliance to the network,
    // so that path confirms first. On success the UI's own stored token is swapped
    // synchronously before any follow-up request, so the next poll already carries
    // the new value and cannot be rejected by the freshly rotated appliance.
    async saveAuth() {
        if (this.authSaving || !this.authTokenEl)
            return;
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
            if (!ok)
                return;
        }
        const saveBtn = document.getElementById("btn-auth-save");
        const discardBtn = document.getElementById("btn-auth-discard");
        this.authSaving = true;
        if (saveBtn) {
            saveBtn.disabled = true;
            saveBtn.setAttribute("aria-busy", "true");
            saveBtn.textContent = "Saving...";
        }
        if (discardBtn)
            discardBtn.disabled = true;
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
            }
            else {
                showToast(token ? "Access token saved. BirdNET-Go and players now need it." : "Open access enabled.", token ? "info" : "warn");
            }
        }
        catch (err) {
            if (err instanceof ApiError && err.errors && err.errors.length > 0) {
                this.setAuthError(err.errors[0].reason ?? err.title);
            }
            else {
                const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
                showToast(`Save failed: ${msg}`, "error");
            }
        }
        finally {
            store.endTokenSwap();
            this.authSaving = false;
            if (saveBtn) {
                saveBtn.disabled = false;
                saveBtn.removeAttribute("aria-busy");
                saveBtn.textContent = "Save Token";
            }
            if (discardBtn)
                discardBtn.disabled = false;
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
    renderLoadError(message) {
        if (!this.tilesEl)
            return;
        this.tilesEl.textContent = "";
        const p = elem("p", "cfg-empty");
        this.tilesEl.appendChild(p);
        renderLoadError(p, message, "Loading system telemetry...", () => void store.retry());
    }
    bindNetwork() {
        if (this.discoveryEl) {
            this.discoveryEl.addEventListener("change", () => {
                this.netDirty = true;
                if (this.netActionsEl)
                    this.netActionsEl.hidden = false;
            });
        }
        document.getElementById("btn-network-save")?.addEventListener("click", () => this.saveNetwork());
        document.getElementById("btn-network-discard")?.addEventListener("click", () => void this.discardNetwork());
    }
    // discardNetwork reverts the network form to the saved config, confirming
    // first when there are unsaved edits so a stray click cannot drop them.
    async discardNetwork() {
        if (this.netDirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "The network settings have unsaved changes that will be lost.",
                confirmLabel: "Discard",
                danger: true,
            });
            if (!ok)
                return;
        }
        const cfg = store.getState().config;
        if (cfg)
            this.populateNetwork(cfg);
    }
    populateNetwork(cfg) {
        if (this.netCardEl)
            this.netCardEl.hidden = false;
        // Config is polled every 3 s, so only write an input when its value actually
        // changes: a redundant assignment is wasteful and could disturb a field the
        // operator is reading. These run only while the form is not dirty (guarded by
        // the caller), so they never overwrite an in-progress edit.
        const rtsp = document.getElementById("sys-rtsp-listen");
        const rtspVal = cfg.listen ?? "";
        if (rtsp && rtsp.value !== rtspVal)
            rtsp.value = rtspVal;
        const mgmt = document.getElementById("sys-mgmt-listen");
        const mgmtVal = cfg.management?.listen ?? "(default)";
        if (mgmt && mgmt.value !== mgmtVal)
            mgmt.value = mgmtVal;
        const discovery = cfg.discovery?.enabled ?? true;
        if (this.discoveryEl && this.discoveryEl.checked !== discovery)
            this.discoveryEl.checked = discovery;
        this.netDirty = false;
        if (this.netActionsEl)
            this.netActionsEl.hidden = true;
    }
    async saveNetwork() {
        try {
            const res = await api.patchConfig({ discovery: { enabled: this.discoveryEl?.checked ?? true } });
            this.netDirty = false;
            if (this.netActionsEl)
                this.netActionsEl.hidden = true;
            await store.refreshConfig();
            showToast(res.restartRequired ? "Discovery setting saved. Restart the appliance to apply." : "Discovery setting applied.");
        }
        catch (err) {
            const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
            showToast(`Save failed: ${msg}`, "error");
        }
    }
    tile(label, sub, value, unit, barPct) {
        const tile = elem("div", "system-tile");
        const header = elem("div", "tile-header");
        header.appendChild(elem("span", undefined, label));
        if (sub)
            header.appendChild(elem("span", "mono", sub));
        tile.appendChild(header);
        const val = elem("div", "tile-value mono");
        val.appendChild(elem("span", undefined, value));
        if (unit)
            val.appendChild(elem("span", "telemetry-unit", unit));
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
    renderTiles() {
        if (!this.tilesEl)
            return;
        const sys = this.system;
        if (!sys)
            return;
        this.tilesEl.textContent = "";
        const cores = sys.cpuCores > 0 ? `${sys.cpuCores} Cores` : "";
        this.tilesEl.appendChild(this.tile("CPU Utilization", cores, sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a", "%", sys.cpuPercent ?? 0));
        if (sys.memTotalBytes > 0) {
            const pct = (sys.memUsedBytes / sys.memTotalBytes) * 100;
            const usedMb = Math.round(sys.memUsedBytes / 1048576);
            const totalMb = Math.round(sys.memTotalBytes / 1048576);
            this.tilesEl.appendChild(this.tile("Memory", `${totalMb} MB Total`, String(usedMb), "MB used", pct));
        }
        this.tilesEl.appendChild(this.tile("SoC Temperature", "", sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a", "deg C", sys.tempCelsius !== undefined ? (sys.tempCelsius / 85) * 100 : undefined));
        if (sys.diskTotalBytes > 0) {
            const pct = (sys.diskUsedBytes / sys.diskTotalBytes) * 100;
            const usedGb = (sys.diskUsedBytes / 1073741824).toFixed(1);
            const totalGb = (sys.diskTotalBytes / 1073741824).toFixed(1);
            this.tilesEl.appendChild(this.tile("Disk", `${totalGb} GB Total`, usedGb, "GB used", pct));
        }
    }
    renderInfo() {
        if (!this.infoEl)
            return;
        const sys = this.system;
        const st = this.status;
        if (!sys && !st)
            return;
        const rows = [];
        if (sys) {
            rows.push(["Hostname", sys.hostname || "-"]);
            rows.push(["Platform", sys.platform || "-"]);
            if (sys.cpuModel)
                rows.push(["CPU", `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`]);
            if (sys.os)
                rows.push(["OS", sys.os]);
            if (sys.kernel)
                rows.push(["Kernel", sys.kernel]);
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
        if (this.infoCardEl)
            this.infoCardEl.hidden = rows.length === 0;
    }
    renderDeviceRows(devices) {
        if (!this.rowsEl)
            return;
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
    td(text, mono = false) {
        return elem("td", mono ? "mono" : undefined, text);
    }
}
