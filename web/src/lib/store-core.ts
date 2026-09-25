// Pure helpers behind the app store's polled reads. No DOM, timers, or network
// of their own: the fetch and the side effects are passed in, so the ordering
// and change-detection rules are unit tested on their own.

import type { LatestGate } from "./latest-core.js";

// gatedRefresh runs one gated read of a polled resource: it takes a gate token,
// awaits fetch, and calls apply only when the gate accepts the token (no newer
// response has been applied meanwhile). It resolves true when fresh data is in
// place: this response was applied, or it was dropped because a newer one had
// already been applied. A failure calls onError and resolves true only when a
// newer response is already applied (fresher data is in place, so the caller
// must not raise a load error for it); otherwise false. It rejects only if
// onError itself throws.
//
// fetch runs before accept, so any validation or normalization that can throw
// belongs in fetch: a response that throws never marks its token applied and
// so never drops an older valid response still in flight.
export async function gatedRefresh<T>(
  gate: LatestGate,
  fetch: () => Promise<T>,
  apply: (value: T) => void,
  onError?: (err: unknown) => void,
): Promise<boolean> {
  const token = gate.begin();
  try {
    const value = await fetch();
    if (!gate.accept(token)) return true;
    apply(value);
    return true;
  } catch (err) {
    onError?.(err);
    return gate.superseded(token);
  }
}

// ChangeTracker reports whether an applied value differs from the last one it
// saw, so the store announces a resource only when its data changed rather than
// on every poll tick. Values are compared by their JSON encoding: every tracked
// value is a parsed API response, and Go's encoding/json writes struct fields in
// declaration order and map keys sorted, so equal data encodes to the same
// string. A spurious difference would only cost one extra announcement.
export class ChangeTracker {
  private last = "";
  private seen = false;

  // changed records value and reports whether it differs from the previous one.
  // The first value after construction or reset always counts as changed, so
  // the initial load always announces (even an empty list).
  changed(value: unknown): boolean {
    const next = JSON.stringify(value) ?? "";
    if (this.seen && next === this.last) return false;
    this.seen = true;
    this.last = next;
    return true;
  }

  // reset forgets the last value, so the next one announces. The store calls it
  // after a failed read, because a view may have replaced its content with a
  // load error that only the next announcement clears, even when the data that
  // comes back matches what was shown before the failure.
  reset(): void {
    this.seen = false;
    this.last = "";
  }
}
