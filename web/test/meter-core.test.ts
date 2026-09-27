// Unit tests for lib/meter-core.ts: the peak needle's hold and decay in
// milliseconds, and the shared frame loop that runs only while a meter has
// something to animate. Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import {
  clampLevel,
  decayPeak,
  FLOOR_DB,
  FrameScheduler,
  MAX_STEP_MS,
  MeterController,
  type MeterPorts,
  needleSettled,
  newNeedle,
  PEAK_DECAY_DB_PER_S,
  PEAK_HOLD_MS,
  raisePeak,
  type Animator,
  type FramePorts,
  type PeakNeedle,
} from "../src/lib/meter-core.ts";
import { at } from "./fixtures.ts";

// EPS absorbs float rounding in the summed frame times.
const EPS = 1e-6;

// runFrames advances a needle through frames every stepMs from start until
// it settles (or a safety cap), and returns when the hold ended (the first
// frame the needle moved) and when it settled.
function runFrames(stepMs: number, peak: number): { released: number; settled: number } {
  const n: PeakNeedle = newNeedle();
  raisePeak(n, peak, 0);
  let released = -1;
  let now = 0;
  while (!needleSettled(n) && now < 60_000) {
    now += stepMs;
    const before = n.db;
    decayPeak(n, now, stepMs);
    if (released < 0 && n.db < before) released = now;
  }
  return { released, settled: now };
}

test("the hold lasts the same time at 60, 120 and 144 Hz", () => {
  for (const hz of [60, 120, 144]) {
    const step = 1000 / hz;
    const { released } = runFrames(step, -20);
    // The first frame past the hold moves the needle, so release lands within
    // one frame after PEAK_HOLD_MS.
    assert.ok(released > PEAK_HOLD_MS && released <= PEAK_HOLD_MS + step + EPS, `${hz} Hz released at ${released} ms, want just after ${PEAK_HOLD_MS} ms`);
  }
});

test("the needle falls at the same rate at every refresh rate", () => {
  // From -20 dBFS the fall to the floor is 40 dB at PEAK_DECAY_DB_PER_S.
  const want = PEAK_HOLD_MS + (40 / PEAK_DECAY_DB_PER_S) * 1000;
  for (const hz of [60, 120, 144]) {
    const step = 1000 / hz;
    const { settled } = runFrames(step, -20);
    // It settles on the first frame at or after the target.
    assert.ok(settled >= want - EPS && settled <= want + step + EPS, `${hz} Hz settled at ${settled} ms, want within a frame after ${want} ms`);
  }
});

test("reduced motion steps at about 10 Hz hold and fall like the animated needle", () => {
  // One decay step per level event, by the time since the previous one.
  const { released, settled } = runFrames(100, -20);
  assert.ok(released > PEAK_HOLD_MS && released <= PEAK_HOLD_MS + 100, `released at ${released} ms`);
  const want = PEAK_HOLD_MS + (40 / PEAK_DECAY_DB_PER_S) * 1000;
  assert.ok(settled >= want - EPS && settled <= want + 100 + EPS, `settled at ${settled} ms, want within a step after ${want} ms`);
});

test("a louder peak raises the needle and restarts the hold; a quieter one does not", () => {
  const n = newNeedle();
  raisePeak(n, -30, 0);
  assert.equal(n.db, -30);
  assert.equal(n.holdUntil, PEAK_HOLD_MS);
  raisePeak(n, -40, 500);
  assert.equal(n.db, -30);
  assert.equal(n.holdUntil, PEAK_HOLD_MS, "a quieter peak must not extend the hold");
  raisePeak(n, -10, 500);
  assert.equal(n.db, -10);
  assert.equal(n.holdUntil, 500 + PEAK_HOLD_MS);
});

test("one decay step is capped, and only the part after the hold falls", () => {
  const n = newNeedle();
  raisePeak(n, -20, 0);
  // A 5 s gap (a stalled tab) moves the needle by one capped step.
  decayPeak(n, 5_000, 5_000);
  assert.equal(n.db, -20 - (PEAK_DECAY_DB_PER_S * MAX_STEP_MS) / 1000);
  // A step that straddles the end of the hold falls only for its tail.
  const m = newNeedle();
  raisePeak(m, -20, 0);
  decayPeak(m, PEAK_HOLD_MS + 10, 50);
  assert.equal(m.db, -20 - (PEAK_DECAY_DB_PER_S * 10) / 1000);
});

