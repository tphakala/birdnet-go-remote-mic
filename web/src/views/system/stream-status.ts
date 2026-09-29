import { deviceStateBadge, h, orderChildren, setLoading, setText } from "../../lib/ui.ts";
import { deviceIdTitle } from "../../lib/text.ts";
import { clientSummary, streamFormats, streamSummary } from "../../lib/dashboard-core.ts";
import type { Config, Device } from "../../lib/types.ts";

// DeviceRowRefs are the stable cells of one Stream Status table row, reused
// across polls so render updates text in place instead of rebuilding every
// row each tick.
interface DeviceRowRefs {
  tr: HTMLElement;
  name: HTMLElement;
  alsa: HTMLElement;
  path: HTMLElement;
  codec: HTMLElement;
  client: HTMLElement;
  stateSpan: HTMLElement;
}

// StreamStatus is the Stream Status table: one row per device from the devices
// read, with its capture address, stream paths, codec and rate, clients and
// state.
export class StreamStatus {
  private readonly bodyEl: HTMLElement | null;
  // Rows keyed by the immutable device id.
  private readonly rows = new Map<string, DeviceRowRefs>();
  // The one full-width row shown instead of device rows: loading, then "No
  // devices configured." once a read lists none. It holds the table's first
  // row height from the first paint.
  private readonly messageRow: HTMLElement;
  private readonly messageCell: HTMLElement;
  // The Stream Status card, busy until the first devices read, or until
  // loadFailed (see there for when that runs). The row count is unknown until
  // then, so a multi-device table still grows once when the devices arrive.
  private readonly cardEl: HTMLElement | null;
  private loaded = false;

  constructor(body: HTMLElement | null) {
    this.bodyEl = body;
    this.cardEl = body?.closest<HTMLElement>(".client-card") ?? null;
    this.messageCell = h("td", { colspan: 6 }, "Loading streams...");
    this.messageRow = h("tr", this.messageCell);
    body?.replaceChildren(this.messageRow);
    if (this.cardEl) setLoading(this.cardEl, true);
  }

  // devices renders a devices read.
  public devices(devices: Device[], cfg: Config | null): void {
    this.loaded = true;
    if (this.cardEl) setLoading(this.cardEl, false);
    this.render(devices, cfg);
  }

  // config renders again from a config read: the table lists every configured
  // stream, which only the config holds. Config fires on every poll, but the
  // rows write only what changed. Only once the devices loaded, or a config
  // arriving first would flash "No devices configured".
  public config(cfg: Config, devices: Device[]): void {
    if (this.loaded) this.render(devices, cfg);
  }

  // loadFailed says so in place of the loading row; a later read that
  // succeeds fills the table. SystemView calls it when the first devices read
  // failed (devicesFailed), whether or not /status answered.
  public loadFailed(): void {
    if (this.loaded) return;
    setText(this.messageCell, "Stream status could not be loaded. Retrying shortly.");
    if (this.cardEl) setLoading(this.cardEl, false);
  }

  // render fills the table, diffed: rows are keyed by the immutable device id,
  // cells updated in place, and rows added, removed and ordered only on change
  // rather than rebuilding the whole tbody every poll.
  private render(devices: Device[], cfg: Config | null): void {
    if (!this.bodyEl) return;
    const body = this.bodyEl;

    if (devices.length === 0) {
      for (const r of this.rows.values()) r.tr.remove();
      this.rows.clear();
      setText(this.messageCell, "No devices configured.");
      if (this.messageRow.parentNode !== body) body.appendChild(this.messageRow);
      return;
    }
    // Non-empty: drop the message row if it is showing.
    if (this.messageRow.parentNode) this.messageRow.remove();

    const want = new Set(devices.map((d) => d.device));
    for (const [id, r] of this.rows) {
      if (!want.has(id)) { r.tr.remove(); this.rows.delete(id); }
    }

    const rows: HTMLElement[] = [];
    for (const d of devices) {
      let r = this.rows.get(d.device);
      if (!r) { r = this.buildRow(); this.rows.set(d.device, r); }
      this.updateRow(r, d, cfg);
      rows.push(r.tr);
    }
    orderChildren(body, rows);
  }

  private buildRow(): DeviceRowRefs {
    const name = h("td");
    const alsa = h("td", { class: "mono" });
    const path = h("td", { class: "mono stream-paths" });
    const codec = h("td", { class: "mono stream-paths" });
    const client = h("td", { class: "mono" });
    const stateSpan = h("span");
    const tr = h("tr", name, alsa, path, codec, client, h("td", stateSpan));
    return { tr, name, alsa, path, codec, client, stateSpan };
  }

  private updateRow(r: DeviceRowRefs, d: Device, cfg: Config | null): void {
    setText(r.name, d.name);
    // The current ALSA address; when the id resolved to no single present device
    // it is absent, so a serving card-index device (container fallback) shows its
    // configured id and anything else shows "-". The persisted id is long and
    // goes in the tooltip.
    setText(r.alsa, d.hwAddr ?? (d.state === "serving" ? d.device : "-"));
    const alsaTitle = deviceIdTitle(d.device);
    if (r.alsa.title !== alsaTitle) r.alsa.title = alsaTitle;
    // By the device id, which a rename does not change.
    const deviceCfg = cfg?.devices.find((c) => c.device === d.device);
    const streams = streamSummary(d, deviceCfg);
    // One path per line (the cell keeps the line breaks), in stream order.
    setText(r.path, streams.paths.join("\n"));
    // One line per stream, in path order: the stream's own format, not the
    // hardware capture depth (the Dashboard card shows that).
    setText(r.codec, streamFormats(d, deviceCfg).join("\n"));
    setText(r.client, clientSummary(streams));
    const badge = deviceStateBadge(d.state);
    if (r.stateSpan.className !== badge.cls) r.stateSpan.className = badge.cls;
    setText(r.stateSpan, badge.label);
  }
}
