// Unit tests for lib/meter-core.ts: the peak needle's hold and decay in
// milliseconds, the shared frame loop that runs only while a meter has
// something to animate, and MeterController, each meter's state and
// sequencing. Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import {
  advanceNeedle,
  clampLevel,
  FLOOR_DB,
  formatReadout,
  FrameScheduler,
  levelRatio,
  MeterController,
  type MeterPorts,
  needleSettled,
  newNeedle,
  PEAK_DECAY_DB_PER_S,
  PEAK_HOLD_MS,
  raisePeak,
  READOUT_INTERVAL_MS,
  WAITING_READOUT,
  type Animator,
  type FramePorts,
} from "../src/lib/meter-core.ts";
import { at, FakeTimers } from "./fixtures.ts";

// curve is where a needle raised to peak at time 0 should be at t on the
// continuous hold-then-fall curve.
function curve(peak: number, t: number): number {
  if (t <= PEAK_HOLD_MS) return peak;
  return Math.max(FLOOR_DB, peak - (PEAK_DECAY_DB_PER_S * (t - PEAK_HOLD_MS)) / 1000);
}

// UPDATE_PATTERNS are the gaps between updates a needle sees: display frames
// at common refresh rates, level events at their 10 Hz cadence, and the same
// events arriving with delivery jitter.
const UPDATE_PATTERNS: [string, number[]][] = [
  ["144 Hz frames", [1000 / 144]],
  ["120 Hz frames", [1000 / 120]],
  ["60 Hz frames", [1000 / 60]],
  ["10 Hz events", [100]],
  ["jittered events 80/120 ms", [80, 120]],
  ["jittered events 60/140 ms", [60, 140]],
  ["bunched events 199/1 ms", [199, 1]],
];

test("the needle follows the same curve at every update pattern", () => {
  const peak = -20;
  const end = PEAK_HOLD_MS + ((peak - FLOOR_DB) / PEAK_DECAY_DB_PER_S) * 1000 + 500;
  for (const [name, gaps] of UPDATE_PATTERNS) {
    const n = newNeedle();
    advanceNeedle(n, 0);
    raisePeak(n, peak, 0);
    let t = 0;
    for (let i = 0; t < end; i++) {
      t += at(gaps, i % gaps.length);
      advanceNeedle(n, t);
      const want = curve(peak, t);
      assert.ok(Math.abs(n.db - want) < 1e-9, `${name}: at ${t.toFixed(1)} ms the needle is at ${n.db}, want ${want}`);
    }
    assert.equal(needleSettled(n), true, `${name}: the needle must end on the floor`);
  }
});

test("a long gap drops the needle by the whole gap", () => {
  const n = newNeedle();
  advanceNeedle(n, 0);
  raisePeak(n, -20, 0);
  // A stalled tab, or a meter off the frame loop, updates again 1 s after the
  // hold ended.
  advanceNeedle(n, PEAK_HOLD_MS + 1000);
  assert.equal(n.db, -20 - PEAK_DECAY_DB_PER_S);
  advanceNeedle(n, 60_000);
  assert.equal(n.db, FLOOR_DB, "the fall must stop at the floor");
  assert.equal(needleSettled(n), true);
});

test("a step that straddles the hold falls only for its tail", () => {
  const n = newNeedle();
  advanceNeedle(n, 0);
  raisePeak(n, -20, 0);
  advanceNeedle(n, PEAK_HOLD_MS - 40);
  assert.equal(n.db, -20, "the needle holds until the hold ends");
  advanceNeedle(n, PEAK_HOLD_MS + 10);
  assert.equal(n.db, -20 - (PEAK_DECAY_DB_PER_S * 10) / 1000);
});

test("a reading not after the last one leaves the needle alone", () => {
  const n = newNeedle();
  advanceNeedle(n, 0);
  raisePeak(n, -20, 0);
  advanceNeedle(n, PEAK_HOLD_MS + 100);
  const db = n.db;
  // An animation frame stamped before a level event taken in the same frame.
  advanceNeedle(n, PEAK_HOLD_MS + 97);
  assert.equal(n.db, db);
  // The next reading falls only for the time since the latest one.
  advanceNeedle(n, PEAK_HOLD_MS + 200);
  assert.equal(n.db, curve(-20, PEAK_HOLD_MS + 200));
});