test("the needle stops at the floor and settles there", () => {
  const n = newNeedle();
  assert.equal(needleSettled(n), true, "a fresh needle rests on the floor");
  raisePeak(n, FLOOR_DB + 1, 0);
  assert.equal(needleSettled(n), false);
  // Still holding just above the floor: not settled.
  decayPeak(n, PEAK_HOLD_MS, MAX_STEP_MS);
  assert.equal(needleSettled(n), false);
  decayPeak(n, PEAK_HOLD_MS + MAX_STEP_MS, MAX_STEP_MS);
  assert.equal(n.db, FLOOR_DB, "the fall must stop at the floor");
  assert.equal(needleSettled(n), true);
});

test("clampLevel maps silence and bad readings to the floor", () => {
  assert.equal(clampLevel(-99), FLOOR_DB);
  assert.equal(clampLevel(Number.NEGATIVE_INFINITY), FLOOR_DB);
  assert.equal(clampLevel(Number.NaN), FLOOR_DB);
  assert.equal(clampLevel(-12.5), -12.5);
  assert.equal(clampLevel(0.4), 0.4);
});

// FakeFrames is a frame source a test steps by hand.
class FakeFrames implements FramePorts {
  private next = 1;
  readonly pending = new Map<number, (now: number) => void>();
  requests = 0;

  request(cb: (now: number) => void): number {
    this.requests++;
    const h = this.next++;
    this.pending.set(h, cb);
    return h;
  }

  cancel(handle: number): void {
    this.pending.delete(handle);
  }

  // step runs the requested frame at now.
  step(now: number): void {
    const [entry] = this.pending;
    assert.ok(entry, "no frame requested");
    const [h, cb] = entry;
    this.pending.delete(h);
    cb(now);
  }
}

// countdown is an animator that wants frames n more times.
function countdown(n: number): Animator & { frames: number[] } {
  const frames: number[] = [];
  return {
    frames,
    frame(now: number): boolean {
      frames.push(now);
      return frames.length < n;
    },
  };
}

test("the loop runs while an animator wants frames and stops when none does", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  assert.equal(s.running(), false);
  const a = countdown(2);
  const b = countdown(1);
  s.wake(a);
  s.wake(b);
  assert.equal(f.requests, 1, "two wakes share one frame request");
  f.step(16);
  assert.deepEqual(b.frames, [16]);
  assert.equal(s.running(), true, "a still wants a frame");
  f.step(32);
  assert.deepEqual(a.frames, [16, 32]);
  assert.equal(s.running(), false, "the loop must stop once every animator settled");
  assert.equal(f.pending.size, 0);
  // A later wake starts it again.
  s.wake(b);
  assert.equal(s.running(), true);
});

test("suspending stops the loop and resuming gives the woken animators their frame", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const a = countdown(1);
  s.wake(a);
  s.setSuspended(true);
  assert.equal(s.running(), false);
  assert.equal(f.pending.size, 0, "the requested frame must be cancelled");
  // A wake while suspended requests nothing, as for a meter built while
  // another view shows.
  const late = countdown(1);
  s.wake(late);
  assert.equal(f.pending.size, 0);
  s.setSuspended(false);
  assert.equal(s.running(), true);
  f.step(100);
  assert.deepEqual(a.frames, [100]);
  assert.deepEqual(late.frames, [100], "an animator woken while suspended must get its frame on resume");
});

test("removing the last animator cancels the pending frame", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const a = countdown(5);
  s.wake(a);
  s.remove(a);
  assert.equal(s.running(), false);
  assert.equal(f.pending.size, 0);
});

test("removing one animator keeps the loop for the other", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const a = countdown(5);
  const b = countdown(5);
  s.wake(a);
  s.wake(b);
  s.remove(a);
  assert.equal(s.running(), true, "the other animator still wants its frame");
  f.step(1);
  assert.deepEqual(b.frames, [1]);
  assert.deepEqual(a.frames, []);
});

test("a wake during a frame runs in the next frame, not again in this one", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const order: string[] = [];
  // a and b wake each other on every frame: without the next-frame rule they
  // would spin inside one tick.
  let b: Animator;
  const a: Animator = {
    frame(now: number): boolean {
      order.push(`a@${now}`);
      s.wake(b);
      return false;
    },
  };
  b = {
    frame(now: number): boolean {
      order.push(`b@${now}`);
      s.wake(a);
      return false;
    },
  };
  s.wake(a);
  s.wake(b);
  f.step(1);
  assert.deepEqual(order, ["a@1", "b@1"], "each animator runs once per frame");
  assert.equal(s.running(), true, "the wakes made during the frame want the next one");
  f.step(2);
  // Frame 2 runs them in the order they were woken during frame 1.
  assert.deepEqual(order, ["a@1", "b@1", "b@2", "a@2"]);
});

