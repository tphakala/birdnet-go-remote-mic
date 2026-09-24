// LatestGate orders overlapping reads of one resource so the newest APPLIED
// response wins. It is pure (no DOM, timers, or network) so the ordering rules
// are unit tested on their own.
//
// A read calls begin() before it starts and accept() with the returned token
// when its response arrives. accept() refuses a token older than the last one
// applied, so a slow older body never overwrites a newer one, but it does NOT
// refuse a token merely because a newer read has started: on a link slower than
// the poll interval every read is overtaken by the next tick before it lands,
// and dropping each one would starve the view forever.
export class LatestGate {
  private started = 0;
  private applied = 0;

  // begin issues the token for a read that is about to start.
  begin(): number {
    return ++this.started;
  }

  // accept reports whether the response for token may be applied, and records
  // it as the newest applied one when it may. The caller applies the body only
  // on true.
  accept(token: number): boolean {
    if (token < this.applied) return false;
    this.applied = token;
    return true;
  }

  // superseded reports whether a response newer than token has already been
  // applied. A failed read uses it to tell "no data" from "fresher data is
  // already in place".
  superseded(token: number): boolean {
    return token < this.applied;
  }

  // invalidate marks every read started so far as stale, for an authoritative
  // write (a PATCH response) that must not be overwritten by a GET that was
  // already in flight. Reads started after it are accepted as usual.
  invalidate(): void {
    this.applied = ++this.started;
  }
}