test("a non-finite time leaves the needle alone", () => {
  const n = newNeedle();
  advanceNeedle(n, 0);
  raisePeak(n, -20, 0);
  advanceNeedle(n, Number.NaN);
  advanceNeedle(n, Number.POSITIVE_INFINITY);
  assert.equal(n.db, -20);
  advanceNeedle(n, PEAK_HOLD_MS + 100);
  assert.equal(n.db, curve(-20, PEAK_HOLD_MS + 100), "a bad reading must not poison later ones");
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

test("the needle settles only on the floor", () => {
  const n = newNeedle();
  assert.equal(needleSettled(n), true, "a fresh needle rests on the floor");
  raisePeak(n, FLOOR_DB + 1, 0);
  assert.equal(needleSettled(n), false);
  n.db = Number.NaN;
  assert.equal(needleSettled(n), true, "a needle that is not above the floor has nowhere to fall");
});

test("clampLevel maps silence and bad readings to the floor", () => {
  assert.equal(clampLevel(-99), FLOOR_DB);
  assert.equal(clampLevel(Number.NEGATIVE_INFINITY), FLOOR_DB);
  assert.equal(clampLevel(Number.NaN), FLOOR_DB);
  assert.equal(clampLevel(-12.5), -12.5);
  assert.equal(clampLevel(0.4), 0.4);
});

test("levelRatio places a level on the scale from the floor to 0 dBFS", () => {
  assert.equal(levelRatio(FLOOR_DB), 0);
  assert.equal(levelRatio(-99), 0);
  assert.equal(levelRatio(Number.NaN), 0);
  assert.equal(levelRatio(FLOOR_DB / 2), 0.5);
  assert.equal(levelRatio(0), 1);
  assert.equal(levelRatio(3), 1);
});

test("formatReadout shows -inf at the floor and one decimal above it", () => {
  assert.equal(formatReadout(FLOOR_DB), "-inf");
  assert.equal(formatReadout(FLOOR_DB + 0.05), "-inf");
  assert.equal(formatReadout(FLOOR_DB + 0.2), "-59.8 dBFS");
  assert.equal(formatReadout(-3.21), "-3.2 dBFS");
});

// FakeFrames is a frame source a test steps by hand.
class FakeFrames implements FramePorts {
  private next = 1;
  readonly pending = new Map<number, (now: number) => void>();
  requests = 0;
  // The scheduler's timed wakes, fired by hand.
  readonly timers = new FakeTimers();

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
    // endHold fires the timed wake a holding needle set for the end of its
    // hold, as the timer would.
    endHold: () => {
      const [wake] = f.timers.pending();
      assert.ok(wake, "no timed wake set for the end of the hold");
      f.timers.fire(wake);
    },
  };
}

test("the first frame draws the empty track", () => {
  const h = meterHarness();
  assert.equal(h.frames.running(), true);
  h.step();
  assert.deepEqual(h.draws, [[FLOOR_DB, FLOOR_DB]]);
  assert.deepEqual(h.readouts, [WAITING_READOUT], "a meter with no level yet must not read as silence");
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
  // The first level leaves the waiting readout, which takes one frame.
  h.setClock(0);
  h.meter.setLevels(-99, -99);
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
  // Later events leave the latch shown without repainting it; with nothing
  // else changed while the needle holds, they ask for no frame at all.
  h.setClock(20);
  h.meter.setLevels(-20, -20);
  assert.equal(h.frames.running(), false);
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

test("levels and frames both advance the needle, and an earlier frame time does not", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  h.endHold();
  // Frames glide the needle by real elapsed time.
  h.f.step(PEAK_HOLD_MS + 50);
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, curve(-20, PEAK_HOLD_MS + 50)]);
  // A level event during the fall brings it up to date at its own time, and
  // the frame that follows is stamped a little earlier: that frame must not
  // move the needle, and the next one falls only from the level event's time.
  h.setClock(PEAK_HOLD_MS + 100);
  h.meter.setLevels(-30, -99);
  h.f.step(PEAK_HOLD_MS + 97);
  assert.deepEqual(h.draws.at(-1), [-30, curve(-20, PEAK_HOLD_MS + 100)]);
  h.f.step(PEAK_HOLD_MS + 116);
  assert.deepEqual(h.draws.at(-1), [-30, curve(-20, PEAK_HOLD_MS + 116)]);
});

