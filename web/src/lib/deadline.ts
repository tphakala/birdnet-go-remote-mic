// DeadlineTimers is the timer API withDeadline schedules with.
export interface DeadlineTimers {
  setTimeout(fn: () => void, ms: number): ReturnType<typeof setTimeout>;
  clearTimeout(handle: ReturnType<typeof setTimeout>): void;
}

// withDeadline runs a request under an abort signal that fires after ms, so a
// stalled request (headers or body) ends in an error rather than hanging. A
// timer and an AbortController rather than AbortSignal.timeout, which is
// missing before Safari 16. timers is the global timers in the app, a fake
// in tests.
export async function withDeadline<T>(
  ms: number,
  run: (signal: AbortSignal) => Promise<T>,
  timers: DeadlineTimers = globalThis,
): Promise<T> {
  const abort = new AbortController();
  const timer = timers.setTimeout(() => abort.abort(), ms);
  try {
    return await run(abort.signal);
  } finally {
    timers.clearTimeout(timer);
  }
}
