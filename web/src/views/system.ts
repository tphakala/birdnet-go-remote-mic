import { api, ApiError } from "../lib/api.js";
import { store } from "../lib/store.js";
import { triggerApplianceRestart } from "../components/restart-modal.js";
import { showToast } from "../components/toast.js";
import type { ApplianceStatus, Config, Device, LoadError, SystemInfo } from "../lib/types.js";

function elem(tag: string, className?: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

function formatUptime(seconds: number): string {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h ${String(m).padStart(2, "0")}m`;
  if (h > 0) return `${h}h ${String(m).padStart(2, "0")}m`;
  return `${m}m`;
}

export class SystemView {
  private tilesEl: HTMLElement | null;
  private infoEl: HTMLElement | null;
  private infoCardEl: HTMLElement | null;
  private rowsEl: HTMLElement | null;
  private system: SystemInfo | null = null;
  private status: ApplianceStatus | null = null;

  private netCardEl: HTMLElement | null;
  private netActionsEl: HTMLElement | null;
  private discoveryEl: HTMLInputElement | null;
  private netDirty = false;

  constructor() {
    this.tilesEl = document.getElementById("sys-tiles");
    this.infoEl = document.getElementById("sys-info");
    this.infoCardEl = document.getElementById("sys-info-card");
    this.rowsEl = document.getElementById("sys-device-rows");
    this.netCardEl = document.getElementById("sys-network-card");
    this.netActionsEl = document.getElementById("sys-network-actions");
    this.discoveryEl = document.getElementById("sys-discovery-enabled") as HTMLInputElement | null;
    const btn = document.getElementById("btn-sys-restart") as HTMLButtonElement | null;
    if (btn) btn.addEventListener("click", () => triggerApplianceRestart());

    store.addEventListener("system", (e: Event) => {
      this.system = (e as CustomEvent<SystemInfo>).detail;
      this.renderTiles();
      this.renderInfo();
    });
    store.addEventListener("status", (e: Event) => {
      this.status = (e as CustomEvent<ApplianceStatus>).detail;
      this.renderTiles();
      this.renderInfo();
    });
    store.addEventListener("devices", (e: Event) => {
      this.renderDeviceRows((e as CustomEvent<Device[]>).detail);
    });
    store.addEventListener("config", (e: Event) => {
      const cfg = (e as CustomEvent<Config>).detail;
      if (!this.netDirty && cfg) this.populateNetwork(cfg);
    });
    store.addEventListener("loaderror", (e: Event) => {
      const detail = (e as CustomEvent<LoadError>).detail;
      if (detail.systemFailed) this.renderLoadError(detail.message);
    });
    this.bindNetwork();
  }

  // renderLoadError swaps the telemetry placeholder for the failure cause and a
  // Retry button so the system view is not stuck loading when /system is
  // unreachable. A successful retry re-renders via the system event.
  private renderLoadError(message: string): void {
    if (!this.tilesEl) return;
    this.tilesEl.textContent = "";
    const p = elem("p", "cfg-empty", `${message} `);
    const retry = elem("button", "btn btn-secondary", "Retry");
    retry.setAttribute("type", "button");
    retry.addEventListener("click", () => {
      if (this.tilesEl) this.tilesEl.textContent = "";
      this.tilesEl?.appendChild(elem("p", "cfg-empty", "Loading system telemetry..."));
      void store.retry();
    });
    p.appendChild(retry);
    this.tilesEl.appendChild(p);
  }

  private bindNetwork(): void {
    if (this.discoveryEl) {
      this.discoveryEl.addEventListener("change", () => {
        this.netDirty = true;
        if (this.netActionsEl) this.netActionsEl.hidden = false;
      });
    }
    document.getElementById("btn-network-save")?.addEventListener("click", () => this.saveNetwork());
    document.getElementById("btn-network-discard")?.addEventListener("click", () => {
      const cfg = store.getState().config;
      if (cfg) this.populateNetwork(cfg);
    });
  }

  private populateNetwork(cfg: Config): void {
    if (this.netCardEl) this.netCardEl.hidden = false;
    const rtsp = document.getElementById("sys-rtsp-listen") as HTMLInputElement | null;
    if (rtsp) rtsp.value = cfg.listen ?? "";
    const mgmt = document.getElementById("sys-mgmt-listen") as HTMLInputElement | null;
    if (mgmt) mgmt.value = cfg.management?.listen ?? "(default)";
    if (this.discoveryEl) this.discoveryEl.checked = cfg.discovery?.enabled ?? true;
    this.netDirty = false;
    if (this.netActionsEl) this.netActionsEl.hidden = true;
  }

  private async saveNetwork(): Promise<void> {
    try {
      await api.patchConfig({ discovery: { enabled: this.discoveryEl?.checked ?? true } });
      this.netDirty = false;
      if (this.netActionsEl) this.netActionsEl.hidden = true;
      await store.refreshConfig();
      showToast("Discovery setting saved. Restart the appliance to apply.");
    } catch (err: unknown) {
      const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
      showToast(`Save failed: ${msg}`, "error");
    }
  }

  private tile(label: string, sub: string, value: string, unit?: string, barPct?: number): HTMLElement {
    const tile = elem("div", "system-tile");
    const header = elem("div", "tile-header");
    header.appendChild(elem("span", undefined, label));
    if (sub) header.appendChild(elem("span", "mono", sub));
    tile.appendChild(header);

    const val = elem("div", "tile-value mono");
    val.appendChild(elem("span", undefined, value));
    if (unit) val.appendChild(elem("span", "telemetry-unit", unit));
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
  private renderTiles(): void {
    if (!this.tilesEl) return;
    const sys = this.system;
    if (!sys) return;

    this.tilesEl.textContent = "";

    const cores = sys.cpuCores > 0 ? `${sys.cpuCores} Cores` : "";
    this.tilesEl.appendChild(this.tile("CPU Utilization", cores,
      sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a", "%",
      sys.cpuPercent ?? 0));

    if (sys.memTotalBytes > 0) {
      const pct = (sys.memUsedBytes / sys.memTotalBytes) * 100;
      const usedMb = Math.round(sys.memUsedBytes / 1048576);
      const totalMb = Math.round(sys.memTotalBytes / 1048576);
      this.tilesEl.appendChild(this.tile("Memory", `${totalMb} MB Total`,
        String(usedMb), "MB used", pct));
    }

    this.tilesEl.appendChild(this.tile("SoC Temperature", "",
      sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a", "deg C",
      sys.tempCelsius !== undefined ? (sys.tempCelsius / 85) * 100 : undefined));

    if (sys.diskTotalBytes > 0) {
      const pct = (sys.diskUsedBytes / sys.diskTotalBytes) * 100;
      const usedGb = (sys.diskUsedBytes / 1073741824).toFixed(1);
      const totalGb = (sys.diskTotalBytes / 1073741824).toFixed(1);
      this.tilesEl.appendChild(this.tile("Disk", `${totalGb} GB Total`,
        usedGb, "GB used", pct));
    }
  }

  private renderInfo(): void {
    if (!this.infoEl) return;
    const sys = this.system;
    const st = this.status;
    if (!sys && !st) return;

    const rows: Array<[string, string]> = [];
    if (sys) {
      rows.push(["Hostname", sys.hostname || "-"]);
      rows.push(["Platform", sys.platform || "-"]);
      if (sys.cpuModel) rows.push(["CPU", `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`]);
      if (sys.os) rows.push(["OS", sys.os]);
      if (sys.kernel) rows.push(["Kernel", sys.kernel]);
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
    if (this.infoCardEl) this.infoCardEl.hidden = rows.length === 0;
  }

  private renderDeviceRows(devices: Device[]): void {
    if (!this.rowsEl) return;
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
      tr.appendChild(this.td(`${d.mode === "pcm" ? "PCM L16" : "OPUS"} ${rate.toLocaleString("en-US")} Hz`, true));
      tr.appendChild(this.td(d.clientConnected ? "Connected" : "-", true));

      const stateTd = document.createElement("td");
      let cls = "status-badge ok";
      let label = "Serving";
      if (d.state === "skipped") { cls = "status-badge crit"; label = "Skipped"; }
      else if (d.state === "failed") { cls = "status-badge crit"; label = "Failed"; }
      stateTd.appendChild(elem("span", cls, label));
      tr.appendChild(stateTd);

      this.rowsEl.appendChild(tr);
    }
  }

  private td(text: string, mono = false): HTMLElement {
    return elem("td", mono ? "mono" : undefined, text);
  }
}