test("a meter off the frame loop does not lose a quieter peak to a stale needle", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  // Another view shows: the loop is suspended while levels keep arriving.
  h.frames.setSuspended(true);
  h.setClock(2000);
  // By now the -20 peak has fallen to about -57.5 dB, so a -40 peak is louder
  // and must raise the needle; a needle not brought up to date first would
  // still read -20 and ignore it.
  h.meter.setLevels(-99, -40);
  h.frames.setSuspended(false);
  h.step();
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, -40]);
});

test("the readout updates at most every 100 ms while the needle falls", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  h.endHold();
  const start = h.readouts.length;
  // Glide the needle down at 60 Hz for one second past the hold.
  const writes: number[] = [];
  for (let t = PEAK_HOLD_MS; t <= PEAK_HOLD_MS + 1000; t += 1000 / 60) {
    const before = h.readouts.length;
    h.f.step(t);
    if (h.readouts.length > before) writes.push(t);
  }
  assert.ok(h.readouts.length - start >= 9, `the falling readout must keep updating, got ${h.readouts.length - start} writes`);
  for (const [i, t] of writes.entries()) {
    if (i === 0) continue;
    const gap = t - at(writes, i - 1);
    assert.ok(gap >= READOUT_INTERVAL_MS - 1e-6, `readout written ${gap.toFixed(1)} ms after the last, want at least ${READOUT_INTERVAL_MS}`);
  }
});

test("the settled readout is always written", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, FLOOR_DB + 1);
  h.step();
  h.endHold();
  // Frames 16 ms apart: the needle reaches the floor well inside a throttle
  // window after the last write.
  let t = PEAK_HOLD_MS;
  while (h.frames.running()) {
    t += 16;
    h.f.step(t);
  }
  assert.equal(h.readouts.at(-1), "-inf", "the final value must be shown even inside the throttle window");
});

test("a new peak during the throttle window shows at once", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -40);
  h.step();
  assert.equal(h.readouts.at(-1), "-40.0 dBFS");
  // 20 ms later, well inside the window, a louder peak arrives.
  h.setClock(20);
  h.meter.setLevels(-99, -10);
  h.step();
  assert.equal(h.readouts.at(-1), "-10.0 dBFS");
});

test("with reduced motion every readout change is written at once", () => {
  const h = meterHarness({ reducedMotion: true });
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  // Level events 40 ms apart, well inside the throttle window, during the fall.
  let t = PEAK_HOLD_MS;
  for (let i = 0; i < 5; i++) {
    t += 40;
    h.setClock(t);
    h.meter.setLevels(-99, -99);
    if (h.frames.running()) h.step();
    assert.equal(h.readouts.at(-1), formatReadout(curve(-20, t)), `event at ${t} ms must show its value`);
  }
});

test("switching reduced motion off wakes a falling needle and it glides", () => {
  const h = meterHarness({ reducedMotion: true });
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  assert.equal(h.frames.running(), false, "reduced motion asks for no more frames");
  h.setClock(PEAK_HOLD_MS + 100);
  h.meter.setReducedMotion(false);
  assert.equal(h.frames.running(), true, "turning animation back on must wake the meter");
  h.step();
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, curve(-20, PEAK_HOLD_MS + 100)]);
  assert.equal(h.frames.running(), true, "a needle above the floor keeps gliding");
  // And back: the glide stops at the next frame.
  h.meter.setReducedMotion(true);
  h.setClock(PEAK_HOLD_MS + 116);
  h.step();
  assert.equal(h.frames.running(), false);
});

test("clearing levels drops the bar and the needle and keeps the latch", () => {
  // Reduced motion: the meter asks for no frames of its own, so only the
  // clear can bring one.
  const h = meterHarness({ reducedMotion: true });
  h.step();
  h.setClock(0);
  h.meter.setLevels(-20, -10, true);
  h.step();
  assert.equal(h.frames.running(), false);
  h.meter.clearLevels();
  assert.equal(h.frames.running(), true, "the cleared meter must be drawn");
  h.step();
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, FLOOR_DB], "the bar and the needle clear");
  assert.equal(h.readouts.at(-1), WAITING_READOUT);
  assert.deepEqual(h.clips, [false, true], "the clip latch stays until the operator clears it");
});