test("an animator that wakes itself and returns false gets the next frame", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const frames: number[] = [];
  const a: Animator = {
    frame(now: number): boolean {
      frames.push(now);
      if (frames.length === 1) s.wake(a);
      return false;
    },
  };
  s.wake(a);
  f.step(1);
  assert.equal(s.running(), true, "the self-wake must not be lost");
  f.step(2);
  assert.deepEqual(frames, [1, 2]);
  assert.equal(s.running(), false);
});

test("removing a peer during a frame skips it", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const b = countdown(5);
  const a: Animator = {
    frame(): boolean {
      s.remove(b);
      return false;
    },
  };
  s.wake(a);
  s.wake(b);
  f.step(1);
  assert.deepEqual(b.frames, [], "a peer removed before its turn must not run");
  assert.equal(s.running(), false);
});

test("a wake then remove during a frame leaves no frame requested", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const c = countdown(1);
  const a: Animator = {
    frame(): boolean {
      s.wake(c);
      s.remove(c);
      return false;
    },
  };
  s.wake(a);
  f.step(1);
  assert.equal(s.running(), false);
  assert.equal(f.pending.size, 0, "no frame may stay requested with nothing to run");
});

test("an animator that throws drops out without stopping the others", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  let badFrames = 0;
  const bad: Animator = {
    frame(): boolean {
      badFrames++;
      throw new Error("boom");
    },
  };
  const good = countdown(3);
  const saved = console.error;
  console.error = () => {};
  try {
    s.wake(bad);
    s.wake(good);
    f.step(1);
    f.step(2);
  } finally {
    console.error = saved;
  }
  assert.deepEqual(good.frames, [1, 2]);
  assert.equal(badFrames, 1, "the bad animator runs once, then waits for its next wake");
});

test("an animator that removes itself during its frame leaves the loop", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const frames: number[] = [];
  const a: Animator = {
    frame(now: number): boolean {
      frames.push(now);
      s.remove(a);
      return true;
    },
  };
  s.wake(a);
  f.step(1);
  assert.equal(s.running(), false, "a self-removed animator must not stay on the loop");
  assert.deepEqual(frames, [1]);
});

test("an animator that throws on every frame is logged once", () => {
  const f = new FakeFrames();
  const s = new FrameScheduler(f);
  const bad: Animator = {
    frame(): boolean {
      throw new Error("boom");
    },
  };
  const logged: unknown[] = [];
  const saved = console.error;
  console.error = (...args: unknown[]) => logged.push(args);
  try {
    for (let i = 1; i <= 3; i++) {
      // A level event wakes it again each time.
      s.wake(bad);
      f.step(i);
    }
  } finally {
    console.error = saved;
  }
  assert.equal(logged.length, 1, "a repeating failure must be logged once, not per frame");
});

// meterHarness builds a MeterController on a real FrameScheduler over
// FakeFrames, with a clock the test sets and ports that record every call.
function meterHarness(opts: { reducedMotion?: boolean; suspended?: boolean } = {}) {
  const f = new FakeFrames();
  const frames = new FrameScheduler(f);
  if (opts.suspended) frames.setSuspended(true);
  let clock = 0;
  const draws: [number, number][] = [];
  const clips: boolean[] = [];
  const readouts: string[] = [];
  const ports: MeterPorts = {
    draw: (rms, peak) => draws.push([rms, peak]),
    showClip: (on) => clips.push(on),
    showReadout: (text) => readouts.push(text),
  };
  const meter = new MeterController(ports, frames, { reducedMotion: opts.reducedMotion ?? false, now: () => clock });
  return {
    meter,
    frames,
    f,
    draws,
    clips,
    readouts,
    setClock: (t: number) => {
      clock = t;
    },
    // step runs the requested frame at the current clock.
    step: () => f.step(clock),
  };
}

test("the first frame draws the empty track", () => {
  const h = meterHarness();
  assert.equal(h.frames.running(), true);
  h.step();
  assert.deepEqual(h.draws, [[FLOOR_DB, FLOOR_DB]]);
  assert.deepEqual(h.readouts, ["-inf"]);
  assert.deepEqual(h.clips, [false]);
  assert.equal(h.frames.running(), false, "an empty meter has nothing to animate");
});

