import { store } from "../../lib/store.ts";
import { clearLoadError, h, renderLoadError, setHidden, setLoading, setText } from "../../lib/ui.ts";
import { KeyedList } from "../../lib/keyed-list.ts";
import { TILE_SLOTS, tileSpecs, type TileSpec } from "../../lib/system-core.ts";
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

// TileSource is what a tile is drawn from: a read's spec, or a usual slot laid
// out before the first read (a slot has no values yet, so it is built but not
// updated).
type TileSource = TileSpec | { key: string; label: string };

// TelemetryTiles is the resource-gauge grid at the top of the System view
// (#sys-tiles). It holds only the live gauges; host and appliance facts live
// in the System Information card below.
export class TelemetryTiles {
  private readonly gridEl: HTMLElement | null;
  // Tiles keyed by tile key. Built once, updated in place, added and removed
  // only when the present set changes. Null without a grid.
  private readonly tiles: KeyedList<TileSource, TileRefs> | null;

  constructor(grid: HTMLElement | null) {
    this.gridEl = grid;
    this.tiles = null;
    if (!grid) return;
    this.tiles = new KeyedList<TileSource, TileRefs>(grid, {
      key: (s) => s.key,
      create: (s) => this.build(s.label),
      nodes: (r) => [r.tile],
      update: (r, s) => {
        if ("value" in s) this.update(r, s);
      },
    });
    // Lay the usual tiles out before the first read, with empty values, so the
    // read fills them in place (they are keyed as the read's tiles are)
    // instead of growing the page.
    this.tiles.sync(TILE_SLOTS);
    setLoading(grid, true);
  }

  // render draws the gauges with the diffed convention: tiles are keyed by a
  // stable key, updated in place, and only added, removed or reordered when the
  // present set changes.
  public render(sys: SystemInfo): void {
    const grid = this.gridEl;
    if (!grid || !this.tiles) return;
    setLoading(grid, false);

    // Remove a load-error placeholder loadError may have left in the grid, so a
    // recovered poll does not strand it among the gauges: the diffed pass below
    // tracks only tile nodes, not this foreign child. Retry may hold focus, so
    // clearLoadError parks it on the view first.
    const stale = grid.querySelector<HTMLElement>(":scope > .cfg-empty");
    if (stale) {
      clearLoadError(stale);
      stale.remove();
    }

    this.tiles.sync(tileSpecs(sys));
  }

  // loadError swaps the telemetry placeholder for the failure cause and a
  // Retry button so the System view is not stuck loading when /system is
  // unreachable. A successful retry re-renders via the system event.
  public loadError(message: string): void {
    if (!this.gridEl || !this.tiles) return;
    // Retry must be usable.
    setLoading(this.gridEl, false);
    this.gridEl.textContent = "";
    // The tiles were just detached, so clear only forgets them; otherwise a
    // later render would reuse detached nodes and the list would not rebuild.
    this.tiles.clear();
    const p = h("p", { class: "cfg-empty" });
    this.gridEl.appendChild(p);
    renderLoadError(p, message, "Loading system telemetry...", () => void store.retry());
  }

  // build creates one resource-gauge tile with stable inner nodes (the sub,
  // value, unit and progress bar are always present and toggled/updated, never
  // rebuilt), so render can update it in place across polls. The label is
  // fixed per tile key, so it is written once here.
  private build(label: string): TileRefs {
    const sub = h("span", { class: "mono" });
    const value = h("span");
    const unit = h("span", { class: "telemetry-unit" });
    const barFill = h("div", { class: "progress-bar-fill" });
    const bar = h("div", { class: "progress-bar-bg" }, barFill);
    const tile = h(
      "div",
      { class: "system-tile" },
      h("div", { class: "tile-header" }, h("span", label), sub),
      h("div", { class: "tile-value mono" }, value, unit),
      bar,
    );

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
