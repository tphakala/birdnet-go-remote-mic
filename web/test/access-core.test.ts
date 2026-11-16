// Unit tests for the pure Access Token card logic. Run with Node's built-in
// test runner (see the web:test task): no browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import { tokenKeydown } from "../src/lib/access-core.ts";

// press drives tokenKeydown with one keydown and counts what it did.
function press(key: string, isComposing: boolean, keyCode: number): { prevented: number; saved: number } {
  let prevented = 0;
  let saved = 0;
  tokenKeydown({ key, isComposing, keyCode, preventDefault: () => prevented++ }, () => saved++);
  return { prevented, saved };
}

test("tokenKeydown saves once on a plain Enter and leaves an IME's confirming Enter alone", () => {
  const cases: Array<{ name: string; key: string; isComposing: boolean; keyCode: number; want: { prevented: number; saved: number } }> = [
    { name: "plain Enter", key: "Enter", isComposing: false, keyCode: 13, want: { prevented: 1, saved: 1 } },
    // Chrome: the confirming Enter keydown reports isComposing.
    { name: "Chrome confirming Enter", key: "Enter", isComposing: true, keyCode: 229, want: { prevented: 0, saved: 0 } },
    // Some IMEs (Firefox on Linux) report the real keyCode while composing,
    // so only isComposing tells.
    { name: "composing Enter with keyCode 13", key: "Enter", isComposing: true, keyCode: 13, want: { prevented: 0, saved: 0 } },
    // Safari: compositionend has already fired, so only keyCode 229 tells.
    { name: "Safari confirming Enter", key: "Enter", isComposing: false, keyCode: 229, want: { prevented: 0, saved: 0 } },
    // Firefox: keys during composition report key "Process".
    { name: "Firefox composition key", key: "Process", isComposing: true, keyCode: 229, want: { prevented: 0, saved: 0 } },
    { name: "a letter", key: "a", isComposing: false, keyCode: 65, want: { prevented: 0, saved: 0 } },
  ];
  for (const c of cases) {
    assert.deepEqual(press(c.key, c.isComposing, c.keyCode), c.want, c.name);
  }
});
