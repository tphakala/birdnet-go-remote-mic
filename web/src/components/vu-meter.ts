import { clampLevel, decayPeak, FLOOR_DB, FrameScheduler, needleSettled, newNeedle, raisePeak, type Animator } from "../lib/meter-core.ts";

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

export class VUMeter implements Animator {
  private canvas: HTMLCanvasElement;
  private ctx: CanvasRenderingContext2D;
  private peakValEl: HTMLElement | null;
  private clipEl: HTMLElement | null;

  private rmsVal: number = FLOOR_DB;
  private readonly needle = newNeedle();
  private isClipped: boolean = false;
  // The latch state last written to the clip button, so a draw touches the
  // DOM only when it changes.
  private shownClip: boolean | null = null;
  // What the canvas last showed, so a frame with nothing new skips the draw.
  // stale forces the next draw: a new meter, a resume, a theme change.
  private stale = true;
  private drawnRms = FLOOR_DB;
  private drawnPeak = FLOOR_DB;
  private shownReadout = "";

  // The previous animation frame's time, for the needle's decay step. Null
  // while the meter is not animating, so the first frame after a wake decays
  // nothing instead of catching up the idle time.
  private lastFrame: number | null = null;
  // The previous level event's time, for the reduced-motion decay step.
  private lastLevels: number | null = null;
  private paused: boolean = false;
  private destroyed = false;
  // When the viewer prefers reduced motion, the needle does not glide: it
  // holds and falls in steps, one per level event, and each event draws once.
  private reducedMotion: boolean;

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
    this.reducedMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;

    if (this.clipEl) {
      this.clipEl.addEventListener("click", () => this.clearClip());
      this.syncClip();
    }

    liveMeters.add(this);
    // The first frame draws the empty track.
    meterFrames.wake(this);
  }

  public setLevels(rms: number, peak: number, clipped: boolean = false): void {
    const now = performance.now();
    this.rmsVal = clampLevel(rms);
    const peakDb = clampLevel(peak);

    if (clipped || peakDb >= -0.1) {
      this.isClipped = true;
    }

    raisePeak(this.needle, peakDb, now);
    // With no animation, the needle falls once per level event (about 10 Hz)
    // by the time since the last one, so it holds and falls as long as the
    // animated needle does, and the dB readout holds recent peaks instead of
    // flickering.
    if (this.reducedMotion) {
      if (this.lastLevels !== null) decayPeak(this.needle, now, now - this.lastLevels);
      this.lastLevels = now;
    }

    if (this.paused || this.destroyed) return;
    // Silence on a settled meter changes nothing on screen: no frame.
    const moving = !this.reducedMotion && !needleSettled(this.needle);
    if (moving || this.stale || this.rmsVal !== this.drawnRms || this.needle.db !== this.drawnPeak || this.isClipped !== this.shownClip) {
      meterFrames.wake(this);
    }
  }

  public clearClip(): void {
    this.isClipped = false;
    this.syncClip();
  }

  // syncClip shows the latch on the clip button: the lit style, and
  // aria-pressed so a screen reader hears whether it is latched (pressing it
  // clears the latch, which releases the button).
  private syncClip(): void {
    const el = this.clipEl;
    if (!el || this.shownClip === this.isClipped) return;
    this.shownClip = this.isClipped;
    el.classList.toggle("clipped", this.isClipped);
    el.setAttribute("aria-pressed", String(this.isClipped));
  }

  // pause takes the meter off the frame loop while its row is hidden. Levels
  // still update its state, drawn on resume.
  public pause(): void {
    if (this.paused) return;
    this.paused = true;
    this.lastFrame = null;
    meterFrames.remove(this);
  }

  public resume(): void {
    if (!this.paused) return;
    this.paused = false;
    this.redraw();
  }

  // redraw repaints the meter on the next frame even if its levels did not
  // change, as after a theme change.
  public redraw(): void {
    this.stale = true;
    if (!this.paused && !this.destroyed) meterFrames.wake(this);
  }

  public destroy(): void {
    this.destroyed = true;
    liveMeters.delete(this);
    meterFrames.remove(this);
  }

  // frame is the meter's turn on the shared loop: move the needle by the time
  // since the last frame, draw if anything changed, and ask for another frame
  // only while the needle still has somewhere to go.
  public frame(now: number): boolean {
    if (!this.reducedMotion) {
      const dt = this.lastFrame === null ? 0 : now - this.lastFrame;
      this.lastFrame = now;
      decayPeak(this.needle, now, dt);
    }
    this.draw();
    const more = !this.reducedMotion && !needleSettled(this.needle);
    if (!more) this.lastFrame = null;
    return more;
  }

  private dbToRatio(db: number): number {
    // Calibrate -60 dBFS to 0.0 and 0 dBFS to 1.0
    if (db <= -60) return 0;
    if (db >= 0) return 1;
    return (db + 60) / 60;
  }

  private draw(): void {
    this.syncClip();
    const peakDb = this.needle.db;
    if (!this.stale && this.rmsVal === this.drawnRms && peakDb === this.drawnPeak) return;
    this.stale = false;
    this.drawnRms = this.rmsVal;
    this.drawnPeak = peakDb;

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

    const rmsRatio = this.dbToRatio(this.rmsVal);
    const activeSegments = Math.round(rmsRatio * numSegments);

    for (let i = 0; i < numSegments; i++) {
      const x = i * (segWidth + gap);
      const segRatio = i / numSegments;
      const segDb = -60 + segRatio * 60;

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
    const needleRatio = this.dbToRatio(peakDb);
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

    // Update DOM indicators
    if (this.peakValEl) {
      const formatted = peakDb <= -59.9 ? "-inf" : `${peakDb.toFixed(1)} dBFS`;
      if (formatted !== this.shownReadout) {
        this.shownReadout = formatted;
        this.peakValEl.textContent = formatted;
      }
    }
  }
}
