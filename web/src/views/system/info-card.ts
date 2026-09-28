import { elem, formatRelative, formatUptime, ICON_VERSION, iconSpan, orderChildren, part, setLoading, setText, svgIcon } from "../../lib/ui.ts";
import { infoRows, withPlaceholders, type InfoLabel } from "../../lib/system-core.ts";
import type { ApplianceStatus, SystemInfo } from "../../lib/types.ts";

// System Information item icons (Lucide glyphs), one per label. The card splits
// into a Hardware column (physical machine) and a Software column (OS + build).
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

const INFO_ICONS: Record<InfoLabel, string> = {
  Platform: ICON_PLATFORM,
  CPU: ICON_CPU,
  Memory: ICON_MEMORY,
  Storage: ICON_STORAGE,
  Hostname: ICON_HOST,
  OS: ICON_OS,
  Kernel: ICON_KERNEL,
  Version: ICON_VERSION,
  "Latest Release": ICON_RELEASE,
  "Last Check": ICON_HISTORY,
  Uptime: ICON_CLOCK,
};

// InfoCard is the System Information card's label and value grid
// (#sys-info-card): the Hardware and Software columns. The update status and
// actions in the same card belong to UpdatePanel.
export class InfoCard {
  private readonly cardEl: HTMLElement | null;
  private readonly hwEl: HTMLElement | null;
  private readonly swEl: HTMLElement | null;
  // Rows keyed by label. Built once, updated in place, added and removed only
  // when the present set changes.
  private readonly rows = new Map<InfoLabel, { dt: HTMLElement; dd: HTMLElement }>();

  constructor(root: HTMLElement | null) {
    this.cardEl = root;
    this.hwEl = part(root, "sys-info-hw");
    this.swEl = part(root, "sys-info-sw");
    // Until both reads are in, every row is laid out with an empty value, so
    // each read fills rows in place instead of growing the card. A failed read
    // leaves the card waiting, not collapsed: polling retries it.
    if (root) setLoading(root, true);
    this.render(null, null);
  }

  // render fills the grid, diffed: rows are keyed by label, values updated in
  // place, and dt/dd pairs added, removed and ordered only on change rather
  // than clearing the grid every poll.
  public render(sys: SystemInfo | null, st: ApplianceStatus | null): void {
    const hw = this.hwEl;
    const sw = this.swEl;
    if (!hw || !sw) return;
    const loading = !sys || !st;

    const read = infoRows(sys, st, Date.now(), formatUptime, formatRelative);
    const rows = loading ? withPlaceholders(read) : read;
    const want = new Set(rows.map((r) => r.label));
    for (const [key, pair] of this.rows) {
      if (!want.has(key)) { pair.dt.remove(); pair.dd.remove(); this.rows.delete(key); }
    }

    // Each column is ordered within its own grid, dt then dd per row.
    const order: Record<"hw" | "sw", HTMLElement[]> = { hw: [], sw: [] };
    for (const r of rows) {
      let pair = this.rows.get(r.label);
      if (!pair) {
        const dt = elem("dt", "info-key");
        dt.append(iconSpan(INFO_ICONS[r.label], "info-key-icon"), document.createTextNode(r.label));
        pair = { dt, dd: elem("dd", "info-val mono", r.value) };
        this.rows.set(r.label, pair);
      } else {
        setText(pair.dd, r.value);
      }
      order[r.group].push(pair.dt, pair.dd);
    }
    orderChildren(hw, order.hw);
    orderChildren(sw, order.sw);
    if (this.cardEl && !loading) setLoading(this.cardEl, false);
  }
}
