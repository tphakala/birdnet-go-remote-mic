// Unit tests for withDeadline in lib/deadline.ts. Run with node:test (see
// web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { withDeadline } from "../src/lib/deadline.ts";

test("withDeadline returns a result that beats the deadline", async () => {
  assert.equal(await withDeadline(1_000, async () => "ok"), "ok");
});

test("withDeadline aborts a request that outlives it", async () => {
  // The request ends only when its signal aborts, as fetch does.
  const run = (signal: AbortSignal) =>
    new Promise<string>((_resolve, reject) => {
      signal.addEventListener("abort", () => reject(new Error("aborted")));
    });
  let caught: unknown = null;
  try {
    await withDeadline(0, run);
  } catch (err) {
    caught = err;
  }
  assert.ok(caught instanceof Error && caught.message === "aborted", "the deadline must abort the request");
});
