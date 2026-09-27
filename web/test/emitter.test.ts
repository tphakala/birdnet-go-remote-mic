// Unit tests for lib/emitter.ts: listeners get the payload emit sent, in
// subscription order, and only for their own event name. Assertions stay out
// of the listeners: a throw inside one does not reach the test. The expected
// compile errors in typeErrors pin the typing: web:typecheck fails if a
// misspelled name or a wrong payload type stops being one. Run with node:test
// (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { Emitter } from "../src/lib/emitter.ts";

interface Events {
  count: number;
  ping: undefined;
}

class Source extends Emitter<Events> {
  count(n: number): void {
    this.emit("count", n);
  }
  ping(): void {
    this.emit("ping");
  }
  typeErrors(): void {
    // @ts-expect-error: the payload must match the event map.
    this.emit("count", "3");
    // @ts-expect-error: an event with no payload takes none.
    this.emit("ping", 1);
    // @ts-expect-error: a payload is required.
    this.emit("count");
    const name: "count" | "ping" = Math.random() < 0.5 ? "count" : "ping";
    // @ts-expect-error: a union-typed name still takes its own event's payload
    // (a number fits "count", but "ping" takes none).
    this.emit(name, 3);
    // @ts-expect-error: "cuont" is not in the event map.
    this.on("cuont", () => {});
    // @ts-expect-error: the listener's parameter type must match the payload.
    this.on("count", (s: string) => s);
  }
}

test("on receives each emitted payload, in order, per listener", () => {
  const src = new Source();
  const got: string[] = [];
  src.on("count", (n) => got.push(`a${n}`));
  src.on("count", (n) => got.push(`b${n}`));
  src.count(1);
  src.count(2);
  assert.deepEqual(got, ["a1", "b1", "a2", "b2"]);
});

test("a listener hears only its own event name", () => {
  const src = new Source();
  const counts: number[] = [];
  const pings: unknown[] = [];
  src.on("count", (n) => counts.push(n));
  src.on("ping", (payload) => pings.push(payload));
  src.ping();
  src.ping();
  src.count(5);
  assert.deepEqual(counts, [5]);
  // A payload-less event delivers undefined, as its event map entry says.
  assert.deepEqual(pings, [undefined, undefined]);
});
