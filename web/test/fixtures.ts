// Shared test fixtures. This file holds no tests of its own.

import type { Timers } from "../src/lib/store.js";
import type { Notification } from "../src/lib/types.js";

// FakeTimer is one timer a FakeTimers has handed out.
export interface FakeTimer {
  fn: () => void;
  ms: number;
  repeat: boolean;
  cleared: boolean;
}

// FakeTimers implements the stores' Timers seam without a clock: a test lists
// what is scheduled and fires it by hand, so a 60 s grace or a backoff runs at
// once and a cancelled timer is visible as cleared.
export class FakeTimers implements Timers {
  readonly all: FakeTimer[] = [];

  // pending lists the live one-shot timers, optionally only those of ms.
  pending(ms?: number): FakeTimer[] {
    return this.all.filter((t) => !t.repeat && !t.cleared && (ms === undefined || t.ms === ms));
  }

  // intervals lists the live repeating timers.
  intervals(): FakeTimer[] {
    return this.all.filter((t) => t.repeat && !t.cleared);
  }

  // fire runs a one-shot timer once, as its deadline passing would.
  fire(t: FakeTimer): void {
    if (t.cleared) throw new Error("fired a cleared timer");
    t.cleared = true;
    t.fn();
  }

  private add(fn: () => void, ms: number, repeat: boolean): ReturnType<typeof setTimeout> {
    this.all.push({ fn, ms, repeat, cleared: false });
    // The handle is the timer's index, cast to the handle type the seam uses.
    return (this.all.length - 1) as unknown as ReturnType<typeof setTimeout>;
  }

  private clear(handle: ReturnType<typeof setTimeout>): void {
    const t = this.all[handle as unknown as number];
    if (t) t.cleared = true;
  }

  setTimeout(fn: () => void, ms: number): ReturnType<typeof setTimeout> {
    return this.add(fn, ms, false);
  }

  clearTimeout(handle: ReturnType<typeof setTimeout>): void {
    this.clear(handle);
  }

  setInterval(fn: () => void, ms: number): ReturnType<typeof setInterval> {
    return this.add(fn, ms, true);
  }

  clearInterval(handle: ReturnType<typeof setInterval>): void {
    this.clear(handle);
  }
}

// notif builds a well-formed notification with every field defaulted, so a test
// sets only what it is about.
export function notif(over: Partial<Notification> & { id: number }): Notification {
  return {
    id: over.id,
    bootId: over.bootId ?? "boot-a",
    time: over.time ?? "2026-09-12T14:00:00Z",
    uptimeMs: over.uptimeMs ?? 0,
    severity: over.severity ?? "info",
    category: over.category ?? "system",
    kind: over.kind ?? "event",
    key: over.key,
    source: over.source,
    title: over.title ?? "Title",
    message: over.message ?? "Message",
  };
}
