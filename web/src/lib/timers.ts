// Timers is the timer API code schedules with when a test must control it:
// the globals in the app, a fake a test fires by hand. A leaf module, so a
// DOM-free core can depend on it without the store.
export interface Timers {
  setTimeout(fn: () => void, ms: number): ReturnType<typeof setTimeout>;
  clearTimeout(handle: ReturnType<typeof setTimeout>): void;
  setInterval(fn: () => void, ms: number): ReturnType<typeof setInterval>;
  clearInterval(handle: ReturnType<typeof setInterval>): void;
}

// OneShotTimers is the part of Timers for code that only sets timeouts.
export type OneShotTimers = Pick<Timers, "setTimeout" | "clearTimeout">;
