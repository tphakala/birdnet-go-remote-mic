import { FLOOR_DB, FrameScheduler, levelRatio, MeterController } from "../lib/meter-core.ts";

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

// meterFrames is the one frame loop every meter draws on. It runs only while a
// meter has something to animate, and the dashboard suspends it while another
// view shows.
export const meterFrames = new FrameScheduler({
  request: (cb) => requestAnimationFrame(cb),
  cancel: (handle) => cancelAnimationFrame(handle),
});

// VUMeter is one channel's meter on the dashboard: the canvas, the dB readout
// and the clip latch button. Its state and sequencing live in a
// MeterController (lib/meter-core.ts); this class only paints.
export class VUMeter {
  private canvas: HTMLCanvasElement;
  private ctx: CanvasRenderingContext2D;
  private peakValEl: HTMLElement | null;
  private clipEl: HTMLElement | null;
  private readonly controller: MeterController;

  constructor(
    canvas: HTMLCanvasElement,
    peakValEl?: HTMLElement | null,
    clipEl?: HTMLElement | null
  ) {
    this.canvas = canvas;
    const context = this.canvas.getContext("2d");
    if (!context) {
      throw new Error("Canvas 2D context is not available");
    }
    this.ctx = context;
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
          if (this.peakValEl) this.peakValEl.textContent = text;
        },
      },
      meterFrames,
      {
        reducedMotion: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false,
        now: () => performance.now(),
      },
    );
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

  public destroy(): void {
    liveMeters.delete(this);
    this.controller.destroy();
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
    const w = this.canvas.width;
    const h = this.canvas.height;
    const ctx = this.ctx;

    ctx.clearRect(0, 0, w, h);

    // Track and unlit-segment tints swap with the theme so the "off" lights stay
    // visible: a dark wash on the light meter ground, a light wash on the dark.
    const trackBg = meterLightTheme ? "rgba(0, 0, 0, 0.05)" : "rgba(255, 255, 255, 0.04)";
    const unlitSeg = meterLightTheme ? "rgba(0, 0, 0, 0.10)" : "rgba(255, 255, 255, 0.06)";

    // Background track
    ctx.fillStyle = trackBg;
    ctx.fillRect(0, 0, w, h);

    // Segmented meter settings
    const numSegments = 36;
    const gap = 2;
    const segWidth = (w - (numSegments - 1) * gap) / numSegments;

    const rmsRatio = levelRatio(rmsDb);
    const activeSegments = Math.round(rmsRatio * numSegments);

    for (let i = 0; i < numSegments; i++) {
      const x = i * (segWidth + gap);
      const segRatio = i / numSegments;
      const segDb = FLOOR_DB - segRatio * FLOOR_DB;

      let color = "rgba(16, 185, 129, 0.85)"; // Green
      if (segDb > -12 && segDb <= -3) {
        color = "rgba(245, 158, 11, 0.9)"; // Amber
      } else if (segDb > -3) {
        color = "rgba(239, 68, 68, 0.95)"; // Red
      }

      if (i < activeSegments) {
        ctx.fillStyle = color;
      } else {
        ctx.fillStyle = unlitSeg;
      }

      ctx.fillRect(x, 0, segWidth, h);
    }

    // Peak hold needle
    const needleRatio = levelRatio(peakDb);
    if (needleRatio > 0.02) {
      const peakX = Math.min(w - 2, Math.max(0, needleRatio * w - 1.5));
      let needleColor = "#10b981";
      if (peakDb > -12 && peakDb <= -3) {
        needleColor = "#f59e0b";
      } else if (peakDb > -3) {
        needleColor = "#ef4444";
      }
      ctx.fillStyle = needleColor;
      ctx.fillRect(peakX, 0, 2, h);
    }
  }
}
