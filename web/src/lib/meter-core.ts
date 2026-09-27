// Pure logic for the VU meters (components/vu-meter.ts): the peak needle's
// hold and decay, measured in milliseconds so they look the same on every
// display refresh rate, the one animation-frame loop every meter shares, and
// MeterController, each meter's state and sequencing. No DOM here: the frame
// source and the drawing are injected, so node:test drives them.

import type { Timers } from "./timers.ts";

// FLOOR_DB is the bottom of the meter scale; anything quieter draws as silence.
export const FLOOR_DB = -60;
// PEAK_HOLD_MS is how long the needle stays at a new peak before it falls.
export const PEAK_HOLD_MS = 750;
// PEAK_DECAY_DB_PER_S is how fast the needle falls once the hold is over.
export const PEAK_DECAY_DB_PER_S = 30;
// READOUT_INTERVAL_MS is the shortest time between two readout changes while
// the needle falls, so the digits stay readable instead of changing on every
// display frame.
export const READOUT_INTERVAL_MS = 100;

// PeakNeedle is the needle's level, the time until which it holds there, and
// the time it was last brought up to date (null before the first update). All
// three are on the caller's clock, in ms.
export interface PeakNeedle {
  db: number;
  holdUntil: number;
  at: number | null;
}

export function newNeedle(): PeakNeedle {
  return { db: FLOOR_DB, holdUntil: 0, at: null };
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

// advanceNeedle brings the needle up to date at now: it falls at
// PEAK_DECAY_DB_PER_S for the part of the time since the last update that is
// past the hold, never below the floor. The fall depends only on elapsed time,
// so any mix of level events and animation frames, at any rate and after any
// gap, puts the needle where it would be on a continuous curve. A reading not
// after the last one changes nothing, so a caller that mixes clocks
// (animation-frame timestamps and performance.now()) cannot move it backwards
// or count the same time twice.
export function advanceNeedle(n: PeakNeedle, now: number): void {
  if (!Number.isFinite(now)) return;
  if (n.at === null) {
    n.at = now;
    return;
  }
  if (!(now > n.at)) return;
  const falling = now - Math.max(n.at, n.holdUntil);
  n.at = now;
  if (!(falling > 0)) return;
  n.db = Math.max(FLOOR_DB, n.db - (PEAK_DECAY_DB_PER_S * falling) / 1000);
}

// needleSettled reports whether the needle has nowhere left to move: it is not
// above the floor (a NaN level counts as settled, so it cannot keep the frame
// loop running). Only raisePeak lifts it above the floor, and it falls back
// only after the hold, so a needle on the floor is never holding.
export function needleSettled(n: PeakNeedle): boolean {
  return !(n.db > FLOOR_DB);
}

// levelRatio places a level on the meter scale: 0 at the floor or below, 1 at
// 0 dBFS or above.
export function levelRatio(db: number): number {
  if (!(db > FLOOR_DB)) return 0;
  if (db >= 0) return 1;
  return (db - FLOOR_DB) / -FLOOR_DB;
}

// Animator is one thing drawn on the shared frame loop. frame draws for the
// frame at now and returns whether it wants the next frame too.
export interface Animator {
  frame(now: number): boolean;
}

// FramePorts is the frame source (requestAnimationFrame and
// cancelAnimationFrame in the browser) and the timers for timed wakes (the
// Timers seam in lib/timers.ts, globalThis in the browser), fakes in tests.
export interface FramePorts {
  request(cb: (now: number) => void): number;
  cancel(handle: number): void;
  timers: Pick<Timers, "setTimeout" | "clearTimeout">;
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
  // Pending timed wakes (wakeAfter), at most one per animator.
  private readonly timed = new Map<Animator, ReturnType<typeof setTimeout>>();
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

  // wakeAfter wakes a once ms from now, replacing any timed wake a already
  // has: an animator with nothing to draw until then (a needle holding its
  // peak) asks for no frames meanwhile.
  wakeAfter(a: Animator, ms: number): void {
    this.cancelTimed(a);
    this.timed.set(a, this.ports.timers.setTimeout(() => {
      this.timed.delete(a);
      this.wake(a);
    }, ms));
  }

  // remove drops a from the loop, as when its meter hides or goes away, also
  // from the frame running now if a has not had its turn yet, and cancels its
  // timed wake.
  remove(a: Animator): void {
    this.active.delete(a);
    this.batch?.delete(a);
    this.cancelTimed(a);
    if (this.active.size === 0) this.disarm();
  }

  private cancelTimed(a: Animator): void {
    const handle = this.timed.get(a);
    if (handle === undefined) return;
    this.ports.timers.clearTimeout(handle);
    this.timed.delete(a);
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
export type FrameSource = Pick<FrameScheduler, "wake" | "wakeAfter" | "remove">;

// WAITING_READOUT is the readout while a meter has no levels to show: before
// its first level and after MeterController.clearLevels.
export const WAITING_READOUT = "--";

// levelBand is the colour band a level falls in: 0 green, 1 amber above
// -12 dBFS, 2 red above -3 dBFS. The segments and the needle share it.
export function levelBand(db: number): 0 | 1 | 2 {
  if (db > -3) return 2;
  if (db > -12) return 1;
  return 0;
}

// formatReadout is the dB readout text for a needle level: "-inf" at the
// floor and within the last 0.1 dB above it.
export function formatReadout(db: number): string {
  return db <= FLOOR_DB + 0.1 ? "-inf" : `${db.toFixed(1)} dBFS`;
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
  // True until a level arrives, and again after clearLevels: the readout
  // says so, since no data is not silence. A new meter starts here, as one
  // built while the stream is down or off the dashboard has no level yet.
  private waiting = true;
  // The needle level behind shownReadout, and the frame time it was written.
  private shownReadoutDb = FLOOR_DB;
  private readoutAt = Number.NEGATIVE_INFINITY;

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
    // Leaving the waiting readout needs a frame even when nothing else moved
    // (a silent channel stays at the floor).
    const wasWaiting = this.waiting;
    this.waiting = false;
    this.rms = clampLevel(rms);
    const peakDb = clampLevel(peak);
    if (clipped || peakDb >= -0.1) this.clipped = true;

    // Every level event brings the needle up to date first, so it is current
    // even while the meter is off the frame loop (a hidden row, another view)
    // and, with reduced motion, steps down once per event.
    advanceNeedle(this.needle, now);
    raisePeak(this.needle, peakDb, now);

    if (this.paused || this.destroyed) return;
    // Silence on a settled or holding meter changes nothing on screen: no
    // frame. A holding needle has its timed wake for the end of the hold.
    const moving = !this.reducedMotion && !needleSettled(this.needle) && now >= this.needle.holdUntil;
    if (wasWaiting || moving || this.stale || this.rms !== this.drawnRms || this.needle.db !== this.drawnPeak || this.clipped !== this.shownClip) {
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

  // clearLevels drops the bar and the needle to the floor when the levels
  // stopped coming (the stream dropped them or went down), so neither shows a
  // stale level, and the readout shows WAITING_READOUT instead of "-inf" until
  // the next level arrives, so no data does not read as silence. The clip
  // latch keeps what it showed: a latch waits for the operator.
  public clearLevels(): void {
    this.waiting = true;
    this.rms = FLOOR_DB;
    this.needle.db = FLOOR_DB;
    this.needle.holdUntil = 0;
    // The hold is over, so a timed wake for its end has nothing left to do.
    this.frames.remove(this);
    if (!this.paused && !this.destroyed) this.frames.wake(this);
  }

  // setReducedMotion follows the viewer's motion preference when it changes.
  // The redraw it asks for also restarts the glide of a needle still falling.
  public setReducedMotion(reduced: boolean): void {
    if (this.reducedMotion === reduced) return;
    this.reducedMotion = reduced;
    this.redraw();
  }

  // frame is the meter's turn on the shared loop: bring the needle up to date
  // (it glides only without reduced motion), draw if anything changed, and ask
  // for another frame only while the needle still has somewhere to go.
  public frame(now: number): boolean {
    if (!this.reducedMotion) advanceNeedle(this.needle, now);
    this.draw();
    this.syncReadout(now);
    if (this.reducedMotion || needleSettled(this.needle)) return false;
    // While the needle holds its peak nothing moves, so rather than a frame
    // per display refresh the meter sleeps until the hold ends.
    const holding = this.needle.holdUntil - now;
    if (holding > 0) {
      this.frames.wakeAfter(this, holding);
      return false;
    }
    return true;
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
  }

  // syncReadout writes the dB readout when its text changed. A rise shows at
  // once; a fall while the needle glides waits until READOUT_INTERVAL_MS after
  // the last write, and the frames of the glide carry it out. The settled value
  // and every reduced-motion step (already one per level event) show at once.
  private syncReadout(now: number): void {
    if (this.waiting) {
      if (this.shownReadout !== WAITING_READOUT) {
        this.shownReadout = WAITING_READOUT;
        this.ports.showReadout(WAITING_READOUT);
      }
      return;
    }
    const db = this.needle.db;
    // The throttle is checked before the text is formatted, since most frames
    // of a glide fall inside it.
    // A needle holding its peak is not falling, so its value shows at once:
    // no frame will come to carry a held-back write out until the hold ends.
    const held = !(db > this.shownReadoutDb) && !this.reducedMotion && !needleSettled(this.needle) && now >= this.needle.holdUntil;
    if (held && now - this.readoutAt < READOUT_INTERVAL_MS) return;
    const text = formatReadout(db);
    if (text === this.shownReadout) return;
    this.shownReadout = text;
    this.shownReadoutDb = db;
    this.readoutAt = now;
    this.ports.showReadout(text);
  }
}
