import { REDUCED_MOTION_QUERY } from "../lib/ui.ts";
import { FLOOR_DB, FrameScheduler, levelBand, levelRatio, MeterController, WAITING_READOUT, WAITING_TITLE } from "../lib/meter-core.ts";

// The 2D context cannot read CSS variables, so the theme is tracked here: a cheap
// attribute cache refreshed whenever html[data-theme] changes (initTheme's
// load-time correction, a pick in the header theme menu, an OS change in System
// mode, or a mode chosen in another tab). The lit segment
// colours (green/amber/red) read on both grounds and stay fixed; only the track
// and unlit-segment tints need to swap, since white-on-light was invisible.
// Shared by every meter instance.
let meterLightTheme = document.documentElement.getAttribute("data-theme") === "light";
// Every meter not yet destroyed. A settled meter draws nothing until its level
// changes, so a theme change asks each one to repaint in the new tints.
const liveMeters = new Set<VUMeter>();
try {
  new MutationObserver(() => {
    meterLightTheme = document.documentElement.getAttribute("data-theme") === "light";
    for (const m of liveMeters) m.redraw();
  }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
} catch {
  /* no MutationObserver: keep the theme detected at load */
}

// The meter is 36 segments over the scale from FLOOR_DB to 0 dBFS, each lit in
// the colour of its level band (levelBand): green, amber, red. The needle
// takes the same band's colour, opaque.
const SEGMENTS = 36;
const SEGMENT_PALETTE = ["rgba(16, 185, 129, 0.85)", "rgba(245, 158, 11, 0.9)", "rgba(239, 68, 68, 0.95)"] as const;
const NEEDLE_PALETTE = ["#10b981", "#f59e0b", "#ef4444"] as const;
const SEGMENT_COLORS: readonly string[] = Array.from({ length: SEGMENTS }, (_, i) =>
  SEGMENT_PALETTE[levelBand(FLOOR_DB - (i / SEGMENTS) * FLOOR_DB)]);

// The viewer's motion preference, followed live: every live meter switches
// between the gliding needle and the per-event steps when it changes.
const reducedMotionQuery: MediaQueryList | null = (() => {
  try {
    return window.matchMedia?.(REDUCED_MOTION_QUERY) ?? null;
  } catch {
    return null;
  }
})();
reducedMotionQuery?.addEventListener?.("change", (e) => {
  for (const m of liveMeters) m.setReducedMotion(e.matches);
});

// A meter scrolled out of view (a card off a phone screen, a row inside a
// scrolled console) skips its canvas paint until it comes back, so it does
// not repaint at the display rate where nobody sees it; its readout stays
// current. One observer serves every meter; without IntersectionObserver
// every meter counts as in view.
const meterByCanvas = new WeakMap<Element, VUMeter>();
const viewObserver: IntersectionObserver | null = (() => {
  try {
    return new IntersectionObserver((entries) => {
      for (const e of entries) meterByCanvas.get(e.target)?.setOffscreen(!e.isIntersecting);
    });
  } catch {
    return null;
  }
})();

// meterFrames is the one frame loop every meter draws on. It runs only while a
// meter has something to animate, and the dashboard suspends it while another
// view shows.
export const meterFrames = new FrameScheduler({
  request: (cb) => requestAnimationFrame(cb),
  cancel: (handle) => cancelAnimationFrame(handle),
  timers: globalThis,
});

// VUMeter is one channel's meter on the dashboard: the canvas, the dB readout
// and the clip latch button. Its state and sequencing live in a
// MeterController (lib/meter-core.ts); this class paints and wires the DOM
// (the clip button, a restored canvas context, theme and motion changes).
export class VUMeter {
  private canvas: HTMLCanvasElement;
  // Null when the browser gives no 2D context: the meter then paints nothing,
  // but its readout and clip latch still work and the rest of the dashboard
  // renders.
  private ctx: CanvasRenderingContext2D | null;
  private peakValEl: HTMLElement | null;
  private clipEl: HTMLElement | null;
  private readonly controller: MeterController;

  constructor(
    canvas: HTMLCanvasElement,
    peakValEl?: HTMLElement | null,
    clipEl?: HTMLElement | null
  ) {
    this.canvas = canvas;
    this.ctx = this.canvas.getContext("2d");
    if (!this.ctx) console.warn("meter: canvas 2D context is not available; the bar and needle will not draw");
    // A browser may drop a canvas's pixels (context loss) and hand it back
    // blank; a settled meter draws nothing new on its own, so repaint.
    this.canvas.addEventListener("contextrestored", () => this.redraw());
    this.peakValEl = peakValEl ?? null;
    this.clipEl = clipEl ?? null;

    if (this.clipEl) {
      this.clipEl.addEventListener("click", () => this.clearClip());
    }

    liveMeters.add(this);
    this.controller = new MeterController(
      {
        draw: (rms, peak) => this.paint(rms, peak),
        showClip: (clipped) => this.showClip(clipped),
        showReadout: (text) => {
          if (!this.peakValEl) return;
          this.peakValEl.textContent = text;
          // Say what "--" means to a pointer user. Only a pointer gets it, by
          // choice: the readout is aria-hidden with the canvas (MeterConsole
          // in web/src/views/dashboard/meter-console.ts hides both), so "--" is
          // visual only, and the stream state reaches everyone through the
          // connection indicator instead.
          // Written on change only: the text changes up to ten times a second.
          const title = text === WAITING_READOUT ? WAITING_TITLE : "";
          if (this.peakValEl.title !== title) this.peakValEl.title = title;
        },
      },
      meterFrames,
      {
        reducedMotion: reducedMotionQuery?.matches ?? false,
        now: () => performance.now(),
      },
    );
    meterByCanvas.set(this.canvas, this);
    viewObserver?.observe(this.canvas);
  }

  public setLevels(rms: number, peak: number, clipped: boolean = false): void {
    this.controller.setLevels(rms, peak, clipped);
  }

  public clearClip(): void {
    this.controller.clearClip();
  }

  public pause(): void {
    this.controller.pause();
  }

  public resume(): void {
    this.controller.resume();
  }

  public redraw(): void {
    this.controller.redraw();
  }

  public clearLevels(): void {
    this.controller.clearLevels();
  }

  public setReducedMotion(reduced: boolean): void {
    this.controller.setReducedMotion(reduced);
  }

  public destroy(): void {
    liveMeters.delete(this);
    viewObserver?.unobserve(this.canvas);
    this.controller.destroy();
  }

  public setOffscreen(offscreen: boolean): void {
    this.controller.setOffscreen(offscreen);
  }

  // showClip shows the latch on the clip button: the lit style, and
  // aria-pressed so a screen reader hears whether it is latched (pressing it
  // clears the latch, which releases the button).
  private showClip(clipped: boolean): void {
    const el = this.clipEl;
    if (!el) return;
    el.classList.toggle("clipped", clipped);
    el.setAttribute("aria-pressed", String(clipped));
  }

  private paint(rmsDb: number, peakDb: number): void {
    const ctx = this.ctx;
    if (!ctx) return;
    const w = this.canvas.width;
    const h = this.canvas.height;

    ctx.clearRect(0, 0, w, h);

    // Track and unlit-segment tints swap with the theme so the "off" lights stay
    // visible: a dark wash on the light meter ground, a light wash on the dark.
    const trackBg = meterLightTheme ? "rgba(0, 0, 0, 0.05)" : "rgba(255, 255, 255, 0.04)";
    const unlitSeg = meterLightTheme ? "rgba(0, 0, 0, 0.10)" : "rgba(255, 255, 255, 0.06)";

    // Background track
    ctx.fillStyle = trackBg;
    ctx.fillRect(0, 0, w, h);

    const gap = 2;
    const segWidth = (w - (SEGMENTS - 1) * gap) / SEGMENTS;
    const activeSegments = Math.round(levelRatio(rmsDb) * SEGMENTS);

    // Neighbouring segments mostly share a colour, so fillStyle is set only
    // when it changes. The last value is local to this paint, so nothing
    // assumes the context kept it from an earlier one.
    let fill = "";
    for (let i = 0; i < SEGMENTS; i++) {
      const color = i < activeSegments ? (SEGMENT_COLORS[i] ?? unlitSeg) : unlitSeg;
      if (color !== fill) {
        ctx.fillStyle = color;
        fill = color;
      }
      ctx.fillRect(i * (segWidth + gap), 0, segWidth, h);
    }

    // Peak hold needle
    const needleRatio = levelRatio(peakDb);
    if (needleRatio > 0.02) {
      const peakX = Math.min(w - 2, Math.max(0, needleRatio * w - 1.5));
      ctx.fillStyle = NEEDLE_PALETTE[levelBand(peakDb)];
      ctx.fillRect(peakX, 0, 2, h);
    }
  }
}
