// Unit tests for withDeadline in lib/deadline.ts. Run with node:test (see
// web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { withDeadline } from "../src/lib/deadline.ts";
import { at, FakeTimers } from "./fixtures.ts";

test("withDeadline arms its deadline, leaves a request that beats it alone, and clears the timer", async () => {
  const timers = new FakeTimers();
  let seen: AbortSignal | undefined;
  const result = await withDeadline(
    4_000,
    async (signal) => {
      seen = signal;
      assert.equal(timers.pending(4_000).length, 1, "the deadline is armed for ms");
      return "ok";
    },
    timers,
  );
  assert.equal(result, "ok");
  assert.equal(seen?.aborted, false, "a request that beats the deadline is not aborted");
  assert.deepEqual(timers.pending(), [], "the timer is cleared once the request ends");
});

test("withDeadline aborts a request when its deadline passes", async () => {
  const timers = new FakeTimers();
  // The request ends only when its signal aborts, as fetch does.
  const pending = withDeadline(
    4_000,
    (signal) =>
      new Promise<string>((_resolve, reject) => {
        signal.addEventListener("abort", () => reject(new Error("aborted")));
      }),
    timers,
  );
  timers.fire(at(timers.pending(4_000), 0));
  let caught: unknown = null;
  try {
    await pending;
  } catch (err) {
    caught = err;
  }
  assert.ok(caught instanceof Error && caught.message === "aborted", "the deadline must abort the request");
});
