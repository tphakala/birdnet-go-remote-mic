// Pure logic for the VU meters (components/vu-meter.ts): the peak needle's
// hold and decay, measured in milliseconds so they look the same on every
// display refresh rate, the one animation-frame loop every meter shares, and
// MeterController, each meter's state and sequencing. No DOM here: the frame
// source and the drawing are injected, so node:test drives them.

// FLOOR_DB is the bottom of the meter scale; anything quieter draws as silence.
export const FLOOR_DB = -60;
// PEAK_HOLD_MS is how long the needle stays at a new peak before it falls.
export const PEAK_HOLD_MS = 750;
// PEAK_DECAY_DB_PER_S is how fast the needle falls once the hold is over.
export const PEAK_DECAY_DB_PER_S = 30;
// MAX_STEP_MS caps one decay step, so a long gap between frames (a stalled
// tab, a resume) lowers the needle by one short step instead of dropping it
// to the floor at once.
export const MAX_STEP_MS = 100;

// PeakNeedle is the needle's level and the time (on the caller's clock, in
// ms) until which it holds there.
export interface PeakNeedle {
  db: number;
  holdUntil: number;
}

export function newNeedle(): PeakNeedle {
  return { db: FLOOR_DB, holdUntil: 0 };
}

// clampLevel maps a level from the levels stream onto the drawn range: a
// non-finite reading or one below the floor is the floor, so silence at the
// API's -99 dBFS reads the same on every event.
export function clampLevel(db: number): number {
  return Number.isFinite(db) ? Math.max(FLOOR_DB, db) : FLOOR_DB;
}

// raisePeak moves the needle up to a louder peak and restarts its hold.
export function raisePeak(n: PeakNeedle, peakDb: number, now: number): void {
  if (peakDb > n.db) {
    n.db = peakDb;
    n.holdUntil = now + PEAK_HOLD_MS;
  }
}

// decayPeak lowers the needle for the dtMs that ended at now: nothing while
// it holds (now - holdUntil is not positive), then PEAK_DECAY_DB_PER_S for the
// part of the step after the hold, never below the floor. The step is capped
// at MAX_STEP_MS.
export function decayPeak(n: PeakNeedle, now: number, dtMs: number): void {
  const falling = Math.min(dtMs, now - n.holdUntil, MAX_STEP_MS);
  if (falling <= 0) return;
  n.db = Math.max(FLOOR_DB, n.db - (PEAK_DECAY_DB_PER_S * falling) / 1000);
}

// needleSettled reports whether the needle has nowhere left to move: it rests
// on the floor. Only raisePeak lifts it above the floor, and it falls back only
// after the hold, so a needle on the floor is never holding.
export function needleSettled(n: PeakNeedle): boolean {
  return n.db <= FLOOR_DB;
}

// Animator is one thing drawn on the shared frame loop. frame draws for the
// frame at now and returns whether it wants the next frame too.
export interface Animator {
  frame(now: number): boolean;
}

// FramePorts is the frame source: requestAnimationFrame and
// cancelAnimationFrame in the browser, a fake in tests.
export interface FramePorts {
  request(cb: (now: number) => void): number;
  cancel(handle: number): void;
}

// FrameScheduler runs one frame loop for every animator that asked for one,
// and only while some animator wants frames: an animator that returns false
// drops out until it is woken again, and the loop stops when none is left.
// Each frame runs the animators woken before it, once each; a wake made during
// a frame (by the animator itself or another) lands in the next frame, so two
// animators that wake each other cannot spin inside one frame. Suspending it
// (the dashboard is not the active view) stops the loop but keeps the woken
// animators, which get their frame on resume.
export class FrameScheduler {
  private readonly ports: FramePorts;
  // The animators due at the next frame. tick swaps it with spare, so a wake
  // during a frame goes to the next one.
  private active = new Set<Animator>();
  private spare = new Set<Animator>();
  // The animators being run by the current tick, so remove can skip a peer
  // not yet reached; null outside a tick.
  private batch: Set<Animator> | null = null;
  private handle: number | null = null;
  private suspended = false;
  // Animators whose frame threw, logged once each: a meter that throws on
  // every frame is woken again by the next level that changes it, and must
  // not log at that rate.
  private readonly reported = new WeakSet<Animator>();
  private readonly onFrame = (now: number): void => this.tick(now);

  constructor(ports: FramePorts) {
    this.ports = ports;
  }

  // wake asks for a frame for a.
  wake(a: Animator): void {
    this.active.add(a);
    this.arm();
  }

  // remove drops a from the loop, as when its meter hides or goes away, also
  // from the frame running now if a has not had its turn yet.
  remove(a: Animator): void {
    this.active.delete(a);
    this.batch?.delete(a);
    if (this.active.size === 0) this.disarm();
  }

  setSuspended(suspended: boolean): void {
    if (this.suspended === suspended) return;
    this.suspended = suspended;
    if (suspended) this.disarm();
    else this.arm();
  }

