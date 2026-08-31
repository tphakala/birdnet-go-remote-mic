import { api, ApiError } from "../lib/api.js";
import { store } from "../lib/store.js";
import { deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
import { triggerApplianceRestart } from "../components/restart-modal.js";
import { showToast } from "../components/toast.js";
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
    constructor() {
        this.tilesEl = document.getElementById("sys-tiles");
        this.infoEl = document.getElementById("sys-info");
        this.infoCardEl = document.getElementById("sys-info-card");
        this.rowsEl = document.getElementById("sys-device-rows");
        this.netCardEl = document.getElementById("sys-network-card");
        this.netActionsEl = document.getElementById("sys-network-actions");
        this.discoveryEl = document.getElementById("sys-discovery-enabled");
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
        });
        store.addEventListener("loaderror", (e) => {
            const detail = e.detail;
            if (detail.systemFailed)
                this.renderLoadError(detail.message);
        });
        this.bindNetwork();
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
        const rtsp = document.getElementById("sys-rtsp-listen");
        if (rtsp)
            rtsp.value = cfg.listen ?? "";
        const mgmt = document.getElementById("sys-mgmt-listen");
        if (mgmt)
            mgmt.value = cfg.management?.listen ?? "(default)";
        if (this.discoveryEl)
            this.discoveryEl.checked = cfg.discovery?.enabled ?? true;
        this.netDirty = false;
        if (this.netActionsEl)
            this.netActionsEl.hidden = true;
    }
    async saveNetwork() {
        try {
            await api.patchConfig({ discovery: { enabled: this.discoveryEl?.checked ?? true } });
            this.netDirty = false;
            if (this.netActionsEl)
                this.netActionsEl.hidden = true;
            await store.refreshConfig();
            showToast("Discovery setting applied.");
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
