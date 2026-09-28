import { store } from "../../lib/store.ts";
import { elem, orderChildren, renderLoadError, setHidden, setText } from "../../lib/ui.ts";
import { tileSpecs, type TileSpec } from "../../lib/system-core.ts";
import type { SystemInfo } from "../../lib/types.ts";

// TileRefs are the stable nodes a tile reuses across polls, so render updates
// in place rather than rebuilding the grid.
interface TileRefs {
  tile: HTMLElement;
  sub: HTMLElement;
  value: HTMLElement;
  unit: HTMLElement;
  bar: HTMLElement;
  barFill: HTMLElement;
}

// TelemetryTiles is the resource-gauge grid at the top of the System view
// (#sys-tiles). It holds only the live gauges; host and appliance facts live
// in the System Information card below.
export class TelemetryTiles {
  private readonly gridEl: HTMLElement | null;
  // Tiles keyed by tile key. Built once, updated in place, added and removed
  // only when the present set changes.
  private readonly tiles = new Map<string, TileRefs>();

  constructor(grid: HTMLElement | null) {
    this.gridEl = grid;
  }

  // render draws the gauges with the diffed convention: tiles are keyed by a
  // stable key, updated in place, and only added, removed or reordered when the
  // present set changes.
  public render(sys: SystemInfo): void {
    const grid = this.gridEl;
    if (!grid) return;

    // Remove a load-error placeholder loadError may have left in the grid, so a
    // recovered poll does not strand it among the gauges: the diffed pass below
    // tracks only tile nodes, not this foreign child.
    grid.querySelector(":scope > .cfg-empty")?.remove();

    const specs = tileSpecs(sys);
    const want = new Set(specs.map((s) => s.key));
    for (const [key, refs] of this.tiles) {
      if (!want.has(key)) { refs.tile.remove(); this.tiles.delete(key); }
    }

    const tiles: HTMLElement[] = [];
    for (const spec of specs) {
      let refs = this.tiles.get(spec.key);
      if (!refs) { refs = this.build(spec.label); this.tiles.set(spec.key, refs); }
      this.update(refs, spec);
      tiles.push(refs.tile);
    }
    orderChildren(grid, tiles);
  }

  // loadError swaps the telemetry placeholder for the failure cause and a
  // Retry button so the System view is not stuck loading when /system is
  // unreachable. A successful retry re-renders via the system event.
  public loadError(message: string): void {
    if (!this.gridEl) return;
    this.gridEl.textContent = "";
    // The tiles were just detached, so drop their stale refs; otherwise a later
    // render would reuse detached nodes and the diffed pass would not rebuild.
    this.tiles.clear();
    const p = elem("p", "cfg-empty");
    this.gridEl.appendChild(p);
    renderLoadError(p, message, "Loading system telemetry...", () => void store.retry());
  }

  // build creates one resource-gauge tile with stable inner nodes (the sub,
  // value, unit and progress bar are always present and toggled/updated, never
  // rebuilt), so render can update it in place across polls. The label is
  // fixed per tile key, so it is written once here.
  private build(label: string): TileRefs {
    const tile = elem("div", "system-tile");
    const header = elem("div", "tile-header");
    const sub = elem("span", "mono");
    header.append(elem("span", undefined, label), sub);
    tile.appendChild(header);

    const val = elem("div", "tile-value mono");
    const value = elem("span");
    const unit = elem("span", "telemetry-unit");
    val.append(value, unit);
    tile.appendChild(val);

    const bar = elem("div", "progress-bar-bg");
    const barFill = elem("div", "progress-bar-fill");
    bar.appendChild(barFill);
    tile.appendChild(bar);

    return { tile, sub, value, unit, bar, barFill };
  }

  private update(refs: TileRefs, spec: TileSpec): void {
    setText(refs.sub, spec.sub);
    setHidden(refs.sub, !spec.sub);
    setText(refs.value, spec.value);
    setText(refs.unit, spec.unit);
    setHidden(refs.unit, !spec.unit);
    if (spec.barPct === undefined) {
      setHidden(refs.bar, true);
    } else {
      setHidden(refs.bar, false);
      const w = `${Math.min(100, Math.max(0, spec.barPct)).toFixed(1)}%`;
      if (refs.barFill.style.width !== w) refs.barFill.style.width = w;
    }
  }
}
