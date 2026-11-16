import assert from "node:assert/strict";
import { test } from "node:test";
import { CONFIRM_VALUE, HeldDialog, TransientDialogs, type DialogPort } from "../src/lib/modal-core.ts";

type Listener = (ev: { preventDefault(): void }) => void;

// FakeDialog stands in for HTMLDialogElement: close() takes effect at once and
// queues the close event, which flush() delivers, as the browser fires it in a
// later task.
class FakeDialog implements DialogPort {
  open = false;
  returnValue = "";
  shows = 0;
  private readonly listeners = new Map<string, Listener[]>();
  private pending = 0;

  showModal(): void {
    if (this.open) throw new Error("showModal on an open dialog");
    this.open = true;
    this.shows++;
  }
  close(returnValue?: string): void {
    if (!this.open) return;
    this.open = false;
    if (returnValue !== undefined) this.returnValue = returnValue;
    this.pending++;
  }
  addEventListener(type: "cancel" | "close", listener: Listener): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  flush(): void {
    for (; this.pending > 0; this.pending--) for (const l of this.listeners.get("close") ?? []) l({ preventDefault() {} });
  }
  // escape is a close request: it closes unless a cancel listener refuses, or
  // always when forced (a browser ignoring preventDefault).
  escape(forced = false): void {
    let prevented = false;
    for (const l of this.listeners.get("cancel") ?? []) l({ preventDefault: () => { prevented = true; } });
    if (forced || !prevented) this.close();
  }
}

test("ask resolves true only for the confirming button", async () => {
  for (const [how, want] of [
    [(d: FakeDialog) => d.close(CONFIRM_VALUE), true],
    [(d: FakeDialog) => d.close("cancel"), false],
    [(d: FakeDialog) => d.escape(), false],
  ] as const) {
    const d = new FakeDialog();
    let disposed = 0;
    const p = new TransientDialogs().ask(d, () => disposed++);
    assert.equal(d.open, true);
    how(d);
    d.flush();
    assert.equal(await p, want);
    assert.equal(disposed, 1);
    assert.equal(d.returnValue, "");
  }
});

test("closeAll cancels every open dialog and leaves a confirmed one confirmed", async () => {
  const stack = new TransientDialogs();
  const a = new FakeDialog();
  const b = new FakeDialog();
  const pa = stack.ask(a);
  const pb = stack.ask(b);
  b.close(CONFIRM_VALUE);
  stack.closeAll();
  stack.closeAll();
  a.flush();
  b.flush();
  assert.deepEqual([await pa, await pb], [false, true]);
  stack.closeAll();
  assert.equal(a.shows + b.shows, 2);
});

test("HeldDialog stays open until hide", () => {
  const d = new FakeDialog();
  const held = new HeldDialog(d);
  held.show();
  held.show();
  assert.equal(d.shows, 1);
  d.escape();
  assert.equal(d.open, true, "a refused close request");
  d.escape(true);
  d.flush();
  assert.equal(d.open, true, "reopened after a forced close");
  d.escape(true);
  held.show();
  d.flush();
  assert.equal(d.shows, 3);
  held.hide();
  d.flush();
  assert.equal(d.open, false);
  held.hide();
  held.show();
  held.hide();
  held.show();
  d.flush();
  assert.equal(d.open, true);
  assert.equal(held.shown, true);
});