  // running reports whether a frame is requested, for tests.
  running(): boolean {
    return this.handle !== null;
  }

  private arm(): void {
    if (this.handle !== null || this.suspended || this.active.size === 0) return;
    this.handle = this.ports.request(this.onFrame);
  }

  private disarm(): void {
    if (this.handle === null) return;
    this.ports.cancel(this.handle);
    this.handle = null;
  }

  private tick(now: number): void {
    this.handle = null;
    const batch = this.active;
    this.active = this.spare;
    this.spare = batch;
    this.batch = batch;
    for (const a of batch) {
      let more = false;
      // One meter that throws must not stop the others, or the loop: it only
      // drops out until its next wake.
      try {
        more = a.frame(now);
      } catch (err) {
        if (!this.reported.has(a)) {
          this.reported.add(a);
          console.error("meter: frame failed:", err);
        }
      }
      // Still in the batch unless removed during its own frame.
      if (more && batch.has(a)) this.active.add(a);
    }
    batch.clear();
    this.batch = null;
    this.arm();
  }
}

// MeterPorts is what a MeterController shows through: the canvas bar and
// needle, the clip latch button and the dB readout. VUMeter implements them
// on the DOM; tests record the calls.
export interface MeterPorts {
  draw(rmsDb: number, peakDb: number): void;
  showClip(clipped: boolean): void;
  showReadout(text: string): void;
}

// FrameSource is the part of FrameScheduler a MeterController uses.
export type FrameSource = Pick<FrameScheduler, "wake" | "remove">;

// formatReadout is the dB readout text for a needle level.
export function formatReadout(db: number): string {
  return db <= -59.9 ? "-inf" : `${db.toFixed(1)} dBFS`;
}

// MeterController is one VU meter's state and sequencing, with no DOM: the
// levels it was given, the peak needle, the clip latch, what it last showed,
// and when it needs a frame. VUMeter owns the canvas and elements and drives
// it; the dashboard never talks to it directly.
export class MeterController implements Animator {
  private readonly ports: MeterPorts;
  private readonly frames: FrameSource;
  private readonly now: () => number;

  private rms = FLOOR_DB;
  private readonly needle = newNeedle();
  private clipped = false;
  // The latch state last shown, so a draw touches the button only on change.
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
  private paused = false;
  private destroyed = false;
  // With reduced motion the needle does not glide: it holds and falls in
  // steps, one per level event, and each event draws once.
  private reducedMotion: boolean;

  constructor(ports: MeterPorts, frames: FrameSource, opts: { reducedMotion: boolean; now: () => number }) {
    this.ports = ports;
    this.frames = frames;
    this.now = opts.now;
    this.reducedMotion = opts.reducedMotion;
    // The latch shows unlatched at once, not at the first frame, which waits
    // while the dashboard is not showing.
    this.syncClip();
    // The first frame draws the empty track.
    this.frames.wake(this);
  }

  public setLevels(rms: number, peak: number, clipped: boolean = false): void {
    const now = this.now();
    this.rms = clampLevel(rms);
    const peakDb = clampLevel(peak);
    if (clipped || peakDb >= -0.1) this.clipped = true;

    raisePeak(this.needle, peakDb, now);
    // With no animation, the needle falls once per level event by the time
    // since the last one.
    if (this.reducedMotion) {
      if (this.lastLevels !== null) decayPeak(this.needle, now, now - this.lastLevels);
      this.lastLevels = now;
    }

    if (this.paused || this.destroyed) return;
    // Silence on a settled meter changes nothing on screen: no frame.
    const moving = !this.reducedMotion && !needleSettled(this.needle);
    if (moving || this.stale || this.rms !== this.drawnRms || this.needle.db !== this.drawnPeak || this.clipped !== this.shownClip) {
      this.frames.wake(this);
    }
  }

  public clearClip(): void {
    this.clipped = false;
    this.syncClip();
  }

  // pause takes the meter off the frame loop while its row is hidden. Levels
  // still update its state, drawn on resume.
  public pause(): void {
    if (this.paused) return;
    this.paused = true;
    this.lastFrame = null;
    this.frames.remove(this);
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
    if (!this.paused && !this.destroyed) this.frames.wake(this);
  }

  public destroy(): void {
    this.destroyed = true;
    this.frames.remove(this);
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

  private syncClip(): void {
    if (this.shownClip === this.clipped) return;
    this.shownClip = this.clipped;
    this.ports.showClip(this.clipped);
  }

  private draw(): void {
    this.syncClip();
    const peakDb = this.needle.db;
    if (!this.stale && this.rms === this.drawnRms && peakDb === this.drawnPeak) return;
    this.stale = false;
    this.drawnRms = this.rms;
    this.drawnPeak = peakDb;
    this.ports.draw(this.rms, peakDb);
    const text = formatReadout(peakDb);
    if (text !== this.shownReadout) {
      this.shownReadout = text;
      this.ports.showReadout(text);
    }
  }
}