test("a holding needle requests no frames until the hold ends", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  assert.equal(h.frames.running(), false, "nothing moves during the hold, so no frames");
  const [wake] = h.f.timers.pending(PEAK_HOLD_MS);
  assert.ok(wake, "the meter must wake itself when the hold ends");
  // A silent level during the hold changes nothing and asks for no frame.
  h.setClock(300);
  h.meter.setLevels(-99, -99);
  assert.equal(h.frames.running(), false);
  h.f.timers.fire(wake);
  assert.equal(h.frames.running(), true, "the glide starts when the hold ends");
  h.f.step(PEAK_HOLD_MS + 100);
  assert.deepEqual(h.draws.at(-1), [FLOOR_DB, curve(-20, PEAK_HOLD_MS + 100)]);
  assert.equal(h.frames.running(), true, "the glide keeps its frames");
});

test("a louder peak during the hold moves the timed wake to the new hold's end", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  h.setClock(400);
  h.meter.setLevels(-99, -10);
  h.step();
  const pending = h.f.timers.pending();
  assert.equal(pending.length, 1, "one timed wake per meter");
  assert.equal(at(pending, 0).ms, PEAK_HOLD_MS, "the new hold runs from the new peak");
});

test("removing an animator cancels its timed wake", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  assert.equal(h.f.timers.pending().length, 1);
  h.meter.pause();
  assert.equal(h.f.timers.pending().length, 0, "a hidden meter must not wake itself");
});

test("with reduced motion a silent level on a still needle asks for no frame", () => {
  const h = meterHarness({ reducedMotion: true });
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  for (let t = 100; t < PEAK_HOLD_MS; t += 100) {
    h.setClock(t);
    h.meter.setLevels(-99, -99);
    assert.equal(h.frames.running(), false, `a silent event at ${t} ms during the hold must not wake the loop`);
  }
});

test("a readout change inside the throttle window is shown when a hold starts", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -10);
  h.step();
  h.endHold();
  // Glide until a falling value has just been written.
  let t = PEAK_HOLD_MS;
  let writes = h.readouts.length;
  while (h.readouts.length === writes) {
    t += 16;
    h.f.step(t);
  }
  writes = h.readouts.length;
  const shown = at(h.readouts, writes - 1);
  // 16 ms later, inside the throttle window, a peak just under the shown
  // value starts a new hold: the needle stops there, and so must the text.
  const peak = Number.parseFloat(shown) - 0.2;
  h.setClock(t + 16);
  h.meter.setLevels(-99, peak);
  h.f.step(t + 16);
  assert.equal(h.readouts.at(-1), formatReadout(peak), "the held value must show at once, not after the hold");
});

test("clearing levels cancels a timed wake for the end of a hold", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-99, -20);
  h.step();
  assert.equal(h.f.timers.pending().length, 1);
  h.meter.clearLevels();
  assert.equal(h.f.timers.pending().length, 0, "the cleared needle has no hold left to wait for");
});

test("a silent channel leaves the waiting readout at its first level", () => {
  const h = meterHarness();
  h.step();
  assert.equal(h.readouts.at(-1), WAITING_READOUT);
  h.setClock(0);
  h.meter.setLevels(-99, -99);
  assert.equal(h.frames.running(), true, "the first level must bring a frame even at the floor");
  h.step();
  assert.equal(h.readouts.at(-1), "-inf");
  h.meter.clearLevels();
  h.step();
  assert.equal(h.readouts.at(-1), WAITING_READOUT);
  h.setClock(5_000);
  h.meter.setLevels(-99, -99);
  h.step();
  assert.equal(h.readouts.at(-1), "-inf", "silence after a clear must replace the waiting readout");
});

test("a cleared meter shows the waiting readout until the next level", () => {
  const h = meterHarness();
  h.step();
  h.setClock(0);
  h.meter.setLevels(-20, -10);
  h.step();
  h.meter.clearLevels();
  h.step();
  assert.equal(h.readouts.at(-1), WAITING_READOUT, "no levels must not read as silence");
  h.setClock(5_000);
  h.meter.setLevels(-30, -25);
  h.step();
  assert.equal(h.readouts.at(-1), "-25.0 dBFS");
});

