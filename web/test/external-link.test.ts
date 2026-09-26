// Unit tests for externalTailStart (lib/ui.ts): which end of an external
// link's text stays on one line with its new-tab icon. Run with node:test over
// the compiled output (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { externalTailStart } from "../src/lib/ui.js";

const tail = (text: string): string => text.slice(externalTailStart(text));

test("a short last word stays whole with the icon", () => {
  assert.equal(tail("Read it on GitHub"), "GitHub");
  assert.equal(tail("Tomi P. Hakala"), "Hakala");
  assert.equal(tail("Bug"), "Bug");
});

test("a long word keeps only its end with the icon, so the rest can wrap", () => {
  assert.equal(tail("github.com/tphakala/birdnet-go-remote-mic"), "te-mic");
  assert.equal(tail("see github.com/tphakala/birdnet-go"), "net-go");
  // Exactly the longest whole word still stays whole.
  assert.equal(tail("x abcdefghijkl"), "abcdefghijkl");
});