test("a level change wakes the loop and draws", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-20, -10);
  assert.equal(h.frames.running(), true, "new levels must request a frame");
  h.step();
  assert.deepEqual(h.draws.at(-1), [-20, -10]);
  assert.equal(h.readouts.at(-1), "-10.0 dBFS");
});

test("silence on a settled meter requests no frame", () => {
  const h = meterHarness();
  h.step();
  for (let i = 1; i <= 5; i++) {
    h.setClock(i * 100);
    h.meter.setLevels(-99, -99);
    assert.equal(h.frames.running(), false, `silent event ${i} must not wake the loop`);
  }
  assert.equal(h.draws.length, 1);
});

test("a paused meter leaves the loop and redraws on resume", () => {
  const h = meterHarness();
  h.step();
  h.meter.pause();
  h.setClock(10);
  h.meter.setLevels(-20, -30);
  assert.equal(h.frames.running(), false, "a paused meter must not request frames");
  h.meter.resume();
  assert.equal(h.frames.running(), true);
  h.step();
  assert.deepEqual(h.draws.at(-1), [-20, -30], "resume shows the levels that arrived while paused");
});

test("pause takes a moving meter off the loop", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-20, -10);
  assert.equal(h.frames.running(), true);
  h.meter.pause();
  assert.equal(h.frames.running(), false, "pause must cancel the meter's frame");
});

test("a destroyed meter never draws again", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-20, -10);
  h.meter.destroy();
  assert.equal(h.frames.running(), false);
  h.meter.redraw();
  h.meter.resume();
  h.meter.setLevels(-10, -5);
  assert.equal(h.frames.running(), false, "nothing may wake a destroyed meter");
  assert.equal(h.draws.length, 1);
});

test("a meter created while the loop is suspended draws on resume", () => {
  const h = meterHarness({ suspended: true });
  assert.equal(h.frames.running(), false);
  assert.deepEqual(h.clips, [false], "the latch shows unlatched before any frame");
  h.frames.setSuspended(false);
  assert.equal(h.frames.running(), true);
  h.step();
  assert.deepEqual(h.draws, [[FLOOR_DB, FLOOR_DB]]);
});

test("the clip latch is shown once and clears", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-20, -20, true);
  h.step();
  assert.deepEqual(h.clips, [false, true]);
  // Later events leave the latch shown without repainting it.
  h.setClock(20);
  h.meter.setLevels(-20, -20);
  h.step();
  assert.deepEqual(h.clips, [false, true]);
  h.meter.clearClip();
  assert.deepEqual(h.clips, [false, true, false]);
});

test("a latched clip on a settled, silent meter wakes the loop once", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-99, -99, true);
  assert.equal(h.frames.running(), true, "a new latch must be shown");
  h.step();
  h.setClock(20);
  h.meter.setLevels(-99, -99);
  assert.equal(h.frames.running(), false, "a latch already shown must not wake the loop again");
});

test("an RMS change alone wakes a settled meter", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-30, -99);
  assert.equal(h.frames.running(), true, "a new bar level must be drawn");
  h.step();
  assert.deepEqual(h.draws.at(-1), [-30, FLOOR_DB]);
});

test("a needle change alone wakes the meter", () => {
  const h = meterHarness();
  h.step();
  h.setClock(10);
  h.meter.setLevels(-99, -40);
  assert.equal(h.frames.running(), true, "a new peak must be drawn");
  h.step();
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, -40]);
});

test("redraw repaints unchanged levels", () => {
  const h = meterHarness();
  h.step();
  h.meter.redraw();
  assert.equal(h.frames.running(), true);
  h.step();
  assert.equal(h.draws.length, 2, "a theme change must repaint even with no new levels");
  h.meter.pause();
  h.meter.resume();
  h.step();
  assert.equal(h.draws.length, 3, "a resumed row must repaint even with no new levels");
});

test("with reduced motion the needle steps once per level event and asks for no more frames", () => {
  const h = meterHarness({ reducedMotion: true });
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  assert.equal(h.frames.running(), false, "reduced motion draws once per event, no animation");
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, -20]);
  // After the hold, each event lowers the needle by the time since the last.
  let t = 0;
  while (t < PEAK_HOLD_MS + 500) {
    t += 100;
    h.setClock(t);
    h.meter.setLevels(-99, -99);
    if (h.frames.running()) h.step();
  }
  const peak = at(h.draws, h.draws.length - 1)[1];
  assert.ok(peak < -20 && peak > FLOOR_DB, `needle at ${peak} dB, want it falling from -20`);
});
