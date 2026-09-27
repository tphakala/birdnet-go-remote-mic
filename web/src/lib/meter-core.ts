// Pure logic for the VU meters (components/vu-meter.ts): the peak needle's
// hold and decay, measured in milliseconds so they look the same on every
// display refresh rate, and the one animation-frame loop every meter shares.
// No DOM here: the frame source is injected, so node:test drives it.

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
// Suspending it (the dashboard is not the active view) stops the loop but
// keeps the woken animators, which get their frame on resume.
export class FrameScheduler {
  private readonly ports: FramePorts;
  private readonly active = new Set<Animator>();
  private handle: number | null = null;
  private suspended = false;
  private readonly onFrame = (now: number): void => this.tick(now);

  constructor(ports: FramePorts) {
    this.ports = ports;
  }

  // wake asks for a frame for a.
  wake(a: Animator): void {
    this.active.add(a);
    this.arm();
  }

  // remove drops a from the loop, as when its meter hides or goes away.
  remove(a: Animator): void {
    this.active.delete(a);
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
    for (const a of this.active) {
      let more = false;
      // One meter that throws must not stop the others, or the loop: it only
      // drops out until its next wake.
      try {
        more = a.frame(now);
      } catch (err) {
        console.error("meter: frame failed:", err);
      }
      if (!more) this.active.delete(a);
    }
    this.arm();
  }
}
