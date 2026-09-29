import { formatRelative, formatUptime, ICON_VERSION, infoRow, orderChildren, part, setLoading, setText, svgIcon } from "../../lib/ui.ts";
import { infoRows, withPlaceholders, type InfoLabel, type InfoRow } from "../../lib/system-core.ts";
import type { ApplianceStatus, SystemInfo } from "../../lib/types.ts";

// System Information item icons (Lucide glyphs), one per label. The card has a Hardware
// group (physical machine), a Network group (links that are up) and a Software
// group (OS + build).
const ICON_PLATFORM =
  svgIcon('<polygon points="12 2 2 7 12 12 22 7 12 2"></polygon><polyline points="2 17 12 22 22 17"></polyline><polyline points="2 12 12 17 22 12"></polyline>', 14);
const ICON_CPU =
  svgIcon('<rect width="16" height="16" x="4" y="4" rx="2"></rect><rect width="6" height="6" x="9" y="9" rx="1"></rect><path d="M15 2v2"></path><path d="M15 20v2"></path><path d="M2 15h2"></path><path d="M2 9h2"></path><path d="M20 15h2"></path><path d="M20 9h2"></path><path d="M9 2v2"></path><path d="M9 20v2"></path>', 14);
const ICON_MEMORY =
  svgIcon('<rect x="3" y="8" width="18" height="8" rx="1"></rect><path d="M6 16v2"></path><path d="M10 16v2"></path><path d="M14 16v2"></path><path d="M18 16v2"></path>', 14);
const ICON_STORAGE =
  svgIcon('<line x1="22" x2="2" y1="12" y2="12"></line><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"></path><line x1="6" x2="6.01" y1="16" y2="16"></line><line x1="10" x2="10.01" y1="16" y2="16"></line>', 14);
const ICON_HOST =
  svgIcon('<rect width="20" height="8" x="2" y="2" rx="2" ry="2"></rect><rect width="20" height="8" x="2" y="14" rx="2" ry="2"></rect><line x1="6" x2="6.01" y1="6" y2="6"></line><line x1="6" x2="6.01" y1="18" y2="18"></line>', 14);
const ICON_OS =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><circle cx="12" cy="12" r="2"></circle>', 14);
const ICON_KERNEL =
  svgIcon('<polyline points="4 17 10 11 4 5"></polyline><line x1="12" x2="20" y1="19" y2="19"></line>', 14);
const ICON_CLOCK =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><polyline points="12 6 12 12 16 14"></polyline>', 14);
// Release rows in the Software column.
const ICON_RELEASE =
  svgIcon('<path d="M11 21.73a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73z"></path><path d="M12 22V12"></path><polyline points="3.29 7 12 12 20.71 7"></polyline><path d="m7.5 4.27 9 5.15"></path>', 14);
const ICON_HISTORY =
  svgIcon('<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"></path><path d="M3 3v5h5"></path><path d="M12 7v5l4 2"></path>', 14);

// Network group icons.
const ICON_NETWORK =
  svgIcon('<rect x="16" y="16" width="6" height="6" rx="1"></rect><rect x="2" y="16" width="6" height="6" rx="1"></rect><rect x="9" y="2" width="6" height="6" rx="1"></rect><path d="M5 16v-3a1 1 0 0 1 1-1h12a1 1 0 0 1 1 1v3"></path><path d="M12 12V8"></path>', 14);
const ICON_GLOBE =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20"></path><path d="M2 12h20"></path>', 14);
const ICON_HASH =
  svgIcon('<line x1="4" x2="20" y1="9" y2="9"></line><line x1="4" x2="20" y1="15" y2="15"></line><line x1="10" x2="8" y1="3" y2="21"></line><line x1="16" x2="14" y1="3" y2="21"></line>', 14);
const ICON_WIFI =
  svgIcon('<path d="M12 20h.01"></path><path d="M2 8.82a15 15 0 0 1 20 0"></path><path d="M5 12.859a10 10 0 0 1 14 0"></path><path d="M8.5 16.429a5 5 0 0 1 7 0"></path>', 14);
const ICON_SIGNAL =
  svgIcon('<path d="M2 20h.01"></path><path d="M7 20v-4"></path><path d="M12 20v-8"></path><path d="M17 20V8"></path><path d="M22 4v16"></path>', 14);
const ICON_RADIO =
  svgIcon('<path d="M4.9 19.1C1 15.2 1 8.8 4.9 4.9"></path><path d="M7.8 16.2c-2.3-2.3-2.3-6.1 0-8.5"></path><circle cx="12" cy="12" r="2"></circle><path d="M16.2 7.8c2.3 2.3 2.3 6.1 0 8.5"></path><path d="M19.1 4.9C23 8.8 23 15.1 19.1 19"></path>', 14);

const ICON_TIMER =
  svgIcon('<line x1="10" x2="14" y1="2" y2="2"></line><line x1="12" x2="15" y1="14" y2="11"></line><circle cx="12" cy="14" r="8"></circle>', 14);

const INFO_ICONS: Record<InfoLabel, string> = {
  Platform: ICON_PLATFORM,
  CPU: ICON_CPU,
  Memory: ICON_MEMORY,
  Storage: ICON_STORAGE,
  Connection: ICON_NETWORK,
  IPv4: ICON_GLOBE,
  IPv6: ICON_HASH,
  "Wi-Fi Network": ICON_WIFI,
  Signal: ICON_SIGNAL,
  Band: ICON_RADIO,
  Hostname: ICON_HOST,
  OS: ICON_OS,
  Kernel: ICON_KERNEL,
  Version: ICON_VERSION,
  "Latest Release": ICON_RELEASE,
  "Last Check": ICON_HISTORY,
  "Next Check": ICON_TIMER,
  Uptime: ICON_CLOCK,
};

// InfoCard is the System Information card's label and value grid
// (#sys-info-card): the Hardware, Network and Software groups. The update status and
// actions in the same card belong to UpdatePanel.
export class InfoCard {
  private readonly cardEl: HTMLElement | null;
  private readonly hwEl: HTMLElement | null;
  private readonly netEl: HTMLElement | null;
  private readonly swEl: HTMLElement | null;
  // Rows keyed by InfoRow.key. Built once, updated in place, added and removed
  // only when the present set changes.
  private readonly rows = new Map<string, { dt: HTMLElement; dd: HTMLElement }>();

  constructor(root: HTMLElement | null) {
    this.cardEl = root;
    this.hwEl = part(root, "sys-info-hw");
    this.netEl = part(root, "sys-info-net");
    this.swEl = part(root, "sys-info-sw");
    // Until both reads are in, every row is laid out with an empty value, so
    // each read fills rows in place instead of growing the card. A failed read
    // leaves the card waiting, not collapsed: polling retries it.
    if (root) setLoading(root, true);
    this.render(null, null);
  }

  // render fills the grid, diffed: rows are keyed by key, values updated in
  // place, and dt/dd pairs added, removed and ordered only on change rather
  // than clearing the grid every poll.
  public render(sys: SystemInfo | null, st: ApplianceStatus | null): void {
    const hw = this.hwEl;
    const net = this.netEl;
    const sw = this.swEl;
    if (!hw || !net || !sw) return;
    const loading = !sys || !st;

    const read = infoRows(sys, st, Date.now(), formatUptime, formatRelative);
    const rows = loading ? withPlaceholders(read) : read;
    const want = new Set(rows.map((r) => r.key));
    for (const [key, pair] of this.rows) {
      if (!want.has(key)) { pair.dt.remove(); pair.dd.remove(); this.rows.delete(key); }
    }

    // Each group is ordered within its own grid, dt then dd per row.
    const order: Record<InfoRow["group"], HTMLElement[]> = { hw: [], net: [], sw: [] };
    for (const r of rows) {
      let pair = this.rows.get(r.key);
      if (!pair) {
        pair = infoRow(r.label, r.value, { icon: INFO_ICONS[r.label], mono: true });
        this.rows.set(r.key, pair);
      } else {
        setText(pair.dd, r.value);
      }
      order[r.group].push(pair.dt, pair.dd);
    }
    orderChildren(hw, order.hw);
    orderChildren(net, order.net);
    orderChildren(sw, order.sw);
    if (this.cardEl && !loading) setLoading(this.cardEl, false);
  }
}
